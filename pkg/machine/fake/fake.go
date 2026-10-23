// Package fake is a driver with no hypervisor. A "guest" is a disco-vm guest
// agent process on the host, confined to a directory that stands in for its
// disk; clones and commits are directory copies.
//
// It exists so that everything above the driver seam — the builder, the
// engine, the guest protocol, the CLI — and the local driver's store and shim
// run exactly as they do against a real VM, on any machine and in CI. Its
// guest OS is always the host's.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	machine.Register("fake", func(cfg machine.Config) (machine.Driver, error) {
		hv, err := New()
		if err != nil {
			return nil, err
		}
		return local.New(cfg, hv)
	})
}

// AgentEnv overrides the agent binary; by default it is the running disco-vm.
const AgentEnv = "DISCO_VM_AGENT"

// BasesEnv is a JSON array of machine.Base, the Catalog a driver from New
// lists, so a test of the CLI has bases to run.
const BasesEnv = "DISCO_VM_FAKE_BASES"

// Hypervisor is the fake driver's hypervisor, under a local.Driver.
type Hypervisor struct {
	// Agent is the disco-vm binary each guest runs as `disco-vm guest`.
	Agent string
	// Catalog is what Bases lists. An instance of one starts from an empty
	// root whose marker names the base, as an install's names its media.
	Catalog []machine.Base
}

var _ local.Hypervisor = (*Hypervisor)(nil)

// New returns a fake hypervisor whose guests run this binary, or
// $DISCO_VM_AGENT.
func New() (*Hypervisor, error) {
	agent := os.Getenv(AgentEnv)
	if agent == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		agent = exe
	}
	d := &Hypervisor{Agent: agent}
	if raw := os.Getenv(BasesEnv); raw != "" {
		if err := json.Unmarshal([]byte(raw), &d.Catalog); err != nil {
			return nil, fmt.Errorf("fake: %s: %w", BasesEnv, err)
		}
	}
	return d, nil
}

func (*Hypervisor) Name() string { return "fake" }

func (*Hypervisor) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS: []machine.OS{machine.OS(runtime.GOOS)},
		Forward: true,
		Agent:   true,
	}
}

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
	if len(inst.Chain) == 0 && inst.Base == nil {
		return errors.New("fake: instance has no image")
	}
	if err := os.MkdirAll(inst.Dir, 0o755); err != nil {
		return err
	}
	if err := pickServicePort(inst.Dir); err != nil {
		return err
	}
	if b := inst.Base; b != nil {
		if b.GuestOS != machine.OS(runtime.GOOS) {
			return fmt.Errorf("fake: a %s host can only fake %s guests, not %s", runtime.GOOS, runtime.GOOS, b.GuestOS)
		}
		if err := os.MkdirAll(rootfs(inst.Dir), 0o755); err != nil {
			return err
		}
		var marker strings.Builder
		fmt.Fprintf(&marker, "os=%s\nbase=%s\nversion=%s\n", b.GuestOS, b.ID, b.Version)
		for _, key := range slices.Sorted(maps.Keys(b.Options)) {
			fmt.Fprintf(&marker, "option.%s=%s\n", key, b.Options[key])
		}
		return os.WriteFile(filepath.Join(rootfs(inst.Dir), markerName), []byte(marker.String()), 0o644) //nolint:gosec // G306: the fake guest's marker, no secrets
	}
	return fsutil.CopyTree(rootfs(inst.Parent().Dir), rootfs(inst.Dir))
}

func (d *Hypervisor) Boot(ctx context.Context, inst local.InstanceSpec, opts local.BootOptions) (local.Machine, error) {
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
	cmd.Env = append(os.Environ(), guest.HostPortsEnv+"="+hostPorts)
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

func (d *Hypervisor) Commit(_ context.Context, inst local.InstanceSpec, dst local.Layer) error {
	if err := os.MkdirAll(dst.Dir, 0o755); err != nil {
		return err
	}
	// Moved, not copied: Commit consumes the instance.
	return os.Rename(rootfs(inst.Dir), rootfs(dst.Dir))
}

func (d *Hypervisor) Destroy(_ context.Context, inst local.InstanceSpec) error {
	return os.RemoveAll(inst.Dir)
}

// Bases is the Catalog the driver was given.
func (d *Hypervisor) Bases(context.Context) ([]machine.Base, error) { return d.Catalog, nil }

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

var _ local.Machine = (*proc)(nil)

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
