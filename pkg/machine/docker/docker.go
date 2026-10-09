// Package docker is a driver that runs Linux guests as Docker containers that
// look like machines: systemd boots as the guest's init, and the guest's root
// has every capability, but only over the guest. On the host it is an
// unprivileged uid.
//
// Docker offers a user namespace only for a whole daemon (userns-remap), not
// per container, so the driver makes one itself without --privileged or any
// added capability. The container's PID 1 is `disco-vm docker-init` (Init),
// which starts systemd in new user, mount, pid, cgroup, uts, and ipc
// namespaces, mapping guest uids 0-65535 to UIDBase and up on the host. Four
// of Docker's protections are relaxed for it, and nothing is added:
//
//   - seccomp=unconfined: the default profile refuses to create namespaces
//     without CAP_SYS_ADMIN.
//   - apparmor=unconfined: docker-default refuses mount, and the guest mounts
//     /proc, /run, its terminals, and its cgroup.
//   - systempaths=unconfined: the kernel mounts a new /proc only where the
//     existing one is not partly hidden.
//   - writable-cgroups=true (with a private cgroup namespace): Init hands the
//     container's cgroup subtree to the guest's root, for systemd.
//
// The guest's files must belong to its uids, so Install shifts the base image
// once (Shift), and commits it as the base layer. A layer is a Docker image,
// committed from a stopped container, and every layer after the base is
// written by the guest, in its uids already.
//
// The daemon owns a running guest, so the driver is remote: the engine runs no
// shim, and every command attaches to the container (Attach).
//
// The agent listens on a root-only unix socket in the guest. Dial runs
// `disco-vm pipe` through docker exec, on the container's side of the user
// namespace, and reaches the socket through the guest's root directory, which
// Init links at relayDir.
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

func init() {
	machine.Register("docker", func() (machine.Driver, error) { return New(), nil })
}

const (
	// UIDBase is where the guest's uids start on the host: guest uid N is
	// host uid UIDBase+N. It is baked into every layer, so it is fixed. It is
	// far above the ranges shadow-utils hands out in /etc/subuid, which start
	// at 100000, so a guest's files never share an owner with a rootless
	// container's.
	UIDBase = 1 << 24
	// UIDCount is how many uids the guest has.
	UIDCount = 65536

	// agentPath and agentSocket are where the agent is in the guest.
	agentPath   = "/usr/local/libexec/disco-vm"
	agentSocket = "/run/disco-vm/agent.sock"
	agentUnit   = "disco-vm-guest.service"
	// relayDir is a tmpfs only the container's root can open, under the
	// guest's own /run, so the guest never sees it. Init links the guest's
	// root directory there.
	relayDir = "/run/disco-vm-relay"

	// defaultImage is the image installed when options.image is not given.
	defaultImage = "debian:trixie"
	// layerRepo names the images the driver commits.
	layerRepo = "disco-vm-layer"

	layerFile     = "layer.json"
	containerFile = "container.json"

	labelInstance = "ai.discobox.vm.instance"
	labelLayer    = "ai.discobox.vm.layer"
)

// Driver is the docker driver.
type Driver struct {
	api *api
}

var (
	_ machine.Driver   = (*Driver)(nil)
	_ machine.Attacher = (*Driver)(nil)
)

// New returns a driver for the daemon $DOCKER_HOST names. It dials nothing
// until it is used.
func New() *Driver { return &Driver{api: newAPI()} }

func (*Driver) Name() string { return "docker" }

func (*Driver) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS:    []machine.OS{machine.Linux},
		CloneModes: []machine.CloneMode{machine.Cold},
		Remote:     true,
	}
}

func (d *Driver) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	in, err := d.api.info(ctx)
	if err != nil {
		return fmt.Errorf("docker: %s: %w", d.api.host, err)
	}
	if in.OSType != "linux" {
		return fmt.Errorf("docker: the daemon runs %s containers; switch it to linux containers", in.OSType)
	}
	if in.CgroupVersion != "2" {
		return fmt.Errorf("docker: the daemon's host uses cgroup v%s; a guest's systemd needs cgroup v2", in.CgroupVersion)
	}
	for _, opt := range in.SecurityOpts {
		if strings.Contains(opt, "rootless") {
			return errors.New("docker: a rootless daemon cannot map a guest's 65536 uids; use a rootful one")
		}
	}
	return nil
}

