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
// A base is any image the daemon has or can pull (Base). The guest's files
// must belong to its uids, so Create of an instance of a base readies it:
// installs systemd and the agent, shifts its files once (Shift), and commits
// that as the instance's own Docker image, which Destroy removes. An image
// (ADR 26-10-23-410) is a Docker image committed from a stopped container,
// written by the guest, in its uids already.
//
// The daemon owns a running guest, so every command attaches to the container
// (Attach), and the daemon keeps everything the driver knows (ADR
// 26-10-23-417): an image is a Docker image tagged
// disco-vm-layer:<project>--<id> and labeled with its comment, a tag is a tag
// of that image in disco-vm/<project>/, and an instance is a container named
// dvm-<project>--<name> and labeled with what it was made from. Nothing is
// kept on the host.
//
// The agent listens on a root-only unix socket in the guest, and is the
// container's guest operations (guest.Client). Each connection to it runs
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
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

func init() {
	machine.Register("docker", func(cfg machine.Config) (machine.Driver, error) { return New(cfg.Project), nil })
}

const (
	// UIDBase is where the guest's uids start on the host: guest uid N is
	// host uid UIDBase+N. It is baked into every image, so it is fixed. It is
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

	// imageRepo names the images the driver commits, each tagged
	// `<project>--<image id>`, and the image readied from a base for an
	// instance, `<project>--base-<instance name>--<random>`. It keeps the name it had
	// when images were layers.
	imageRepo = "disco-vm-layer"
	// tagRepo starts the repository of every tag, before the project:
	// disco-vm/<project>/<name>:<tag>.
	tagRepo = "disco-vm/"
	// namePrefix starts every container's name, before the project.
	namePrefix = "dvm-"

	// labelInstance is an instance's name, on its container.
	labelInstance = "ai.discobox.vm.instance"
	// labelID is an image's ID, on its Docker image and on the container of
	// an instance of it. It keeps the name it had when images were layers.
	labelID      = "ai.discobox.vm.layer"
	labelProject = "ai.discobox.vm.project"
	labelComment = "ai.discobox.vm.comment"
	// labelImage is the reference an instance was made from, and labelCPUs
	// and labelMemory its size, on its container.
	labelImage  = "ai.discobox.vm.image"
	labelCPUs   = "ai.discobox.vm.cpus"
	labelMemory = "ai.discobox.vm.memory"
)

// AgentEnv names the disco-vm binary, built for linux and the daemon's CPU,
// that Create puts in a base's image. It is this binary when unset on a linux
// host.
const AgentEnv = "DISCO_VM_AGENT"

// Driver is the docker driver.
type Driver struct {
	api     *api
	project string
	// Agent is the disco-vm binary Create puts in a base's image.
	Agent string
}

var _ machine.Driver = (*Driver)(nil)

// New returns a driver for the daemon $DOCKER_HOST names, for a project
// (machine.DefaultProject when empty), whose agent is $DISCO_VM_AGENT or, on
// linux, this binary. It dials nothing until it is used.
func New(project string) *Driver {
	if project == "" {
		project = machine.DefaultProject
	}
	agent := os.Getenv(AgentEnv)
	if agent == "" && runtime.GOOS == "linux" {
		agent, _ = os.Executable()
	}
	return &Driver{api: newAPI(), project: project, Agent: agent}
}

func (*Driver) Name() string { return "docker" }

func (*Driver) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS: []machine.OS{machine.Linux},
		Agent:   true,
		Keys:    machine.KeyCapabilities{Mode: machine.KeysNone},
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

// containerRef is a container the driver reaches.
type containerRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Install is not offered: any image the daemon can pull is a base instead.
func (*Driver) Install(context.Context, machine.InstallSpec, machine.Image) (machine.Image, error) {
	return machine.Image{}, fmt.Errorf("docker: run an image from a registry, such as debian:trixie: %w", machine.ErrUnsupported)
}

