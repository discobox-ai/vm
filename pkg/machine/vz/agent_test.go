package vz

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// olderAgent is a macOS guest whose agent speaks the protocol before this
// host's until a moveScript exec restarts it, which, like launchd's restart,
// ends the exec's connection before it answers.
type olderAgent struct {
	machine.Guest
	mu       sync.Mutex
	dir      string
	uploaded []byte
	mode     int64
	moved    bool
	// stays keeps the older protocol after the move, as an agent whose
	// replacement did not take.
	stays bool
	// refuses answers the move as the script does when the uploaded binary
	// is not an agent of the protocol asked for: exit 3, nothing touched.
	refuses bool
}

// exited is a process that has already exited with code, having written out.
type exited struct {
	machine.Process
	code int
	out  string
}

func (p exited) Wait(stdout, _ io.Writer) (int, error) {
	_, _ = io.WriteString(stdout, p.out)
	return p.code, nil
}

func (exited) CloseStdin() error { return nil }
func (exited) Close() error      { return nil }

func (g *olderAgent) Info(context.Context) (machine.GuestInfo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.moved && !g.stays {
		return machine.GuestInfo{OS: "darwin", Version: "new", Protocol: guest.Protocol}, nil
	}
	return machine.GuestInfo{OS: "darwin", Version: "old", Protocol: guest.Protocol - 1}, nil
}

func (g *olderAgent) CopyTo(_ context.Context, dir string, r io.Reader) error {
	tr := tar.NewReader(r)
	h, err := tr.Next()
	if err != nil {
		return err
	}
	data, err := io.ReadAll(tr)
	if err != nil {
		return err
	}
	if h.Name != "disco-vm.new" {
		return errors.New("unexpected file " + h.Name)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dir, g.uploaded, g.mode = dir, data, h.Mode
	return nil
}

func (g *olderAgent) Exec(_ context.Context, req machine.ExecRequest) (machine.Process, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !slices.Equal(req.Argv, moveRequest().Argv) || g.uploaded == nil {
		return nil, errors.New("unexpected exec")
	}
	if g.refuses {
		return exited{code: 3, out: "the uploaded agent does not speak protocol 1: not an agent"}, nil
	}
	g.moved = true
	return nil, io.ErrUnexpectedEOF
}

func agentFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disco-vm")
	if err := os.WriteFile(p, []byte("this host's disco-vm"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMoveAgentMovesAnOlderOne(t *testing.T) {
	g := &olderAgent{}
	var log bytes.Buffer
	if err := moveAgent(t.Context(), g, agentFile(t), nil, &log); err != nil {
		t.Fatal(err)
	}
	if g.dir != "/usr/local/libexec" || string(g.uploaded) != "this host's disco-vm" || g.mode != 0o755 {
		t.Fatalf("uploaded %q (mode %o) to %s", g.uploaded, g.mode, g.dir)
	}
	if !strings.Contains(log.String(), fmt.Sprintf("speaks protocol %d;", guest.Protocol-1)) || !strings.Contains(log.String(), fmt.Sprintf("protocol %d\n", guest.Protocol)) {
		t.Fatalf("log:\n%s", log.String())
	}
}

func TestMoveAgentLeavesACurrentOne(t *testing.T) {
	g := &olderAgent{moved: true}
	if err := moveAgent(t.Context(), g, filepath.Join(t.TempDir(), "absent"), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if g.uploaded != nil {
		t.Fatal("a current agent was replaced")
	}
}

func TestMoveAgentStopsWithItsGuest(t *testing.T) {
	g := &olderAgent{stays: true}
	stopped := make(chan struct{})
	close(stopped)
	err := moveAgent(t.Context(), g, agentFile(t), stopped, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("a guest that stopped mid-move: %v", err)
	}
}

func TestMoveAgentRefusesOneThatDidNotMove(t *testing.T) {
	g := &olderAgent{stays: true}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := moveAgent(ctx, g, agentFile(t), nil, io.Discard)
	if err == nil {
		t.Fatal("an agent that still speaks the older protocol was accepted")
	}
}

// A binary that is not an agent of this host's protocol is refused in the
// guest before it replaces anything, and the boot fails saying so.
func TestMoveAgentRefusesABinaryThatIsNotAnAgent(t *testing.T) {
	g := &olderAgent{refuses: true}
	err := moveAgent(t.Context(), g, agentFile(t), nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exit 3") || !strings.Contains(err.Error(), "not an agent") {
		t.Fatalf("a binary the guest refused: %v", err)
	}
	if g.moved {
		t.Fatal("the agent was moved")
	}
}

// The script the guest runs checks the uploaded agent before it touches the
// running one, and keeps the running one.
func TestMoveScriptChecksBeforeItReplaces(t *testing.T) {
	check := strings.Index(moveScript, "guest --protocol")
	keep := strings.Index(moveScript, ".old")
	replace := strings.Index(moveScript, `mv -f "$new"`)
	kill := strings.Index(moveScript, "launchctl kill")
	if check < 0 || keep < check || replace < keep || kill < replace {
		t.Fatalf("moveScript does not check, keep, replace, and kill in that order:\n%s", moveScript)
	}
}

// A binary that is not disco-vm is refused on the host, before anything
// reaches the guest: a file that is no Go binary, and this test binary.
func TestCheckAgentBinaryRefusesWhatIsNotDiscoVM(t *testing.T) {
	if err := checkAgentBinary(agentFile(t)); err == nil {
		t.Fatal("a file that is no Go binary was taken for disco-vm")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := checkAgentBinary(self); err == nil || !strings.Contains(err.Error(), "not disco-vm") {
		t.Fatalf("the test binary: %v", err)
	}
}

// disco-vm itself is taken: a build of cmd/disco-vm, so a change to the main
// package's path fails here, on every OS CI runs, and not first at a boot.
func TestCheckAgentBinaryTakesDiscoVM(t *testing.T) {
	if testing.Short() {
		t.Skip("builds cmd/disco-vm")
	}
	bin := filepath.Join(t.TempDir(), "disco-vm")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "../../../cmd/disco-vm")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build disco-vm: %v\n%s", err, out)
	}
	if err := checkAgentBinary(bin); err != nil {
		t.Fatal(err)
	}
}