// layerRef is a layer's layer.json.
type layerRef struct {
	// Image is the image's ID, and Ref the tag that keeps it from being
	// pruned. DeleteLayer removes the tag, and the image with it.
	Image string `json:"image"`
	Ref   string `json:"ref"`
}

// containerRef is an instance's container.json.
type containerRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Install pulls an image, installs systemd in it if it has none, installs the
// agent as a systemd service, shifts its files into the guest's uids, and
// commits it as the base layer.
func (d *Driver) Install(ctx context.Context, spec machine.InstallSpec, dst machine.Layer) error {
	if spec.GuestOS != machine.Linux {
		return fmt.Errorf("docker: guests are linux, not %s", spec.GuestOS)
	}
	if spec.Media != "" && spec.Media != "latest" {
		return fmt.Errorf("docker: install from media \"latest\"; pick an image with options.image, not media %q", spec.Media)
	}
	for key := range spec.Options {
		if key != "image" {
			return fmt.Errorf("docker: unknown install option %q (have: image)", key)
		}
	}
	image := spec.Options["image"]
	if image == "" {
		image = defaultImage
	}
	log := logger(spec.Log)
	in, err := d.api.info(ctx)
	if err != nil {
		return err
	}
	if err := checkArch(spec.Agent, in.Architecture); err != nil {
		return err
	}
	if _, err := d.api.inspectImage(ctx, image); isNotFound(err) {
		log("docker: pulling %s", image)
		if err := d.api.pull(ctx, image, log); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	log("docker: installing systemd and the agent in %s", image)
	id, err := d.api.createContainer(ctx, containerName("install"), containerConfig{
		Image:      image,
		Entrypoint: []string{"/bin/sh", "-c", installScript},
		Cmd:        []string{},
		User:       "0",
		Labels:     map[string]string{labelLayer: dst.ID},
	})
	if err != nil {
		return fmt.Errorf("docker: create install container: %w", err)
	}
	defer d.removeContainer(id)
	agent, err := agentTar(spec.Agent)
	if err != nil {
		return err
	}
	if err := d.api.putArchive(ctx, id, "/", agent); err != nil {
		return fmt.Errorf("docker: copy the agent in: %w", err)
	}
	if err := d.api.start(ctx, id); err != nil {
		return fmt.Errorf("docker: start install container: %w", err)
	}
	code, err := d.api.wait(ctx, id)
	if err != nil {
		return err
	}
	if out, _ := d.api.logs(context.Background(), id); out != "" {
		log("%s", strings.TrimRight(out, "\n"))
	}
	if code != 0 {
		return fmt.Errorf("docker: install in %s exited %d", image, code)
	}

	log("docker: committing the base layer")
	return d.commit(ctx, id, dst, []string{
		`ENTRYPOINT ["` + agentPath + `", "docker-init"]`,
		`CMD []`,
		`USER 0`,
		`WORKDIR /`,
		`STOPSIGNAL SIGTERM`,
	})
}

// installScript runs as the install container's root, before the shift, so
// that a package manager works as it always does.
var installScript = `set -e
if [ ! -x /lib/systemd/systemd ] && [ ! -x /usr/lib/systemd/systemd ]; then
  echo "installing systemd"
  if command -v apt-get >/dev/null; then
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends systemd systemd-sysv dbus >/dev/null
    rm -rf /var/lib/apt/lists/*
  elif command -v dnf >/dev/null; then
    dnf install -y -q systemd dbus && dnf clean all
  elif command -v zypper >/dev/null; then
    zypper -n -q install systemd dbus-1 && zypper clean -a
  else
    echo "the image has no systemd and no package manager disco-vm knows (apt-get, dnf, zypper)" >&2
    exit 1
  fi
fi
units=/etc/systemd/system
mkdir -p $units/multi-user.target.wants
cat > $units/` + agentUnit + ` <<'UNIT'
[Unit]
Description=disco-vm guest agent
After=local-fs.target

[Service]
ExecStart=` + agentPath + ` guest --listen unix:` + agentSocket + `
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
UNIT
ln -sf ../` + agentUnit + ` $units/multi-user.target.wants/` + agentUnit + `
# A container has no kernel of its own to configure, and no consoles.
for unit in sys-kernel-config.mount sys-kernel-debug.mount sys-kernel-tracing.mount systemd-modules-load.service \
    systemd-firstboot.service getty@tty1.service serial-getty@ttyS0.service; do
  ln -sf /dev/null $units/$unit
done
for target in /lib/systemd/system/multi-user.target /usr/lib/systemd/system/multi-user.target; do
  if [ -e $target ]; then ln -sf $target $units/default.target; break; fi
done
# Docker's images keep packages from starting services; a machine's do not.
rm -f /usr/sbin/policy-rc.d
# Every clone makes its own. D-Bus keeps a copy, which systemd would take for
# an empty /etc/machine-id, so it is made a link to it, as Debian does.
: > /etc/machine-id
if [ -d /var/lib/dbus ]; then ln -sf /etc/machine-id /var/lib/dbus/machine-id; fi
exec ` + agentPath + ` docker-init --shift
`

// agentTar is the agent as a tar stream to extract at /.
func agentTar(agent string) (io.Reader, error) {
	data, err := os.ReadFile(agent)
	if err != nil {
		return nil, fmt.Errorf("docker: agent: %w", err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dir := strings.TrimPrefix(filepath.Dir(agentPath), "/")
	for _, d := range []string{"usr/", "usr/local/", dir + "/"} {
		if err := tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			return nil, err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(agentPath, "/"), Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(data))}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(data); err != nil {
		return nil, err
	}
	return &buf, tw.Close()
}

// commit commits a stopped container as the layer dst.
func (d *Driver) commit(ctx context.Context, container string, dst machine.Layer, changes []string) error {
	tag := label(dst.ID, 24) + "-" + fsutil.RandomHex(3)
	image, err := d.api.commit(ctx, container, layerRepo, tag, changes)
	if err != nil {
		return fmt.Errorf("docker: commit: %w", err)
	}
	ref := layerRef{Image: image, Ref: layerRepo + ":" + tag}
	if err := fsutil.WriteJSON(filepath.Join(dst.Dir, layerFile), ref); err != nil {
		_ = d.api.removeImage(context.Background(), ref.Ref)
		return err
	}
	return nil
}

func (d *Driver) Prepare(ctx context.Context, inst machine.InstanceSpec) error {
	if len(inst.Chain) == 0 {
		return errors.New("docker: instance has no image")
	}
	if inst.Mode != "" && inst.Mode != machine.Cold {
		return fmt.Errorf("docker: clone mode %q: %w", inst.Mode, machine.ErrUnsupported)
	}
	var layer layerRef
	if err := fsutil.ReadJSON(filepath.Join(inst.Parent().Dir, layerFile), &layer); err != nil {
		return fmt.Errorf("docker: layer %s: %w", inst.Parent().ID, err)
	}
	if err := os.MkdirAll(inst.Dir, 0o755); err != nil {
		return err
	}
	none := []string{}
	name := containerName(inst.ID)
	id, err := d.api.createContainer(ctx, name, containerConfig{
		Image:    layer.Image,
		Hostname: name,
		Cmd:      []string{},
		Labels:   map[string]string{labelInstance: inst.ID},
		HostConfig: hostConfig{
			SecurityOpt: []string{
				"seccomp=unconfined", "apparmor=unconfined", "label=disable", "writable-cgroups=true",
			},
			CgroupnsMode:  "private",
			MaskedPaths:   &none,
			ReadonlyPaths: &none,
			Tmpfs:         map[string]string{relayDir: "mode=0700"},
		},
	})
	if err != nil {
		return fmt.Errorf("docker: create %s: %w", name, err)
	}
	if err := fsutil.WriteJSON(filepath.Join(inst.Dir, containerFile), containerRef{ID: id, Name: name}); err != nil {
		d.removeContainer(id)
		return err
	}
	return nil
}

func (d *Driver) Boot(ctx context.Context, inst machine.InstanceSpec, opts machine.BootOptions) (machine.Machine, error) {
	var ref containerRef
	if err := fsutil.ReadJSON(filepath.Join(inst.Dir, containerFile), &ref); err != nil {
		return nil, fmt.Errorf("docker: instance %s is not prepared: %w", inst.ID, err)
	}
	if opts.GUI {
		return nil, fmt.Errorf("docker: a window on the guest's display: %w", machine.ErrUnsupported)
	}
	if len(opts.Shares) > 0 {
		return nil, fmt.Errorf("docker: shared directories: %w", machine.ErrUnsupported)
	}
	c, err := d.api.inspectContainer(ctx, ref.ID)
	if err != nil {
		return nil, fmt.Errorf("docker: container %s: %w", ref.Name, err)
	}
	if !c.State.Running {
		res := hostConfig{NanoCPUs: int64(opts.CPUs) * 1e9}
		if opts.Memory != 0 {
			// No swap, as a machine of that size has none.
			res.Memory, res.MemorySwap = int64(opts.Memory), int64(opts.Memory)
		}
		if res.NanoCPUs != c.HostConfig.NanoCPUs || res.Memory != c.HostConfig.Memory {
			if err := d.api.update(ctx, ref.ID, res); err != nil {
				return nil, fmt.Errorf("docker: size %s: %w", ref.Name, err)
			}
		}
		if err := d.api.start(ctx, ref.ID); err != nil {
			return nil, fmt.Errorf("docker: start %s: %w", ref.Name, err)
		}
	}
	if opts.Console != nil {
		fmt.Fprintf(opts.Console, "docker: %s (%s) is running\n", ref.Name, ref.ID[:12])
	}
	return newContainer(d.api, ref), nil
}

// Attach finds the instance's container through the daemon, as any process
// can: the daemon owns it, so the engine runs no shim. A guest that powered
// itself off has already stopped its container, because Init exits with it.
func (d *Driver) Attach(ctx context.Context, inst machine.InstanceSpec) (machine.Machine, error) {
	var ref containerRef
	if err := fsutil.ReadJSON(filepath.Join(inst.Dir, containerFile), &ref); err != nil {
		return nil, fmt.Errorf("docker: instance %s has no container: %w", inst.ID, machine.ErrNotRunning)
	}
	c, err := d.api.inspectContainer(ctx, ref.ID)
	switch {
	case isNotFound(err):
		return nil, fmt.Errorf("docker: container %s is gone: %w", ref.Name, machine.ErrNotRunning)
	case err != nil:
		return nil, fmt.Errorf("docker: container %s: %w", ref.Name, err)
	case !c.State.Running:
		return nil, fmt.Errorf("docker: container %s is %s: %w", ref.Name, c.State.Status, machine.ErrNotRunning)
	}
	return newContainer(d.api, ref), nil
}

// Commit commits the stopped instance's container as a new image. It keeps
// the container's config, which is its parent layer's.
func (d *Driver) Commit(ctx context.Context, inst machine.InstanceSpec, dst machine.Layer) error {
	var ref containerRef
	if err := fsutil.ReadJSON(filepath.Join(inst.Dir, containerFile), &ref); err != nil {
		return fmt.Errorf("docker: instance %s: %w", inst.ID, err)
	}
	c, err := d.api.inspectContainer(ctx, ref.ID)
	if err != nil {
		return fmt.Errorf("docker: container %s: %w", ref.Name, err)
	}
	if c.State.Running {
		return fmt.Errorf("docker: commit needs a stopped container; %s is %s", ref.Name, c.State.Status)
	}
	// systemd wrote the guest's machine ID at its first boot; a layer that
	// kept it would give it to every instance built on the layer. (D-Bus's
	// copy is a link to it, from Install.)
	if err := d.api.putArchive(ctx, ref.ID, "/", emptyMachineID()); err != nil {
		return fmt.Errorf("docker: clear %s's machine ID: %w", ref.Name, err)
	}
	return d.commit(ctx, ref.ID, dst, nil)
}

// emptyMachineID is /etc/machine-id, empty and the guest root's, as a tar
// stream to extract at /. systemd makes a new ID at the next first boot.
func emptyMachineID() io.Reader {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "etc/machine-id", Typeflag: tar.TypeReg, Mode: 0o444, Uid: UIDBase, Gid: UIDBase})
	_ = tw.Close()
	return &buf
}

