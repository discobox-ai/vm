// Package fake is a driver with no hypervisor. A "guest" is a disco-vm guest
// agent process on the host, confined to a directory that stands in for its
// disk; a copy of a committed VM is a directory copy.
//
// It exists so that everything above the driver seam — the builder, the
// engine, the guest protocol, the CLI — and the local driver's store and shim
// run exactly as they do against a real VM, on any machine and in CI. Its
// guest OS is always the host's.
package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

func init() {
	machine.Register("fake", machine.Registration{
		New: func(cfg machine.Config) (machine.Driver, error) {
			conf, err := decode(cfg.DriverConfig)
			if err != nil {
				return nil, err
			}
			hv, err := New(cfg.Base, conf)
			if err != nil {
				return nil, err
			}
			return local.New(cfg, hv)
		},
		Definition: Definition(),
		Validate:   Validate,
	})
}

// KeyEnv is the key the command line calls the fake driver with: one it
// issued. Unset, the caller is the root directory's owner.
const KeyEnv = "DISCO_VM_FAKE_KEY" //nolint:gosec // G101: an environment variable's name, not a credential

// DefaultBase is the base of a driver from New given none.
const DefaultBase = "scratch"

// Hypervisor is the fake driver's hypervisor, under a local.Driver.
type Hypervisor struct {
	// Agent is the disco-vm binary each guest runs as `disco-vm guest`.
	Agent string
	// BaseName names the hypervisor's base, a fresh VM's start: an empty
	// root whose marker names it, as an install's names its media. Empty
	// is none, and a fresh VM is refused.
	BaseName string
	// NoCommit makes a hypervisor that does not report Capabilities.Commit,
	// and so installs nothing either (ADR 26-11-03-868): a test option, so
	// that a driver's refusals of a commit and of a copy, and the paths
	// that skip them, run on every OS.
	NoCommit bool
	// MaxRunning is the Capabilities.MaxRunning it reports: none by default,
	// a test option so that the local driver's admission runs on every OS.
	MaxRunning map[machine.OS]int
}

var _ local.Hypervisor = (*Hypervisor)(nil)

// New returns a fake hypervisor whose base is base (machine.Config.Base),
// or DefaultBase, and whose guests run cfg's agent, or this binary. It
// commits unless cfg says not to, and caps running guests where cfg says.
func New(base string, cfg Config) (*Hypervisor, error) {
	agent := cfg.Agent
	if agent == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		agent = exe
	}
	if base == "" {
		base = DefaultBase
	}
	hv := &Hypervisor{Agent: agent, BaseName: base, NoCommit: cfg.NoCommit}
	if cfg.MaxRunning != nil {
		hv.MaxRunning = map[machine.OS]int{machine.OS(runtime.GOOS): *cfg.MaxRunning}
	}
	return hv, nil
}

func (*Hypervisor) Name() string { return "fake" }

func (d *Hypervisor) Capabilities() machine.Capabilities {
	caps := machine.Capabilities{
		GuestOS:    []machine.OS{machine.OS(runtime.GOOS)},
		Ports:      true,
		Agent:      true,
		Install:    []machine.OS{machine.OS(runtime.GOOS)},
		Commit:     true,
		Keys:       machine.KeyCapabilities{Mode: machine.KeysIssue, Scope: machine.ScopePool},
		Volumes:    []machine.Volume{machine.VolumeData, machine.VolumeCache},
		MaxRunning: d.MaxRunning,
	}
	if d.NoCommit {
		caps.Install, caps.Commit = nil, false
	}
	return caps
}

// Chains is false: a copy of a committed VM is a tree of its own.
func (*Hypervisor) Chains() bool { return false }

// Base is the host's OS, where the hypervisor has a base.
func (d *Hypervisor) Base() (machine.OS, bool) { return machine.OS(runtime.GOOS), d.BaseName != "" }

// SharesLoopback is true: a fake guest is a process on the host's loopback,
// and so are its peers.
func (*Hypervisor) SharesLoopback() bool { return true }

func (*Hypervisor) Check(context.Context) error { return nil }

const (
	rootfsName = "rootfs"
	addrName   = "agent.addr"
	// hostPortsName is where a running fake guest finds the ports its host
	// listens on (guest.HostPortsEnv).
	hostPortsName = "host-ports"
	// servicePortName holds the loopback port of the instance's service.
	servicePortName = "service.port"
	markerName      = ".disco-vm-install"
)

func rootfs(dir string) string { return filepath.Join(dir, rootfsName) }

// cacheName is the directory of a generation SaveCache writes.
const cacheName = "cache"

// volume is where a fake guest's volume is: a directory in its root at the
// guest's machine.VolumePath, which Commit leaves out of the image. Bytes
// are a hint: a directory has no size.
func volume(inst local.InstanceSpec, v machine.Volume) string {
	return guest.Resolve(rootfs(inst.Dir), machine.VolumePath(inst.GuestOS, v))
}

