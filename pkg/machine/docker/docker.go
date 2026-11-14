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
// The base is an image a registry has, configured (machine.Config.Base). The
// guest's files must belong to its uids, so a fresh Create readies it:
// installs systemd and the agent, shifts its files once (Shift), and commits
// that as the VM's own Docker image, which Destroy removes. A committed VM
// (ADR 26-11-03-606) is a Docker image committed from a stopped container,
// written by the guest, in its uids already, and a VM created from it is a
// container of that image.
//
// The daemon owns a running guest, so every command attaches to the container
// (Attach), and the daemon keeps everything the driver knows (ADR
// 26-10-23-417): a VM is a container named dvm-<pool>--<name>, labeled
// with its name, size, and volumes, and a committed VM a Docker image tagged
// disco-vm-vm:<pool>--<name>, labeled likewise. Nothing is kept on the
// host.
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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

func init() {
	machine.Register("docker", machine.Registration{
		New:        func(cfg machine.Config) (machine.Driver, error) { return New(cfg) },
		Definition: Definition(),
		Validate:   Validate,
	})
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

	// baseRepo names the image readied from the base for a fresh VM,
	// `<pool>--<name>--<random>`.
	baseRepo = "disco-vm-base"
	// vmRepo names the committed VMs' images, `<pool>--<name>` (vmTag).
	vmRepo = "disco-vm-vm"
	// namePrefix starts every container's name, before the pool.
	namePrefix = "dvm-"

	// labelInstance is a VM's name, on its container or its committed
	// image.
	labelInstance = "ai.discobox.vm.instance"
	labelPool     = "ai.discobox.vm.pool"
	// labelCommitted is set on a committed VM's image, and blank on every
	// container, which inherits its image's labels.
	labelCommitted = "ai.discobox.vm.committed"
	// labelFrom is the committed VM a VM was created from, and labelCPUs and
	// labelMemory its size, on its container and its committed image.
	labelFrom   = "ai.discobox.vm.from"
	labelCPUs   = "ai.discobox.vm.cpus"
	labelMemory = "ai.discobox.vm.memory"
	// labelImage is the base's reference, on the container that readies it.
	labelImage = "ai.discobox.vm.image"
)

// Driver is the docker driver.
type Driver struct {
	api  *api
	pool string
	// base is the image a fresh VM is readied from; empty is none.
	base string
	// Agent is the disco-vm binary Create puts in a base's image, and the
	// helper image (helperRepo) runs.
	Agent string
	// Root is where the driver keeps volumes and caches on the daemon's host
	// (ADR 26-11-03-782): Config.Root, or DefaultRoot.
	Root string
}

var _ machine.Driver = (*Driver)(nil)

// New returns a driver for the daemon its configuration (cfg.DriverConfig)
// names (HostDaemon), reached through cfg.Dial, as NewWithDaemon does.
func New(cfg machine.Config) (*Driver, error) {
	config, err := machine.Normalize("docker", cfg.DriverConfig)
	if err != nil {
		return nil, err
	}
	conf, err := decode(config)
	if err != nil {
		return nil, err
	}
	return newDriver(cfg, conf, HostDaemon(conf.Host, cfg.Dialer())), nil
}

// NewWithDaemon returns a driver for the daemon d, for cfg's pool
// (machine.DefaultPool when empty), whose fresh VMs are readied from
// cfg.Base, whose agent is the configured one, with no fallback,
// and whose directories are under the configured root on the daemon's
// host. A configuration that names a host is refused: the daemon is d. It
// dials nothing until it is used.
func NewWithDaemon(cfg machine.Config, d Daemon) (*Driver, error) {
	// Normalized first, as machine.New does, so a stored configuration
	// with a retired key is taken here too (ADR 26-11-11-215 §3).
	config, err := machine.Normalize("docker", cfg.DriverConfig)
	if err != nil {
		return nil, err
	}
	if given, err := machine.DecodeConfig[Config]("docker", config); err != nil {
		return nil, err
	} else if given.Host != "" {
		return nil, errors.New("docker: configuration: host: the driver is given its daemon")
	}
	conf, err := decode(config)
	if err != nil {
		return nil, err
	}
	return newDriver(cfg, conf, d), nil
}