func (d *Driver) Destroy(ctx context.Context, inst machine.InstanceSpec) error {
	var ref containerRef
	if err := fsutil.ReadJSON(filepath.Join(inst.Dir, containerFile), &ref); err == nil {
		if err := d.api.removeContainer(ctx, ref.ID); err != nil {
			return fmt.Errorf("docker: remove %s: %w", ref.Name, err)
		}
	}
	return os.RemoveAll(inst.Dir)
}

// DeleteLayer removes the layer's tag, and with it the image, which nothing
// else uses: the store deletes a layer only once no layer is built on it.
func (d *Driver) DeleteLayer(ctx context.Context, layer machine.Layer) error {
	var ref layerRef
	if err := fsutil.ReadJSON(filepath.Join(layer.Dir, layerFile), &ref); err != nil {
		return nil //nolint:nilerr // never committed, or already released: nothing to delete
	}
	if err := d.api.removeImage(ctx, ref.Ref); err != nil {
		return fmt.Errorf("docker: remove image %s: %w", ref.Ref, err)
	}
	return nil
}

// Endpoint is empty: a container's service is reached by dialing its port
// through the daemon.
func (*Driver) Endpoint(context.Context, machine.InstanceSpec) (string, error) { return "", nil }

// removeContainer is best-effort cleanup of a container the driver made for
// itself.
func (d *Driver) removeContainer(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = d.api.removeContainer(ctx, id)
}