// prepareVolumes makes the instance's volumes in its root: empty, whatever
// the image holds at their paths, and the cache a copy of its generation.
func prepareVolumes(inst local.InstanceSpec) error {
	if inst.Data != nil {
		if err := freshDir(volume(inst, machine.VolumeData)); err != nil {
			return err
		}
	}
	if inst.Cache == nil {
		return nil
	}
	dir := volume(inst, machine.VolumeCache)
	if err := freshDir(dir); err != nil {
		return err
	}
	if inst.CacheFrom == nil {
		return nil
	}
	return fsutil.CopyTree(filepath.Join(inst.CacheFrom.Dir, cacheName), dir)
}

func freshDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

// Install creates an empty root. The media is not read; it is recorded, so a
// test can see what the spec asked for.
func (d *Hypervisor) Install(_ context.Context, spec local.InstallSpec, dst local.Layer) error {
	if spec.GuestOS != machine.OS(runtime.GOOS) {
		return fmt.Errorf("fake: a %s host can only fake %s guests, not %s", runtime.GOOS, runtime.GOOS, spec.GuestOS)
	}
	if err := os.MkdirAll(rootfs(dst.Dir), 0o755); err != nil {
		return err
	}
	if spec.Log != nil {
		fmt.Fprintf(spec.Log, "fake: installed %s from %q\n", spec.GuestOS, spec.Media)
	}
	marker := fmt.Sprintf("os=%s\nmedia=%s\nedition=%s\n", spec.GuestOS, spec.Media, spec.Edition)
	return os.WriteFile(filepath.Join(rootfs(dst.Dir), markerName), []byte(marker), 0o644) //nolint:gosec // G306: the fake guest's install marker, no secrets
}

func (d *Hypervisor) Prepare(_ context.Context, inst local.InstanceSpec) error {
	if len(inst.Chain) == 0 && d.BaseName == "" {
		return errors.New("fake: a fresh VM, and no base")
	}
	if err := os.MkdirAll(inst.Dir, 0o755); err != nil {
		return err
	}
	if err := pickServicePort(inst.Dir); err != nil {
		return err
	}
	if len(inst.Chain) == 0 {
		if err := os.MkdirAll(rootfs(inst.Dir), 0o755); err != nil {
			return err
		}
		marker := fmt.Sprintf("os=%s\nbase=%s\n", runtime.GOOS, d.BaseName)
		if err := os.WriteFile(filepath.Join(rootfs(inst.Dir), markerName), []byte(marker), 0o644); err != nil { //nolint:gosec // G306: the fake guest's marker, no secrets
			return err
		}
		return prepareVolumes(inst)
	}
	if err := fsutil.CopyTree(rootfs(inst.Parent().Dir), rootfs(inst.Dir)); err != nil {
		return err
	}
	return prepareVolumes(inst)
}

func (d *Hypervisor) Boot(ctx context.Context, inst local.InstanceSpec, opts local.BootOptions) (machine.Machine, error) {
	if err := machine.CheckFile(d.Agent); err != nil {
		return nil, fmt.Errorf("fake: the agent: %w", err)
	}
	addrFile := filepath.Join(inst.Dir, addrName)
	_ = os.Remove(addrFile)
	//nolint:gosec,noctx // G204: the agent is the driver's own disco-vm binary; noctx: the guest outlives Boot's ctx and stops through Stop or Kill
	cmd := exec.Command(d.Agent, "guest",
		"--listen", "tcp:127.0.0.1:0",
		"--addr-file", addrFile,
		"--root", rootfs(inst.Dir),
		"--fake")
	console := opts.Console
	if console == nil {
		console = io.Discard
	}
	cmd.Stdout, cmd.Stderr = console, console
	hostPorts := filepath.Join(inst.Dir, hostPortsName)
	_ = os.RemoveAll(hostPorts)
	if err := os.MkdirAll(hostPorts, 0o755); err != nil {
		return nil, err
	}
	cmd.Env = append(machine.WithoutKeys(os.Environ()), guest.HostPortsEnv+"="+hostPorts)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("fake: start guest agent %s: %w", d.Agent, err)
	}
	m := &proc{cmd: cmd, done: make(chan struct{}), hostPorts: hostPorts}
	go func() {
		err := cmd.Wait()
		// A guest that exits 0 powered itself off; anything else crashed.
		if err != nil && !m.killed.Load() {
			m.err = err
		}
		close(m.done)
	}()

	// The agent picks its own port and writes it down once it is listening.
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
listening:
	for {
		if data, err := os.ReadFile(addrFile); err == nil && strings.TrimSpace(string(data)) != "" {
			m.addr = strings.TrimSpace(string(data))
			break listening
		}
		select {
		case <-m.done:
			if m.err == nil {
				return nil, errors.New("fake: guest agent exited during boot")
			}
			return nil, fmt.Errorf("fake: guest agent exited during boot: %w", m.err)
		case <-ctx.Done():
			_ = m.Kill(context.Background())
			return nil, ctx.Err()
		case <-deadline.C:
			_ = m.Kill(context.Background())
			return nil, errors.New("fake: guest agent did not start listening")
		case <-time.After(20 * time.Millisecond):
		}
	}
	m.Client = guest.NewClient(func(ctx context.Context) (net.Conn, error) { return m.Dial(ctx, guest.AgentPort) })
	if err := m.WaitReady(ctx, m.done); err != nil {
		_ = m.Kill(context.Background())
		return nil, err
	}
	return m, nil
}

