package local_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/internal/proc"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// A start that gives up before its shim comes up, as one whose boot outlasts
// the caller's deadline does, leaves no shim behind it, and no guest holding
// a slot under an instance that reads as stopped.
func TestAStartThatGivesUpLeavesNoShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the slow shim is a shell script")
	}
	ctx := context.Background()
	c := capped{t: t, root: t.TempDir(), agent: buildAgent(t), limit: 1}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	script := filepath.Join(dir, "slow-shim")
	// The shim takes two seconds to come up, as a macOS guest's boot can
	// outlast a --timeout; exec keeps the PID the script wrote.
	body := "#!/bin/sh\necho $$ > " + pidFile + "\nsleep 2\nexec " + c.agent + " --root " + c.root +
		" --driver fake --pool a --driver-config '" + c.config() + "' shim \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: the script is run
		t.Fatal(err)
	}
	slow, err := local.New(machine.Config{
		Dir: c.root, Pool: "a",
		ShimCommand:  []string{script},
		DriverConfig: json.RawMessage(c.config()),
	}, &fake.Hypervisor{Agent: c.agent, BaseName: "stock", MaxRunning: map[machine.OS]int{machine.OS(runtime.GOOS): 1}})
	if err != nil {
		t.Fatal(err)
	}
	one := c.create(slow, "one")
	other := c.driver("b")
	two := c.create(other, "two")

	startCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if _, err := slow.Start(startCtx, one, machine.BootOptions{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a start past its deadline: %v, want DeadlineExceeded", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	})

	// Long enough for a shim left behind to have come up and booted.
	time.Sleep(5 * time.Second)
	if proc.Alive(pid) {
		t.Errorf("the shim of a start that gave up (pid %d) is still running", pid)
	}
	m, err := other.Boot(ctx, two, machine.BootOptions{})
	if err != nil {
		t.Fatalf("a boot after a start that gave up, at a cap of one: %v, want its slot free", err)
	}
	kill(t, m)
}

// A VM whose boot holds it, as a shim still booting does before it writes
// shim.json, is not removed from under it; once its machine stops it is.
func TestDestroyLeavesABootThatHoldsTheVM(t *testing.T) {
	ctx := context.Background()
	c := capped{t: t, root: t.TempDir(), agent: buildAgent(t), limit: 1}
	d := c.driver("a")
	inst := c.create(d, "one")
	m, err := d.Boot(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Destroy(ctx, inst); err == nil || !strings.Contains(err.Error(), "is being booted") {
		t.Errorf("a removal of a VM a boot holds: %v, want it refused", err)
	}
	if _, err := d.Instance(ctx, "one"); err != nil {
		t.Errorf("the VM after its removal was refused: %v", err)
	}
	kill(t, m)
	if err := d.Destroy(ctx, inst); err != nil {
		t.Fatalf("a removal once its machine stopped: %v", err)
	}
	if _, err := d.Instance(ctx, "one"); !errors.Is(err, machine.ErrNotFound) {
		t.Errorf("the VM once removed: %v, want ErrNotFound", err)
	}
}

// hanging is a root whose fake guests never answer: the agent its driver,
// and every shim of it, boots is a script that writes its PID and sleeps,
// so a boot waits on it until its context ends.
type hanging struct {
	capped
	pidFile string
	config  string
}

func newHanging(t *testing.T) hanging {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the hanging guest is a shell script")
	}
	h := hanging{t: t, root: t.TempDir(), agent: buildAgent(t), limit: 1}
	dir := t.TempDir()
	h.pidFile = filepath.Join(dir, "pid")
	script := filepath.Join(dir, "hanging-guest")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho $$ > "+h.pidFile+"\nexec sleep 60\n"), 0o755); err != nil { //nolint:gosec // G306: the script is run
		t.Fatal(err)
	}
	h.config = fmt.Sprintf(`{"agent":%q,"maxRunning":1}`, script)
	return h
}