// ready makes the image an instance of a base runs: it pulls the base's
// image, installs systemd in it if it has none, installs the agent as a
// systemd service, shifts its files into the guest's uids, and commits it as
// tag, `<project>--base-<name>--<random>`, labeled with nothing of an image's,
// so no listing of images takes it for one. The container that readies it is
// an instance of the project's while it runs (`base-<random>`), so one a
// crash leaves is listed, and `rm` removes it.
func (d *Driver) ready(ctx context.Context, b machine.Base, name, tag string) error {
	image := b.Options["image"]
	in, err := d.api.info(ctx)
	if err != nil {
		return err
	}
	if d.Agent == "" {
		return fmt.Errorf("docker: a guest needs the disco-vm agent built for linux: set %s to one", AgentEnv)
	}
	if err := checkArch(d.Agent, in.Architecture); err != nil {
		return err
	}
	if _, err := d.api.inspectImage(ctx, image); isNotFound(err) {
		if err := d.api.pull(ctx, image); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	readying := "base-" + fsutil.RandomHex(3)
	id, err := d.api.createContainer(ctx, containerName(d.project, readying), containerConfig{
		Image:      image,
		Entrypoint: []string{"/bin/sh", "-c", installScript},
		Cmd:        []string{},
		User:       "0",
		Labels:     map[string]string{labelInstance: readying, labelProject: d.project, labelImage: image},
	})
	if err != nil {
		return fmt.Errorf("docker: create a container to ready %s: %w", image, err)
	}
	defer d.removeContainer(id)
	agent, err := agentTar(d.Agent)
	if err != nil {
		return err
	}
	if err := d.api.putArchive(ctx, id, "/", agent); err != nil {
		return fmt.Errorf("docker: copy the agent in: %w", err)
	}
	if err := d.api.start(ctx, id); err != nil {
		return fmt.Errorf("docker: start the container that readies %s: %w", image, err)
	}
	code, err := d.api.wait(ctx, id)
	if err != nil {
		return err
	}
	if code != 0 {
		out, _ := d.api.logs(context.Background(), id)
		return fmt.Errorf("docker: readying %s for %s exited %d:\n%s", image, name, code, strings.TrimRight(out, "\n"))
	}
	changes := []string{
		`ENTRYPOINT ["` + agentPath + `", "docker-init"]`,
		`CMD []`,
		`USER 0`,
		`WORKDIR /`,
		`STOPSIGNAL SIGTERM`,
	}
	for _, key := range []string{labelInstance, labelImage, labelProject} {
		changes = append(changes, "LABEL "+key+`=""`)
	}
	if _, err := d.api.commit(ctx, id, imageRepo, tag, changes); err != nil {
		return fmt.Errorf("docker: commit %s readied: %w", image, err)
	}
	return nil
}

// installScript runs as root in the container that readies a base, before the shift, so
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

// commit commits a stopped container as the image dst, its Docker image
// labeled with what the image is, and with no instance's labels, which the
// container carried.
func (d *Driver) commit(ctx context.Context, container string, dst machine.Image) error {
	labels := map[string]string{
		labelID: dst.ID, labelProject: d.project, labelComment: dst.Comment,
		labelInstance: "", labelImage: "", labelCPUs: "", labelMemory: "",
	}
	var changes []string
	for _, key := range sortedKeys(labels) {
		changes = append(changes, "LABEL "+key+"="+strconv.Quote(labels[key]))
	}
	if _, err := d.api.commit(ctx, container, imageRepo, imageTag(d.project, dst.ID), changes); err != nil {
		return fmt.Errorf("docker: commit: %w", err)
	}
	return nil
}

func (d *Driver) Create(ctx context.Context, spec machine.InstanceSpec) (machine.Instance, error) {
	if err := machine.ValidateName(spec.Name); err != nil {
		return machine.Instance{}, err
	}
	if spec.Base == nil && spec.ImageID == "" {
		return machine.Instance{}, errors.New("docker: an instance needs an image or a base")
	}
	if len(spec.Forwards) > 0 {
		return machine.Instance{}, fmt.Errorf("docker: forwards: %w", machine.ErrUnsupported)
	}
	if spec.Isolated || len(spec.Networks) > 0 {
		return machine.Instance{}, fmt.Errorf("docker: isolation: %w", machine.ErrUnsupported)
	}
	if spec.Egress != nil {
		return machine.Instance{}, fmt.Errorf("docker: egress: %w", machine.ErrUnsupported)
	}
	name := containerName(d.project, spec.Name)
	from, base := imageRepo+":"+imageTag(d.project, spec.ImageID), ""
	if spec.Base != nil {
		// Refused before the base is readied, which takes a minute.
		if _, err := d.api.inspectContainer(ctx, name); err == nil {
			return machine.Instance{}, fmt.Errorf("docker: an instance named %q: %w", spec.Name, machine.ErrExists)
		}
		// Named for the instance, with a random end, so two creates racing
		// for one name never share it.
		base = baseTag(d.project, spec.Name) + fsutil.RandomHex(3)
		if err := d.ready(ctx, *spec.Base, spec.Name, base); err != nil {
			return machine.Instance{}, err
		}
		from = imageRepo + ":" + base
	}
	image, err := d.api.inspectImage(ctx, from)
	switch {
	case isNotFound(err) && spec.Base != nil:
		return machine.Instance{}, fmt.Errorf("docker: %s, readied for %s, is gone: %w", from, spec.Name, machine.ErrNotFound)
	case isNotFound(err):
		return machine.Instance{}, fmt.Errorf("docker: image %s: %w", spec.ImageID, machine.ErrNotFound)
	case err != nil:
		d.removeImage(base)
		return machine.Instance{}, err
	}
	none := []string{}
	id, err := d.api.createContainer(ctx, name, containerConfig{
		Image:    image.ID,
		Hostname: spec.Name,
		Cmd:      []string{},
		Labels: map[string]string{
			labelInstance: spec.Name, labelProject: d.project, labelID: spec.ImageID, labelImage: spec.Image,
			labelCPUs: strconv.Itoa(spec.CPUs), labelMemory: strconv.FormatUint(spec.Memory, 10),
			labelComment: "",
		},
		// The service is published on the host's loopback, on a port the
		// daemon picks at every start.
		ExposedPorts: map[string]struct{}{servicePortKey: {}},
		HostConfig: hostConfig{
			PortBindings: map[string][]portBinding{servicePortKey: {{HostIP: "127.0.0.1"}}},
			SecurityOpt: []string{
				"seccomp=unconfined", "apparmor=unconfined", "label=disable", "writable-cgroups=true",
			},
			CgroupnsMode:  "private",
			MaskedPaths:   &none,
			ReadonlyPaths: &none,
			Tmpfs:         map[string]string{relayDir: "mode=0700"},
		},
	})
	if isConflict(err) {
		d.removeImage(base)
		return machine.Instance{}, fmt.Errorf("docker: an instance named %q: %w", spec.Name, machine.ErrExists)
	}
	if err != nil {
		d.removeImage(base)
		return machine.Instance{}, fmt.Errorf("docker: create %s: %w", name, err)
	}
	c, err := d.api.inspectContainer(ctx, id)
	if err != nil {
		d.removeContainer(id)
		d.removeImage(base)
		return machine.Instance{}, fmt.Errorf("docker: container %s: %w", name, err)
	}
	inst := d.instance(c, map[string]string{image.ID: spec.ImageID})
	inst.Base = spec.Base
	return inst, nil
}

// Start is Boot: the daemon owns the container, whatever process started it.
func (d *Driver) Start(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	return d.Boot(ctx, inst, opts)
}

func (d *Driver) Boot(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	if opts.GUI {
		return nil, fmt.Errorf("docker: a window on the guest's display: %w", machine.ErrUnsupported)
	}
	ref := containerRef{ID: inst.ID, Name: containerName(d.project, inst.Name)}
	c, err := d.api.inspectContainer(ctx, ref.ID)
	if err != nil {
		return nil, fmt.Errorf("docker: container %s: %w", ref.Name, err)
	}
	if !c.State.Running {
		res := hostConfig{NanoCPUs: int64(inst.CPUs) * 1e9}
		if inst.Memory != 0 {
			// No swap, as a machine of that size has none.
			res.Memory, res.MemorySwap = int64(inst.Memory), int64(inst.Memory)
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
	m := newContainer(d.api, ref)
	watch, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	if err := m.WaitReady(ctx, d.stopped(watch, ref.ID)); err != nil {
		_ = m.Kill(context.Background())
		return nil, fmt.Errorf("docker: %s: %w", ref.Name, err)
	}
	return m, nil
}

// stopped is closed if the container stops before ctx ends. It is not the
// machine's Done, whose watcher would outlive Boot in a program that embeds
// the engine: Boot ends ctx when it returns.
func (d *Driver) stopped(ctx context.Context, id string) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			if c, err := d.api.inspectContainer(ctx, id); isNotFound(err) || (err == nil && !c.State.Running) {
				close(ch)
				return
			}
		}
	}()
	return ch
}