// Commit empties the VM's volumes in place: a committed VM never holds one,
// and its mount point stays, as an empty directory a real driver's would.
// What only a running guest needs goes too.
func (d *Hypervisor) Commit(_ context.Context, inst local.InstanceSpec) error {
	for v, attached := range map[machine.Volume]bool{machine.VolumeData: inst.Data != nil, machine.VolumeCache: inst.Cache != nil} {
		if attached {
			if err := freshDir(volume(inst, v)); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{addrName, hostPortsName} {
		if err := os.RemoveAll(filepath.Join(inst.Dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// MoveVolumes renames each of the instance's volumes from its old root into
// its new one, over whatever the new root holds at its path (a committed
// VM's empty mount point). A volume already moved is kept.
func (d *Hypervisor) MoveVolumes(_ context.Context, from, to local.InstanceSpec) error {
	for v, attached := range map[machine.Volume]bool{machine.VolumeData: from.Data != nil, machine.VolumeCache: from.Cache != nil} {
		if !attached {
			continue
		}
		src, dst := volume(from, v), volume(to, v)
		if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
			if _, err := os.Lstat(dst); err == nil {
				continue
			}
			return fmt.Errorf("fake: the %s volume is in neither the old root nor the new one", v)
		} else if err != nil {
			return err
		}
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// SaveCache copies the instance's cache volume into the generation dst.
func (d *Hypervisor) SaveCache(_ context.Context, inst local.InstanceSpec, dst local.Layer) error {
	return fsutil.CopyTree(volume(inst, machine.VolumeCache), filepath.Join(dst.Dir, cacheName))
}

func (d *Hypervisor) Destroy(_ context.Context, inst local.InstanceSpec) error {
	return os.RemoveAll(inst.Dir)
}

// pickServicePort gives the instance a free port on the host's loopback for
// its service: a fake guest is a host process, so a port of its own keeps two
// guests' services apart.
func pickServicePort(dir string) error {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, servicePortName), []byte(port), 0o644) //nolint:gosec // G306: a port number
}

// Service is the port Prepare picked on the host's loopback, which the
// instance's shim serves.
func (*Hypervisor) Service(inst local.InstanceSpec) (string, uint32, error) {
	data, err := os.ReadFile(filepath.Join(inst.Dir, servicePortName))
	if err != nil {
		return "", 0, fmt.Errorf("fake: instance %s is not prepared: %w", inst.ID, err)
	}
	port, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 16)
	if err != nil {
		return "", 0, fmt.Errorf("fake: instance %s service port: %w", inst.ID, err)
	}
	return "tcp:127.0.0.1:" + strconv.FormatUint(port, 10), uint32(port), nil
}

// proc is a running fake guest. Its guest operations are its agent's.
type proc struct {
	*guest.Client
	cmd       *exec.Cmd
	addr      string
	hostPorts string
	done      chan struct{}
	err       error
	once      sync.Once
	killed    atomic.Bool
}

var _ machine.Machine = (*proc)(nil)

// Dial reaches the agent at its own address. A fake guest is a host process,
// so any other port is the host's loopback.
func (m *proc) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	select {
	case <-m.done:
		return nil, errors.New("fake: guest is stopped")
	default:
	}
	addr := m.addr
	if port != guest.AgentPort {
		addr = net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(port), 10))
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

var _ machine.HostListener = (*proc)(nil)

// Listen listens on loopback and publishes the address where the fake guest,
// a host process, finds it (guest.DialHost).
func (m *proc) Listen(port uint32) (net.Listener, error) {
	// machine.HostListener.Listen takes no context; a loopback listen does not
	// block.
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	file := filepath.Join(m.hostPorts, strconv.FormatUint(uint64(port), 10))
	err = os.WriteFile(file+".tmp", []byte(l.Addr().String()), 0o644) //nolint:gosec // G306: a loopback address the fake guest reads, no secrets
	if err == nil {
		err = os.Rename(file+".tmp", file)
	}
	if err != nil {
		_ = l.Close()
		return nil, err
	}
	return &published{Listener: l, file: file}, nil
}

// published is a fake host port, unpublished when it closes.
type published struct {
	net.Listener
	file string
}

func (p *published) Close() error {
	_ = os.Remove(p.file)
	return p.Listener.Close()
}

func (m *proc) Kill(context.Context) error {
	m.once.Do(func() {
		m.killed.Store(true)
		_ = m.cmd.Process.Kill()
	})
	<-m.done
	return nil
}

func (m *proc) Done() <-chan struct{} { return m.done }

func (m *proc) Err() error {
	<-m.done
	return m.err
}