func newDriver(cfg machine.Config, conf Config, d Daemon) *Driver {
	pool := cfg.Pool
	if pool == "" {
		pool = machine.DefaultPool
	}
	return &Driver{api: newAPI(d), pool: pool, base: cfg.Base, Agent: conf.Agent, Root: conf.Root}
}

func (*Driver) Name() string { return "docker" }

func (*Driver) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS: []machine.OS{machine.Linux},
		Agent:   true,
		Commit:  true,
		Keys:    machine.KeyCapabilities{Mode: machine.KeysNone},
		Volumes: []machine.Volume{machine.VolumeData, machine.VolumeCache},
		// The container's address on its network; its URL is a port the
		// daemon publishes on the host's loopback, never public.
		URLs: machine.URLCapabilities{Private: machine.URLAlways},
	}
}

func (d *Driver) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	in, err := d.api.info(ctx)
	if err != nil {
		return fmt.Errorf("docker: %s: %w", d.api.name, err)
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

// Install is not offered: a fresh VM is readied from the configured base.
func (*Driver) Install(context.Context, machine.InstallSpec, string) (machine.Instance, error) {
	return machine.Instance{}, fmt.Errorf("docker: create a fresh VM from the configured base (%s, such as debian:sid) instead: %w", machine.BaseEnv, machine.ErrUnsupported)
}

// ready makes the image a fresh VM runs: it pulls the base's image, installs
// systemd in it if it has none, installs the agent as a systemd service,
// shifts its files into the guest's uids, and commits it as
// baseRepo:<pool>--<name>--<random>, labeled with nothing of a VM's. The
// container that readies it is a VM of the pool's while it runs
// (`base-<random>`), so one a crash leaves is listed, and `rm` removes it.
func (d *Driver) ready(ctx context.Context, image, name, tag string) error {
	in, err := d.api.info(ctx)
	if err != nil {
		return err
	}
	if d.Agent == "" {
		return fmt.Errorf("docker: a guest needs the disco-vm agent built for linux: configure agent (%s) as one", AgentEnv)
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
	readying := "base-" + fsutil.RandomHex(3) // readyingName
	id, err := d.api.createContainer(ctx, containerName(d.pool, readying), containerConfig{
		Image:      image,
		Entrypoint: []string{"/bin/sh", "-c", installScript},
		Cmd:        []string{},
		User:       "0",
		Labels:     map[string]string{labelInstance: readying, labelPool: d.pool, labelImage: image},
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
	for _, key := range []string{labelInstance, labelImage, labelPool} {
		changes = append(changes, "LABEL "+key+`=""`)
	}
	if _, err := d.api.commit(ctx, id, baseRepo, tag, changes); err != nil {
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

// commitVM commits a stopped container as the committed VM inst,
// vmRepo:<pool>--<name>, labeled with what it is, and with nothing of a
// running VM's, which the container carried and every container of it would
// inherit.
func (d *Driver) commitVM(ctx context.Context, container string, inst machine.Instance) error {
	labels := map[string]string{
		labelInstance: inst.Name, labelPool: d.pool, labelCommitted: "true", labelFrom: inst.From,
		labelCPUs: strconv.Itoa(inst.CPUs), labelMemory: strconv.FormatUint(inst.Memory, 10), labelImage: "",
		// A committed VM never holds a volume, and replaces no container.
		labelData: "", labelCache: "", labelCacheBytes: "", labelGeneration: "", labelReplaces: "", labelReplacedImage: "",
	}
	var changes []string
	for _, key := range sortedKeys(labels) {
		changes = append(changes, "LABEL "+key+"="+strconv.Quote(labels[key]))
	}
	repo, tag, _ := strings.Cut(vmTag(d.pool, inst.Name), ":")
	if _, err := d.api.commit(ctx, container, repo, tag, changes); err != nil {
		return fmt.Errorf("docker: commit: %w", err)
	}
	return nil
}

// Create makes a container of the committed VM spec.From's image, or of an
// image readied from the base for a fresh VM. A name a committed VM or a
// container holds is ErrExists, checked again once the container is made,
// since a commit of another VM of the name may have taken it in between.
func (d *Driver) Create(ctx context.Context, spec machine.InstanceSpec) (machine.Instance, error) {
	if err := machine.ValidateName(spec.Name); err != nil {
		return machine.Instance{}, err
	}
	if len(spec.Forwards) > 0 {
		return machine.Instance{}, fmt.Errorf("docker: forwards: %w", machine.ErrUnsupported)
	}
	if spec.From != nil && machine.OtherPool(spec.From.Pool, d.pool) {
		return machine.Instance{}, fmt.Errorf("docker: a VM from another pool's (%s): %w", spec.From.Pool, machine.ErrUnsupported)
	}
	if spec.Isolated || len(spec.Networks) > 0 {
		return machine.Instance{}, fmt.Errorf("docker: isolation: %w", machine.ErrUnsupported)
	}
	if err := spec.URLs.Check(d.Capabilities().URLs); err != nil {
		return machine.Instance{}, fmt.Errorf("docker: %w", err)
	}
	if spec.Egress != nil {
		return machine.Instance{}, fmt.Errorf("docker: egress: %w", machine.ErrUnsupported)
	}
	if spec.From == nil && d.base == "" {
		return machine.Instance{}, fmt.Errorf("docker: a fresh VM needs a base image, which is the driver's configuration: set %s to one, such as debian:sid: %w", machine.BaseEnv, machine.ErrUnsupported)
	}
	name := containerName(d.pool, spec.Name)
	// Refused before a base is readied, which takes a minute.
	if err := d.free(ctx, spec.Name); err != nil {
		return machine.Instance{}, err
	}
	var from, base, fromName string
	if spec.From != nil {
		src, found, err := d.committed(ctx, spec.From.Name)
		if err != nil {
			return machine.Instance{}, err
		}
		if !found {
			return machine.Instance{}, fmt.Errorf("docker: committed VM %s: %w", spec.From.Name, machine.ErrNotFound)
		}
		from, fromName = src.ID, src.Name
	} else {
		// Named for the VM, with a random end, so two creates racing for one
		// name never share it.
		base = baseTag(d.pool, spec.Name) + fsutil.RandomHex(3)
		if err := d.ready(ctx, d.base, spec.Name, base); err != nil {
			return machine.Instance{}, err
		}
		from = baseRepo + ":" + base
	}
	image, err := d.api.inspectImage(ctx, from)
	switch {
	case isNotFound(err) && spec.From == nil:
		return machine.Instance{}, fmt.Errorf("docker: %s, readied for %s, is gone: %w", from, spec.Name, machine.ErrNotFound)
	case isNotFound(err):
		return machine.Instance{}, fmt.Errorf("docker: committed VM %s: %w", fromName, machine.ErrNotFound)
	case err != nil:
		d.removeImage(base)
		return machine.Instance{}, err
	}
	vols, err := d.prepareVolumes(ctx, spec)
	if err != nil {
		d.unstage(vols.staging)
		d.removeImage(base)
		return machine.Instance{}, err
	}
	labels := map[string]string{
		labelInstance: spec.Name, labelPool: d.pool, labelFrom: fromName,
		labelCPUs: strconv.Itoa(spec.CPUs), labelMemory: strconv.FormatUint(spec.Memory, 10),
		// Set even when empty, over any its image carries.
		labelCommitted: "", labelImage: "", labelReplaces: "", labelReplacedImage: "",
		labelData: "", labelCache: "", labelCacheBytes: "", labelGeneration: "",
	}
	maps.Copy(labels, vols.labels)
	id, err := d.api.createContainer(ctx, name, d.vmConfig(image.ID, spec.Name, labels, vols.mounts))
	if isConflict(err) {
		d.unstage(vols.staging)
		d.removeImage(base)
		return machine.Instance{}, fmt.Errorf("docker: a VM named %q: %w", spec.Name, machine.ErrExists)
	}
	if err != nil {
		d.unstage(vols.staging)
		d.removeImage(base)
		return machine.Instance{}, fmt.Errorf("docker: create %s: %w", name, err)
	}
	c, err := d.api.inspectContainer(ctx, id)
	if err == nil {
		if _, taken, cerr := d.committed(ctx, spec.Name); cerr != nil {
			err = cerr
		} else if taken {
			err = fmt.Errorf("a VM named %q was committed meanwhile: %w", spec.Name, machine.ErrExists)
		}
	}
	if err == nil {
		// The container holds the name, so the directories are this VM's.
		err = d.installVolumes(ctx, spec, vols)
	}
	if err != nil {
		d.removeContainer(id)
		d.unstage(vols.staging)
		d.removeImage(base)
		return machine.Instance{}, fmt.Errorf("docker: container %s: %w", name, err)
	}
	return d.instance(c), nil
}

// vmConfig is a VM's container: of image, named for the VM, with its labels
// and mounts.
func (d *Driver) vmConfig(image, name string, labels map[string]string, mounts []mount) containerConfig {
	none := []string{}
	return containerConfig{
		Image:    image,
		Hostname: name,
		Cmd:      []string{},
		Labels:   labels,
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
			Mounts:        mounts,
		},
	}
}

// free refuses a name the pool's committed VMs or containers hold.
func (d *Driver) free(ctx context.Context, name string) error {
	if _, taken, err := d.committed(ctx, name); err != nil {
		return err
	} else if taken {
		return fmt.Errorf("docker: a committed VM named %q: %w", name, machine.ErrExists)
	}
	for _, ref := range []string{containerName(d.pool, name), upgradeName(d.pool, name)} {
		if _, err := d.api.inspectContainer(ctx, ref); err == nil {
			return fmt.Errorf("docker: a VM named %q: %w", name, machine.ErrExists)
		} else if !isNotFound(err) {
			return err
		}
	}
	return nil
}

// image reports a committed VM, which is a Docker image, by its ID: the
// daemon's image IDs are `sha256:<hex>`, and its container IDs bare hex.
// It is the provider's own kind of resource, which the seam does not report
// (whether a VM is committed is the caller's to record).
func image(inst machine.Instance) bool { return strings.HasPrefix(inst.ID, "sha256:") }

// refuseCommitted is ErrCommitted for a committed VM, which has no
// container.
func refuseCommitted(inst machine.Instance) error {
	if image(inst) {
		return fmt.Errorf("docker: VM %s: %w", inst.Name, machine.ErrCommitted)
	}
	return nil
}

// Start is Boot: the daemon owns the container, whatever process started it.
func (d *Driver) Start(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	return d.Boot(ctx, inst, opts)
}

func (d *Driver) Boot(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	if err := refuseCommitted(inst); err != nil {
		return nil, err
	}
	if opts.GUI {
		return nil, fmt.Errorf("docker: a window on the guest's display: %w", machine.ErrUnsupported)
	}
	c, err := d.whole(ctx, inst)
	if err != nil {
		return nil, fmt.Errorf("docker: container %s: %w", containerName(d.pool, inst.Name), err)
	}
	ref := containerRef{ID: c.ID, Name: containerName(d.pool, inst.Name)}
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
	if image(inst) {
		return nil, fmt.Errorf("docker: VM %s is committed, an image with no container: %w", inst.Name, machine.ErrNotRunning)
	}
	c, err := d.resolve(ctx, inst)
	ref := containerRef{ID: c.ID, Name: containerName(d.pool, inst.Name)}
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

// Commit commits the stopped VM's container as its committed image,
// vmRepo:<pool>--<name>, then removes the container, its volumes, and the
// image readied for it. A container a failed removal leaves is the committed
// VM's: it is listed no more, and Destroy of the committed VM removes it.
func (d *Driver) Commit(ctx context.Context, inst machine.Instance) (machine.Instance, error) {
	if err := refuseCommitted(inst); err != nil {
		return machine.Instance{}, err
	}
	c, err := d.whole(ctx, inst)
	if err != nil {
		return machine.Instance{}, fmt.Errorf("docker: container %s: %w", containerName(d.pool, inst.Name), err)
	}
	ref := containerRef{ID: c.ID, Name: containerName(d.pool, inst.Name)}
	if c.State.Running {
		return machine.Instance{}, fmt.Errorf("docker: commit needs a stopped container; %s is %s", ref.Name, c.State.Status)
	}
	// systemd wrote the guest's machine ID at its first boot; a committed VM
	// that kept it would give it to every VM created from it. (D-Bus's copy
	// is a link to it, from installScript.)
	if err := d.api.putArchive(ctx, ref.ID, "/", emptyMachineID()); err != nil {
		return machine.Instance{}, fmt.Errorf("docker: clear %s's machine ID: %w", ref.Name, err)
	}
	if err := d.commitVM(ctx, ref.ID, d.instance(c)); err != nil {
		return machine.Instance{}, err
	}
	committed, found, err := d.committed(ctx, inst.Name)
	if err == nil && !found {
		err = fmt.Errorf("docker: %s, just committed, is gone: %w", vmTag(d.pool, inst.Name), machine.ErrNotFound)
	}
	if err != nil {
		return machine.Instance{}, err
	}
	if err := d.destroyVM(ctx, inst.Name, []containerInfo{c}, c.ID); err != nil {
		return committed, err
	}
	return committed, nil
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

// staleBase is how old an image readied from the base must be before Destroy
// takes it for one a failed Destroy, or a create that lost a race, left: a
// readying takes minutes, and one that is younger may be a create of the same
// name still under way.
const staleBase = 30 * time.Minute

// Destroy removes a committed VM's image, or a VM's containers and
// directories. A VM created from a committed VM runs its image by ID, so it
// stands alone: the daemon only untags an image a container uses, and the
// Destroy of the last such container removes it.
func (d *Driver) Destroy(ctx context.Context, inst machine.Instance) error {
	if !image(inst) {
		cs, err := d.vmContainers(ctx, inst)
		if err != nil {
			return err
		}
		return d.destroyVM(ctx, inst.Name, cs, inst.ID)
	}
	// A container a failed commit left goes first, while the image names it,
	// and an upgrade's new one a commit racing it left.
	for _, ref := range []string{containerName(d.pool, inst.Name), upgradeName(d.pool, inst.Name)} {
		if c, err := d.api.inspectContainer(ctx, ref); err == nil && d.managed(c.Config.Labels) {
			if err := d.destroyVM(ctx, inst.Name, []containerInfo{c}, c.ID); err != nil {
				return err
			}
		}
	}
	tag := vmTag(d.pool, inst.Name)
	img, err := d.api.inspectImage(ctx, tag)
	if isNotFound(err) || (err == nil && img.ID != inst.ID) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("docker: image %s: %w", tag, err)
	}
	if err := d.api.removeImageForce(ctx, tag); err != nil {
		return fmt.Errorf("docker: remove committed VM %s: %w", tag, err)
	}
	return nil
}

// destroyVM removes a VM's directories, then its containers cs (or, with
// none, the container id, which may be gone already), then the named
// volumes of a VM from before ADR 26-11-03-782, then the images the
// containers ran where nothing else needs them. Directories go first, so a
// Destroy that fails leaves the VM listed, and a second one finds them; a
// VM whose containers are gone has none left.
func (d *Driver) destroyVM(ctx context.Context, name string, cs []containerInfo, id string) error {
	if slices.ContainsFunc(cs, hasDirs) {
		if _, err := d.runDirs(ctx, dirsRequest{Op: opRemove, Name: name}, ""); err != nil {
			return err
		}
	}
	if len(cs) == 0 {
		cs = []containerInfo{{ID: id}}
	}
	for _, c := range cs {
		if err := d.api.removeContainer(ctx, c.ID); err != nil {
			return fmt.Errorf("docker: remove %s: %w", containerName(d.pool, name), err)
		}
	}
	if err := d.removeOldVolumes(ctx, name); err != nil {
		return err
	}
	for _, c := range cs {
		for _, own := range []string{c.Image, c.Config.Labels[labelReplacedImage]} {
			if err := d.removeImages(ctx, own, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeImages removes own, the image a removed container of the VM name
// ran, if no tag names it any more (a committed VM removed while the
// container ran it), then the image readied from the base for it, and
// every other one of its name no container runs that is older than
// staleBase. One a later VM of the name, or another VM, runs is in use, and
// the daemon keeps it.
func (d *Driver) removeImages(ctx context.Context, own, name string) error {
	if own != "" {
		if img, err := d.api.inspectImage(ctx, own); err == nil && len(img.RepoTags) == 0 {
			if err := d.api.removeImage(ctx, own); err != nil && !isConflict(err) {
				return fmt.Errorf("docker: remove image %s: %w", own, err)
			}
		}
	}
	images, err := d.api.images(ctx, baseRepo)
	if err != nil {
		return fmt.Errorf("docker: list images: %w", err)
	}
	prefix := baseRepo + ":" + baseTag(d.pool, name)
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
	if image(inst) {
		return service, fmt.Errorf("docker: VM %s is committed, an image with no container: %w", inst.Name, machine.ErrNotRunning)
	}
	c, err := d.resolve(ctx, inst)
	ref := containerRef{ID: c.ID, Name: containerName(d.pool, inst.Name)}
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
// itself in baseRepo; an empty tag is none.
func (d *Driver) removeImage(tag string) {
	if tag == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = d.api.removeImage(ctx, baseRepo+":"+tag)
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

// openAgent opens the agent as an ELF file, once it is checked to be a
// regular file at an absolute path (ADR 26-11-11-215 §7): nothing else is
// read from a path the configuration names.
func openAgent(agent string) (*elf.File, error) {
	if err := machine.CheckFile(agent); err != nil {
		return nil, fmt.Errorf("docker: the agent: %w", err)
	}
	f, err := elf.Open(agent)
	if err != nil {
		return nil, fmt.Errorf("docker: the agent %s is not a linux binary (build disco-vm with GOOS=linux and configure agent, $DISCO_VM_AGENT, as it): %w", agent, err)
	}
	return f, nil
}

// checkArch refuses an agent built for another CPU than the daemon's, which
// would otherwise fail later as a container that never starts.
func checkArch(agent, arch string) error {
	f, err := openAgent(agent)
	if err != nil {
		return err
	}
	defer f.Close()
	want := map[elf.Machine]string{elf.EM_X86_64: "x86_64", elf.EM_AARCH64: "aarch64"}[f.Machine]
	if arch != want {
		return fmt.Errorf("docker: the daemon runs %s but the agent %s is built for %s", arch, agent, f.Machine)
	}
	return nil
}

// containerName is an instance's container: `dvm-<pool>--<name>`.
func containerName(pool, name string) string {
	return machine.ResourcePrefix(namePrefix, pool) + name
}

// baseTag starts the tag in baseRepo of every image readied from the base
// for a VM of a name: `<pool>--<name>--`, which a random end completes.
// No name has "--" in it, so no other name's tags start the same way.
func baseTag(pool, name string) string { return machine.ResourcePrefix("", pool) + name + "--" }