// Attach finds the instance's container through the daemon, as any process
// can: the daemon owns it, so the engine runs no shim. A guest that powered
// itself off has already stopped its container, because Init exits with it.
func (d *Driver) Attach(ctx context.Context, inst machine.Instance) (machine.Machine, error) {
	ref := containerRef{ID: inst.ID, Name: containerName(d.project, inst.Name)}
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

// Commit commits the stopped instance's container as a new image, and
// removes the container. The image keeps the container's config, which is its
// image's.
func (d *Driver) Commit(ctx context.Context, inst machine.Instance, dst machine.Image) (machine.Image, error) {
	ref := containerRef{ID: inst.ID, Name: containerName(d.project, inst.Name)}
	c, err := d.api.inspectContainer(ctx, ref.ID)
	if err != nil {
		return machine.Image{}, fmt.Errorf("docker: container %s: %w", ref.Name, err)
	}
	if c.State.Running {
		return machine.Image{}, fmt.Errorf("docker: commit needs a stopped container; %s is %s", ref.Name, c.State.Status)
	}
	// systemd wrote the guest's machine ID at its first boot; an image that
	// kept it would give it to every instance of the image. (D-Bus's copy is
	// a link to it, from installScript.)
	if err := d.api.putArchive(ctx, ref.ID, "/", emptyMachineID()); err != nil {
		return machine.Image{}, fmt.Errorf("docker: clear %s's machine ID: %w", ref.Name, err)
	}
	dst.GuestOS = machine.Linux
	if err := d.commit(ctx, ref.ID, dst); err != nil {
		return machine.Image{}, err
	}
	img, err := d.Image(ctx, dst.ID)
	if err != nil {
		return machine.Image{}, err
	}
	if err := d.Destroy(ctx, inst); err != nil {
		return img, err
	}
	return img, nil
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

// staleBase is how old an image readied from a base must be before Destroy
// takes it for one a failed Destroy, or a create that lost a race, left: a
// readying takes minutes, and one that is younger may be a create of the same
// name still under way.
const staleBase = 30 * time.Minute

// Destroy removes the instance's container, then the image readied from a
// base for it, the one its container ran, and every other one of its name no
// container runs that is older than staleBase. One a later instance of the
// name runs is in use, and the daemon keeps it. An image snapshotted from the
// instance keeps what it needs of its own: the daemon only untags an image
// another is built on.
func (d *Driver) Destroy(ctx context.Context, inst machine.Instance) error {
	own := ""
	if c, err := d.api.inspectContainer(ctx, inst.ID); err == nil {
		own = c.Image
	}
	if err := d.api.removeContainer(ctx, inst.ID); err != nil {
		return fmt.Errorf("docker: remove %s: %w", containerName(d.project, inst.Name), err)
	}
	images, err := d.api.images(ctx, imageRepo)
	if err != nil {
		return fmt.Errorf("docker: list images: %w", err)
	}
	prefix := imageRepo + ":" + baseTag(d.project, inst.Name)
	for _, img := range images {
		if img.ID != own && time.Since(time.Unix(img.Created, 0)) < staleBase {
			continue
		}
		for _, tag := range img.RepoTags {
			if !strings.HasPrefix(tag, prefix) {
				continue
			}
			if err := d.api.removeImage(ctx, tag); err != nil && !isConflict(err) {
				return fmt.Errorf("docker: remove image %s: %w", tag, err)
			}
		}
	}
	return nil
}

// servicePortKey is guest.ServicePort as the daemon's API names a port.
var servicePortKey = fmt.Sprintf("%d/tcp", guest.ServicePort)

// Service is guest.ServicePort on the container's network, published on the
// host's loopback at a port the daemon picked when the container started, so
// its URL lasts only as long as the container runs. The daemon is the
// host's own: one reached over tcp:// publishes on its own loopback. Its
// PrivateURL is the port at the container's address on its network, which
// another container there dials, as it does every other port.
func (d *Driver) Service(ctx context.Context, inst machine.Instance) (machine.Service, error) {
	service := machine.Service{Listen: fmt.Sprintf("tcp:0.0.0.0:%d", guest.ServicePort)}
	ref := containerRef{ID: inst.ID, Name: containerName(d.project, inst.Name)}
	c, err := d.api.inspectContainer(ctx, ref.ID)
	switch {
	case isNotFound(err):
		return service, fmt.Errorf("docker: container %s is gone: %w", ref.Name, machine.ErrNotRunning)
	case err != nil:
		return service, fmt.Errorf("docker: container %s: %w", ref.Name, err)
	case !c.State.Running:
		return service, fmt.Errorf("docker: container %s is %s: %w", ref.Name, c.State.Status, machine.ErrNotRunning)
	}
	for _, b := range c.NetworkSettings.Ports[servicePortKey] {
		if b.HostPort != "" {
			service.URL = "http://" + net.JoinHostPort("127.0.0.1", b.HostPort)
			break
		}
	}
	if service.URL == "" {
		return service, fmt.Errorf("docker: container %s publishes no service port; create the instance again", ref.Name)
	}
	// A container with no IPv4 address on its network (an IPv6-only one) has
	// no private URL, and its URL still serves.
	if addr := containerAddress(c); addr != "" {
		service.PrivateURL = "http://" + net.JoinHostPort(addr, strconv.FormatUint(uint64(guest.ServicePort), 10))
	}
	return service, nil
}

// containerAddress is a running container's address on its network: the
// first by name, on the rare container on more than one.
func containerAddress(c containerInfo) string {
	for _, name := range slices.Sorted(maps.Keys(c.NetworkSettings.Networks)) {
		if addr := c.NetworkSettings.Networks[name].IPAddress; addr != "" {
			return addr
		}
	}
	return ""
}

// removeContainer is best-effort cleanup of a container the driver made for
// itself.
// removeImage is best-effort cleanup of an image tag the driver made for
// itself in imageRepo; an empty tag is none.
func (d *Driver) removeImage(tag string) {
	if tag == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = d.api.removeImage(ctx, imageRepo+":"+tag)
}

func (d *Driver) removeContainer(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = d.api.removeContainer(ctx, id)
}

// dialAgent reaches the guest agent's unix socket, through the guest's root
// directory.
func dialAgent(ctx context.Context, a *api, container string) (net.Conn, error) {
	return dialExec(ctx, a, container, []string{agentPath, "pipe", "unix:" + relayDir + "/guest" + agentSocket}, guestAddr{container: container, port: guest.AgentPort})
}

// checkArch refuses an agent built for another CPU than the daemon's, which
// would otherwise fail later as a container that never starts.
func checkArch(agent, arch string) error {
	f, err := elf.Open(agent)
	if err != nil {
		return fmt.Errorf("docker: the agent %s is not a linux binary (build disco-vm with GOOS=linux and set DISCO_VM_AGENT to it): %w", agent, err)
	}
	defer f.Close()
	want := map[elf.Machine]string{elf.EM_X86_64: "x86_64", elf.EM_AARCH64: "aarch64"}[f.Machine]
	if arch != want {
		return fmt.Errorf("docker: the daemon runs %s but the agent %s is built for %s", arch, agent, f.Machine)
	}
	return nil
}

// containerName is an instance's container: `dvm-<project>--<name>`.
func containerName(project, name string) string {
	return machine.ResourcePrefix(namePrefix, project) + name
}

// imageTag is an image's Docker image's tag in imageRepo: `<project>--<id>`.
func imageTag(project, id string) string { return machine.ResourcePrefix("", project) + id }

// baseTag starts the tag of every image readied from a base for an instance
// of a name: `<project>--base-<name>--`, which a random end completes. No
// name has "--" in it, so no other name's tags start the same way.
func baseTag(project, name string) string { return imageTag(project, "base-"+name+"--") }
