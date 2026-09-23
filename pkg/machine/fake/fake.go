// Package fake is a driver with no hypervisor. A "guest" is a disco-vm guest
// agent process on the host, confined to a directory that stands in for its
// disk; clones and commits are directory copies.
//
// A warm fake image is a pool of pre-copied roots, and a resume clone takes one
// by renaming it into place instead of copying the image. There is no memory
// to save, so that is all resume means here, but it has vz's shape: a stage on
// disk, used once per clone, topped up by warming again.
//
// It exists so that everything above the driver seam — the image store, the
// builder, the engine and its shim, the guest protocol, the CLI — runs exactly
// as it does against a real VM, on any machine and in CI, while the hcs and vz
// drivers are written. Its guest OS is always the host's.
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

func init() {
	machine.Register("fake", func() (machine.Driver, error) { return New() })
}

// AgentEnv overrides the agent binary; by default it is the running disco-vm.
const AgentEnv = "DISCO_VM_AGENT"

// Driver is the fake driver.
type Driver struct {
	// Agent is the disco-vm binary each guest runs as `disco-vm guest`.
	Agent string
}

var (
	_ machine.Driver = (*Driver)(nil)
	_ machine.Warmer = (*Driver)(nil)
)

// New returns a fake driver whose guests run this binary, or $DISCO_VM_AGENT.
func New() (*Driver, error) {
	agent := os.Getenv(AgentEnv)
	if agent == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		agent = exe
	}
	return &Driver{Agent: agent}, nil
}

func (*Driver) Name() string { return "fake" }

func (*Driver) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS:    []machine.OS{machine.OS(runtime.GOOS)},
		CloneModes: []machine.CloneMode{machine.Cold, machine.Resume},
	}
}

func (*Driver) Check(context.Context) error { return nil }

const (
	rootfsName = "rootfs"
	stagesName = "stages"
	addrName   = "agent.addr"
	markerName = ".disco-vm-install"
)

func rootfs(dir string) string { return filepath.Join(dir, rootfsName) }

// Install creates an empty root. The media is not read; it is recorded, so a
// test can see what the spec asked for.
func (d *Driver) Install(_ context.Context, spec machine.InstallSpec, dst machine.Layer) error {
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
	return os.WriteFile(filepath.Join(rootfs(dst.Dir), markerName), []byte(marker), 0o644)
}

func (d *Driver) Prepare(_ context.Context, inst machine.InstanceSpec) error {
	if len(inst.Chain) == 0 {
		return errors.New("fake: instance has no image")
	}
	if err := os.MkdirAll(inst.Dir, 0o755); err != nil {
		return err
	}
	switch inst.Mode {
	case "", machine.Cold:
		return fsutil.CopyTree(rootfs(inst.Parent().Dir), rootfs(inst.Dir))
	case machine.Resume:
		// Take any staged root. A rename is atomic, so two clones racing for
		// the last one cannot both win.
		stages, _ := staged(inst.WarmDir)
		for _, stage := range stages {
			if err := os.Rename(stage, rootfs(inst.Dir)); err == nil {
				return nil
			}
		}
		return fmt.Errorf("fake: %w", machine.ErrNotWarm)
	default:
		return fmt.Errorf("fake: clone mode %q: %w", inst.Mode, machine.ErrUnsupported)
	}
}

// Warm copies the image's root until Count are staged.
func (d *Driver) Warm(_ context.Context, spec machine.WarmSpec) (machine.Stage, error) {
	if len(spec.Chain) == 0 {
		return nil, errors.New("fake: warm: no image")
	}
	stages, err := staged(spec.Dir)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(spec.Dir, stagesName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for n := len(stages); n < max(spec.Count, 1); n++ {
		// Copied under a temporary name and renamed, so a half-copied root is
		// never taken.
		tmp := filepath.Join(spec.Dir, "tmp-"+fsutil.RandomHex(6))
		if err := fsutil.CopyTree(rootfs(spec.Chain[len(spec.Chain)-1].Dir), tmp); err != nil {
			_ = os.RemoveAll(tmp)
			return nil, err
		}
		if err := os.Rename(tmp, filepath.Join(dir, fsutil.RandomHex(6))); err != nil {
			_ = os.RemoveAll(tmp)
			return nil, err
		}
		if spec.Log != nil {
			fmt.Fprintf(spec.Log, "fake: staged resume clone %d of %d\n", n+1, spec.Count)
		}
	}
	return nil, nil
}

func (d *Driver) Warmth(_ context.Context, spec machine.WarmSpec) (machine.Warmth, error) {
	stages, err := staged(spec.Dir)
	if err != nil || len(stages) == 0 {
		return machine.Warmth{Mode: machine.Cold}, err
	}
	return machine.Warmth{Mode: machine.Resume, Clones: len(stages)}, nil
}

func (d *Driver) Cool(_ context.Context, spec machine.WarmSpec) error {
	return os.RemoveAll(spec.Dir)
}

// staged lists the staged roots under a warm directory.
func staged(warmDir string) ([]string, error) {
	if warmDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(filepath.Join(warmDir, stagesName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		out = append(out, filepath.Join(warmDir, stagesName, entry.Name()))
	}
	return out, nil
}

func (d *Driver) Boot(ctx context.Context, inst machine.InstanceSpec, opts machine.BootOptions) (machine.Machine, error) {
	addrFile := filepath.Join(inst.Dir, addrName)
	_ = os.Remove(addrFile)
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
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("fake: start guest agent %s: %w", d.Agent, err)
	}
	m := &proc{cmd: cmd, done: make(chan struct{})}
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
	for {
		if data, err := os.ReadFile(addrFile); err == nil && strings.TrimSpace(string(data)) != "" {
			m.addr = strings.TrimSpace(string(data))
			return m, nil
		}
		select {
		case <-m.done:
			return nil, fmt.Errorf("fake: guest agent exited during boot: %v", m.err)
		case <-ctx.Done():
			_ = m.Kill(context.Background())
			return nil, ctx.Err()
		case <-deadline.C:
			_ = m.Kill(context.Background())
			return nil, errors.New("fake: guest agent did not start listening")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (d *Driver) Commit(_ context.Context, inst machine.InstanceSpec, dst machine.Layer) error {
	if err := os.MkdirAll(dst.Dir, 0o755); err != nil {
		return err
	}
	// Moved, not copied: Commit consumes the instance.
	return os.Rename(rootfs(inst.Dir), rootfs(dst.Dir))
}

func (d *Driver) Destroy(_ context.Context, inst machine.InstanceSpec) error {
	return os.RemoveAll(inst.Dir)
}

func (d *Driver) DeleteLayer(context.Context, machine.Layer) error { return nil }

// proc is a running fake guest.
type proc struct {
	cmd    *exec.Cmd
	addr   string
	done   chan struct{}
	err    error
	once   sync.Once
	killed atomic.Bool
}

func (m *proc) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	if port != guest.AgentPort {
		return nil, fmt.Errorf("fake: port %d: only the agent port %d exists", port, guest.AgentPort)
	}
	select {
	case <-m.done:
		return nil, errors.New("fake: guest is stopped")
	default:
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", m.addr)
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