func (h hanging) driver() *local.Driver {
	h.t.Helper()
	d, err := local.New(machine.Config{
		Dir: h.root, Pool: "a",
		ShimCommand:  []string{h.agent, "--root", h.root, "--driver", "fake", "--pool", "a", "--driver-config", h.config, "shim"},
		DriverConfig: json.RawMessage(h.config),
	}, &fake.Hypervisor{Agent: h.agent, BaseName: "stock", MaxRunning: map[machine.OS]int{machine.OS(runtime.GOOS): 1}})
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

// guestPID waits for the hanging guest to have been started, and returns its
// PID.
func (h hanging) guestPID() int {
	h.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if data, err := os.ReadFile(h.pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				h.t.Cleanup(func() {
					if p, err := os.FindProcess(pid); err == nil {
						_ = p.Kill()
					}
				})
				return pid
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatal("the guest was never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A start that gives up while its shim is booting has the shim cancel the
// boot: the guest it started is gone, and its slot free, by the time Start
// returns.
func TestAStartThatGivesUpCancelsItsBoot(t *testing.T) {
	ctx := context.Background()
	h := newHanging(t)
	d := h.driver()
	one := h.create(d, "one")
	two := h.create(h.capped.driver("b"), "two")

	startCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	started := make(chan error, 1)
	go func() {
		_, err := d.Start(startCtx, one, machine.BootOptions{})
		started <- err
	}()
	pid := h.guestPID()
	canceled := time.Now()
	cancel()
	if err := <-started; !errors.Is(err, context.Canceled) {
		t.Fatalf("a start canceled during its boot: %v, want Canceled", err)
	}
	// Well inside the fake's own fifteen seconds for a guest to listen,
	// which would end the boot anyway.
	if took := time.Since(canceled); took > 10*time.Second {
		t.Errorf("a start canceled during its boot returned after %s, want its shim to give the boot up at once", took)
	}
	if proc.Alive(pid) {
		t.Errorf("the guest of a start that gave up (pid %d) is still running", pid)
	}
	m, err := h.capped.driver("b").Boot(ctx, two, machine.BootOptions{})
	if err != nil {
		t.Fatalf("a boot once a start gave up, at a cap of one: %v, want its slot free", err)
	}
	kill(t, m)
}

// A VM a shim is still booting is not removed from under it; once its start
// gives up, it is.
func TestDestroyLeavesAShimStillBooting(t *testing.T) {
	ctx := context.Background()
	h := newHanging(t)
	d := h.driver()
	inst := h.create(d, "one")

	startCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	started := make(chan error, 1)
	go func() {
		_, err := d.Start(startCtx, inst, machine.BootOptions{})
		started <- err
	}()
	h.guestPID()
	// The shim admitted its boot before it started the guest.
	if unlock, err := fsutil.TryLock(filepath.Join(h.root, "instances", inst.ID, "boot.lock")); err == nil {
		unlock()
		t.Fatal("a shim booting holds no boot lock")
	}
	if err := d.Destroy(ctx, inst); err == nil || !strings.Contains(err.Error(), "is being booted") {
		t.Errorf("a removal of a VM its shim is booting: %v, want it refused", err)
	}
	cancel()
	<-started
	if err := d.Destroy(ctx, inst); err != nil {
		t.Fatalf("a removal once its start gave up: %v", err)
	}
}

// spawnShim starts a shim of the instance as Start does, its stdin a pipe
// whose write end it returns, and waits for it to serve the instance. The
// channel is closed once the shim has exited.
func spawnShim(t *testing.T, c capped, d *local.Driver, inst machine.Instance) (<-chan struct{}, *os.File) {
	t.Helper()
	stdin, lifeline, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	cmd := exec.Command(c.agent, "--root", c.root, "--driver", "fake", "--pool", "a", "--driver-config", c.config(), "shim", inst.ID) //nolint:gosec,noctx // G204: the test's own build
	cmd.Stdin = stdin
	cmd.Env = append(os.Environ(), "DISCO_VM_SHIM_STARTER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := d.Attach(context.Background(), inst); err == nil {
			return exited, lifeline
		}
		if time.Now().After(deadline) {
			t.Fatal("the shim never served the instance")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A shim whose lifeline closes unanswered once it has booted, as when its
// start gives up between the boot and attaching it, kills the machine and
// exits; one whose start answered holds the machine whatever becomes of the
// pipe.
func TestAShimHoldsItsMachineOnlyOnceStarted(t *testing.T) {
	ctx := context.Background()
	c := capped{t: t, root: t.TempDir(), agent: buildAgent(t), limit: 1}
	d := c.driver("a")
	one := c.create(d, "one")
	two := c.create(c.driver("b"), "two")

	exited, lifeline := spawnShim(t, c, d, one)
	_ = lifeline.Close()
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		t.Fatal("a shim whose start gave up once it had booted is still running")
	}
	m, err := c.driver("b").Boot(ctx, two, machine.BootOptions{})
	if err != nil {
		t.Fatalf("a boot once a booted shim's start gave up, at a cap of one: %v, want its slot free", err)
	}
	kill(t, m)

	_, lifeline = spawnShim(t, c, d, one)
	if _, err := lifeline.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = lifeline.Close()
	time.Sleep(time.Second)
	m, err = d.Attach(ctx, one)
	if err != nil {
		t.Fatalf("a started shim once its pipe closed: %v, want it running", err)
	}
	kill(t, m)
}
