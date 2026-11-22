package archil

import (
	"cmp"
	"context"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
)

// ImageEnv names the OCI image the conformance suite runs on Archil as the
// driver's base (machine.Config.Base), such as debian:sid.
const ImageEnv = "DISCO_VM_ARCHIL_IMAGE"

// TestConformance runs the conformance suite against Archil itself. It needs
// ARCHIL_API_KEY, ARCHIL_REGION, and ImageEnv, so it runs only where all are
// set; it makes sandboxes and disks in a pool of its own and removes them.
func TestConformance(t *testing.T) {
	image := os.Getenv(ImageEnv)
	if image == "" || os.Getenv(APIKeyEnv) == "" || os.Getenv(RegionEnv) == "" {
		t.Skipf("set %s, %s (such as aws-us-east-1), and %s (an image, such as debian:sid) to run the conformance suite against Archil", APIKeyEnv, RegionEnv, ImageEnv)
	}
	config, err := machine.ConfigFromValues("archil", machine.ConfigFromEnv("archil"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := machine.New("archil", machine.Config{Pool: "conformance", Base: image, Keys: machine.KeyFromEnv("archil"), DriverConfig: config})
	if err != nil {
		t.Fatal(err)
	}
	machinetest.Run(t, d, machinetest.Config{
		CPUs:        2,
		Memory:      2 << 30,
		Timeout:     5 * time.Minute,
		ScratchDir:  "/var/lib",
		Credentials: d.(*Driver).honoredTokens,
		Pause:       d.(*Driver).pause,
	})
}

// pause pauses a VM's sandbox as Archil does at its session's limit, by
// Archil's own pause (POST .../pause), which answers 202 and goes through
// pausing.
func (d *Driver) pause(ctx context.Context, inst machine.Instance) error {
	return d.api.call(ctx, http.MethodPost, "/api/sandboxes/"+url.PathEscape(inst.ID)+"/pause", nil, nil, nil)
}

// honoredTokens lists the port tokens Archil still honors for a VM's
// sandbox, by ID: none for a sandbox gone, since Archil lists no tokens of
// a sandbox it deleted. After a commit, of a sandbox still there, it tells;
// after a destroy, the fake's own count, which outlives the sandbox, does.
func (d *Driver) honoredTokens(ctx context.Context, inst machine.Instance) ([]string, error) {
	tokens, err := d.api.portTokens(ctx, inst.ID)
	if isNotFound(err) {
		return nil, nil
	}
	var ids []string
	for _, t := range tokens {
		ids = append(ids, t.ID)
	}
	return ids, err
}

// TestConformanceFakeAPI runs the conformance suite against the fake Archil
// (fakeAPI), whose every listing pages two at a time, and finds nothing left
// behind in it.
func TestConformanceFakeAPI(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	// Registered first, so it runs once the suite's own cleanups have.
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, s := range f.sandboxes {
			t.Errorf("sandbox %s (%s) was left behind", s.name, s.status)
		}
		for _, d := range f.disks {
			t.Errorf("disk %s was left behind", d.name)
		}
		for _, tok := range f.tokens {
			if tok.honored() {
				t.Errorf("a port token of sandbox %s was left honored", tok.sandbox)
			}
		}
	})
	d := f.driver(t, machine.DefaultPool, "debian:sid")
	machinetest.Run(t, d, machinetest.Config{
		// Not Archil's default size, so a size not kept shows.
		CPUs:       3,
		Memory:     3 << 30,
		Timeout:    30 * time.Second,
		ScratchDir: "/var/lib",
		URLClient:  f.gatewayClient(),
		// Its tokens as the fake keeps them, past their sandbox's delete.
		Credentials: func(_ context.Context, inst machine.Instance) ([]string, error) {
			return f.honoredTokens(inst.ID), nil
		},
		SkipGuestDNS: "the fake Archil's guest is a process on this host, on its network",
		Pause:        d.pause,
		// The fake's guest is a host process, on the host's clock.
		PauseHold: 2 * time.Second,
	})
}