// dial reaches a guest port: the agent's port is its unix socket, through the
// guest's root directory, and any other is a TCP port on the loopback, which
// the guest shares with the container.
func dial(ctx context.Context, a *api, container string, port uint32) (net.Conn, error) {
	target := fmt.Sprintf("tcp:127.0.0.1:%d", port)
	if port == guest.AgentPort {
		target = "unix:" + relayDir + "/guest" + agentSocket
	}
	return dialExec(ctx, a, container, []string{agentPath, "pipe", target}, guestAddr{container: container, port: port})
}

// checkArch refuses an agent built for another CPU than the daemon's, which
// would otherwise fail later as a container that never starts.
func checkArch(agent, arch string) error {
	f, err := elf.Open(agent)
	if err != nil {
		return fmt.Errorf("docker: the agent %s is not a linux binary (build disco-vm with GOOS=linux and pass --agent): %w", agent, err)
	}
	defer f.Close()
	want := map[elf.Machine]string{elf.EM_X86_64: "x86_64", elf.EM_AARCH64: "aarch64"}[f.Machine]
	if arch != want {
		return fmt.Errorf("docker: the daemon runs %s but the agent %s is built for %s", arch, agent, f.Machine)
	}
	return nil
}

// containerName is a fresh container name that says where it came from.
func containerName(id string) string { return "dvm-" + label(id, 16) + "-" + fsutil.RandomHex(2) }

// label keeps the characters of s that a name allows, up to n of them.
func label(s string, n int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if b.Len() == n {
			break
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

func logger(w io.Writer) func(string, ...any) {
	return func(format string, args ...any) {
		if w != nil {
			fmt.Fprintf(w, format+"\n", args...)
		}
	}
}