// TestFreshVMsUseTheConfiguredImage: a fresh VM is a sandbox of the driver's
// base, or of Archil's default image when it has none, and a VM made from
// another carries the other's.
func TestFreshVMsUseTheConfiguredImage(t *testing.T) {
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	for _, c := range []struct{ base, want string }{{"", fakeDefaultImage}, {"debian:sid", "debian:sid"}} {
		d := f.driver(t, "images", c.base)
		inst, err := d.Create(ctx, machine.InstanceSpec{Name: "fresh"})
		if err != nil {
			t.Fatal(err)
		}
		copied, err := d.Create(ctx, machine.InstanceSpec{Name: "copy", From: &inst})
		if err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		for _, s := range f.sandboxes {
			if s.image != c.want {
				t.Errorf("base %q: sandbox %s runs %s, want %s", c.base, s.name, s.image, c.want)
			}
		}
		f.mu.Unlock()
		for _, inst := range []machine.Instance{copied, inst} {
			if err := d.Destroy(ctx, inst); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestTerminal: an exec with a TTY starts its process with the request's
// terminal, or 24 rows and 80 columns when it names none, and a resize
// reaches the process API naming that process.
func TestTerminal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "tty", "")
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "tty"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	m, err := d.Start(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []machine.ExecRequest{
		{Argv: []string{"cat"}, TTY: true, Rows: 45, Cols: 123},
		{Argv: []string{"cat"}, TTY: true},
	} {
		p, err := m.Exec(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Resize(50, 100); err != nil {
			t.Fatal(err)
		}
		if err := p.CloseStdin(); err != nil {
			t.Fatal(err)
		}
		if code, err := p.Wait(nil, nil); err != nil || code != 0 {
			t.Fatalf("exit %d, %v", code, err)
		}
		// The fake resizes only the process a resize names, which is gone.
		if err := p.Resize(1, 1); err == nil {
			t.Error("a resize of a process that exited was answered")
		}
		_ = p.Close()
	}
	f.mu.Lock()
	got := f.terminals
	f.mu.Unlock()
	want := [][2]uint16{{45, 123}, {50, 100}, {24, 80}, {50, 100}}
	if len(got) != len(want) {
		t.Fatalf("terminals %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("terminals %v, want %v", got, want)
		}
	}
	if err := m.Kill(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestCommandLinesAreWhatShRuns: every line the driver makes, as sh reads it
// in the fake, enters systemd's namespaces with the request's directory
// ("/" for none), its account for another, and its argv, whatever a word
// holds.
func TestCommandLinesAreWhatShRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	argv := []string{"printf", "%s\n", "it's", "two words", "", "$HOME", "`id`", "a\\b", "line\nbreak", "--", "*"}
	for _, user := range []string{"", "disco", "o'neil"} {
		for _, dir := range []string{"", "/a dir/it's"} {
			gotDir, gotUser, got, entered, err := parseLine(command(machine.ExecRequest{Argv: argv, User: user, Dir: dir}))
			if err != nil {
				t.Fatal(err)
			}
			if !entered || gotDir != cmp.Or(dir, "/") || gotUser != user || !slices.Equal(got, argv) {
				t.Errorf("as %q in %q: sh read %q in %q as %q (entered %v), want %q", user, dir, got, gotDir, gotUser, entered, argv)
			}
		}
	}
}

// TestStateReadsPausedAndWakesNothing: a sandbox Archil paused, or is
// pausing, reads as paused, one running as running, and any other as
// stopped, and reading it leaves it as it was.
func TestStateReadsPausedAndWakesNothing(t *testing.T) {
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "states", "debian:sid")
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "vm"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	for status, want := range map[string]machine.State{
		"running": machine.StateRunning, "paused": machine.StatePaused, "pausing": machine.StatePaused,
		"pending": machine.StateStopped, "stopping": machine.StateStopped, "stopped": machine.StateStopped,
	} {
		f.mu.Lock()
		f.sandboxes[0].status = status
		f.mu.Unlock()
		if got, err := d.State(ctx, inst); err != nil || got != want {
			t.Errorf("a sandbox %s: state %q, %v; want %q", status, got, err, want)
		}
		f.mu.Lock()
		after := f.sandboxes[0].status
		f.mu.Unlock()
		if after != status {
			t.Errorf("a sandbox %s is %s once its state is read", status, after)
		}
	}
	f.mu.Lock()
	f.sandboxes[0].status = "stopped"
	f.mu.Unlock()
}

// TestPauseWaitsOutPausingAndReturnsARefusal: a pause of a sandbox Archil is
// still pausing waits until it is paused, and one Archil refuses while the
// sandbox runs is that refusal at once, the sandbox still running.
func TestPauseWaitsOutPausingAndReturnsARefusal(t *testing.T) {
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "pauses", "debian:sid")
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "vm"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	setStatus := func(status string) {
		f.mu.Lock()
		f.sandboxes[0].status = status
		f.mu.Unlock()
	}
	m := newMachine(d.api, f.sandboxes[0].id, inst.Name)

	setStatus("running")
	f.mu.Lock()
	f.refusePause = true
	f.mu.Unlock()
	refusedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := m.Pause(refusedCtx); err == nil || !isConflict(err) {
		t.Fatalf("a pause Archil refuses: %v, want its conflict", err)
	}
	if got, err := d.State(ctx, inst); err != nil || got != machine.StateRunning {
		t.Fatalf("a sandbox refused a pause: %q, %v; want running", got, err)
	}
	f.mu.Lock()
	f.refusePause = false
	f.mu.Unlock()

	setStatus("pausing")
	go func() {
		time.Sleep(1500 * time.Millisecond)
		setStatus("paused")
	}()
	if err := m.Pause(ctx); err != nil {
		t.Fatalf("a pause of a sandbox pausing: %v", err)
	}
	if got, err := d.State(ctx, inst); err != nil || got != machine.StatePaused {
		t.Fatalf("a sandbox paused once it was pausing: %q, %v; want paused", got, err)
	}
	setStatus("stopped")
}
