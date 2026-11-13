package vz

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
)

// imageGuest is a macOS guest whose daemon runs agentPath itself, as one
// installed before the agent stub did, until an adoptScript exec restarts
// it, which, like launchd's restart, ends the exec's connection before it
// answers.
type imageGuest struct {
	machine.Guest
	mu       sync.Mutex
	dir      string
	uploaded []byte
	mode     int64
	adopted  bool
	probes   int
	// onStub is a guest already on this stub.
	onStub bool
	// stays runs agentPath after the restart still, as a guest whose stub
	// did not take.
	stays bool
	// refuses answers the adoption as the script does when a step fails:
	// exit 1, and the old agent goes on serving.
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

func (g *imageGuest) CopyTo(_ context.Context, dir string, r io.Reader) error {
	tr := tar.NewReader(r)
	h, err := tr.Next()
	if err != nil {
		return err
	}
	data, err := io.ReadAll(tr)
	if err != nil {
		return err
	}
	if h.Name != "disco-vm.stub" {
		return errors.New("unexpected file " + h.Name)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dir, g.uploaded, g.mode = dir, data, h.Mode
	return nil
}

func (g *imageGuest) Exec(_ context.Context, req machine.ExecRequest) (machine.Process, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case slices.Equal(req.Argv, agentProbe.Argv):
		g.probes++
		if g.onStub || g.adopted && !g.stays {
			return exited{out: agentCopy + "\n" + stubMark + "\n"}, nil
		}
		return exited{out: agentPath + "\n\xcf\xfa\xed\xfe\n"}, nil
	case slices.Equal(req.Argv, []string{"/bin/sh", "-c", adoptScript(agentDir)}) && g.uploaded != nil:
		if g.refuses {
			return exited{code: 1, out: "chown: disco-vm.stub: Operation not permitted"}, nil
		}
		g.adopted = true
		return nil, io.ErrUnexpectedEOF
	}
	return nil, errors.New("unexpected exec")
}

func TestAdoptStubPutsAnImageOnTheStub(t *testing.T) {
	g := &imageGuest{}
	if err := adoptStub(t.Context(), g, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if g.dir != agentDir || string(g.uploaded) != agentStub || g.mode != 0o755 || !g.adopted {
		t.Fatalf("uploaded %d bytes to %s, mode %o, adopted %v", len(g.uploaded), g.dir, g.mode, g.adopted)
	}
}

func TestAdoptStubLeavesAGuestOnIt(t *testing.T) {
	g := &imageGuest{onStub: true}
	if err := adoptStub(t.Context(), g, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if g.uploaded != nil || g.adopted || g.probes != 1 {
		t.Fatalf("a guest on the stub: uploaded %d bytes, adopted %v, %d probes", len(g.uploaded), g.adopted, g.probes)
	}
}

func TestAdoptStubStopsWithItsGuest(t *testing.T) {
	stopped := make(chan struct{})
	close(stopped)
	err := adoptStub(t.Context(), &imageGuest{stays: true}, stopped, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("a guest that stopped: %v", err)
	}
}

func TestAdoptStubRefusesAGuestThatDidNotTakeIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := adoptStub(ctx, &imageGuest{stays: true}, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "runs from") {
		t.Fatalf("a guest still on its image's agent: %v", err)
	}
}

func TestAdoptStubReportsAFailedStep(t *testing.T) {
	err := adoptStub(t.Context(), &imageGuest{refuses: true}, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exit 1") || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("a step that failed: %v", err)
	}
}

// fakeBin is a directory of programs a script finds first on its PATH: mount
// prints $FAKE_MOUNT, and the others print their arguments to $FAKE_LOG.
func fakeBin(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mount"), []byte("#!/bin/sh\nprintf '%s\\n' \"$FAKE_MOUNT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\necho "+name+" \"$@\" >> \"$FAKE_LOG\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func agentScript(version string) []byte {
	return []byte("#!/bin/sh\necho agent " + version + " \"$@\"\n")
}

// runStub runs the stub with its disk at dir/disk, mounted as mounts says,
// holding disk (none when nil), and its copy at dir/copy (none when nil).
func runStub(t *testing.T, mounts string, disk, held []byte) (out, errOut, dir string) {
	t.Helper()
	dir = t.TempDir()
	diskDir := filepath.Join(dir, "disk")
	if err := os.Mkdir(diskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if disk != nil {
		if err := os.WriteFile(filepath.Join(diskDir, agentFile), disk, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if held != nil {
		if err := os.WriteFile(filepath.Join(dir, "copy"), held, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mounts = strings.ReplaceAll(mounts, "DISK", diskDir)
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", stubScript(diskDir, filepath.Join(dir, "copy"), 1), "disco-vm", "guest")
	cmd.Env = append(os.Environ(), "PATH="+fakeBin(t)+":"+os.Getenv("PATH"), "FAKE_MOUNT="+mounts)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run()
	return stdout.String(), stderr.String(), dir
}

const isoMount = "/dev/disk4 on DISK (cd9660, local, read-only)"

func TestStubCopiesTheDisksAgentAndRunsIt(t *testing.T) {
	out, errOut, dir := runStub(t, "/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)\n"+isoMount, agentScript("v2"), agentScript("v1"))
	if out != "agent v2 guest\n" || errOut != "" {
		t.Fatalf("the stub ran %q, %q; want the disk's agent", out, errOut)
	}
	info, err := os.Stat(filepath.Join(dir, "copy"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("the copy: %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "copy.new")); !os.IsNotExist(err) {
		t.Fatalf("the stub left copy.new: %v", err)
	}
}

func TestStubLeavesACopyThatIsTheDisks(t *testing.T) {
	out, _, d := runStub(t, isoMount, agentScript("v1"), agentScript("v1"))
	if out != "agent v1 guest\n" {
		t.Fatalf("the stub ran %q", out)
	}
	// Run again on the same copy: an equal one is not rewritten.
	before, err := os.Stat(filepath.Join(d, "copy"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", stubScript(filepath.Join(d, "disk"), filepath.Join(d, "copy"), 1), "disco-vm", "guest")
	cmd.Env = append(os.Environ(), "PATH="+fakeBin(t)+":"+os.Getenv("PATH"), "FAKE_MOUNT="+strings.ReplaceAll(isoMount, "DISK", filepath.Join(d, "disk")))
	if out, err := cmd.Output(); err != nil || string(out) != "agent v1 guest\n" {
		t.Fatalf("the second run: %q, %v", out, err)
	}
	after, err := os.Stat(filepath.Join(d, "copy"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("an equal copy was rewritten")
	}
}

func TestStubRunsItsCopyWithoutTheDisk(t *testing.T) {
	for name, mounts := range map[string]string{
		"no disk":             "/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)",
		"not ISO 9660":        "/dev/disk4s1 on DISK (msdos, local, read-only)",
		"writable":            "/dev/disk4 on DISK (cd9660, local)",
		"another of the name": "/dev/disk4 on DISK 1 (cd9660, local, read-only)",
	} {
		t.Run(name, func(t *testing.T) {
			out, errOut, _ := runStub(t, mounts, agentScript("v2"), agentScript("v1"))
			if out != "agent v1 guest\n" || !strings.Contains(errOut, "no agent disk") {
				t.Fatalf("the stub ran %q, %q; want its copy", out, errOut)
			}
		})
	}
}

// runAdopt runs adoptScript in a directory holding the daemon's program,
// with chown and launchctl faked.
func runAdopt(t *testing.T, program, held []byte) (dir, log string) {
	t.Helper()
	dir = t.TempDir()
	base := filepath.Base(agentPath)
	for name, data := range map[string][]byte{base: program, base + ".stub": []byte(agentStub), filepath.Base(agentCopy): held} {
		if data == nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(t.TempDir(), "log")
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", adoptScript(dir))
	cmd.Env = append(os.Environ(), "PATH="+fakeBin(t, "chown", "launchctl")+":"+os.Getenv("PATH"), "FAKE_LOG="+logPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("adoptScript: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(logPath)
	return dir, string(data)
}

func TestAdoptScriptKeepsAnImagesAgentAsTheCopy(t *testing.T) {
	agent := []byte("\xcf\xfa\xed\xfe an image's agent")
	dir, log := runAdopt(t, agent, nil)
	if got, _ := os.ReadFile(filepath.Join(dir, filepath.Base(agentCopy))); !bytes.Equal(got, agent) {
		t.Fatalf("the copy is %q, not the image's agent", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, filepath.Base(agentPath))); string(got) != agentStub {
		t.Fatal("the daemon's program is not the stub")
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.Base(agentPath)+".stub")); !os.IsNotExist(err) {
		t.Fatalf("the upload was left: %v", err)
	}
	if !strings.Contains(log, "chown root:wheel disco-vm.stub") || !strings.Contains(log, "launchctl kill SIGKILL system/"+daemonID) {
		t.Fatalf("the script ran %q", log)
	}
}

func TestAdoptScriptReplacesAnOlderStubAndKeepsItsCopy(t *testing.T) {
	held := []byte("\xcf\xfa\xed\xfe the copy")
	dir, _ := runAdopt(t, []byte("#!/bin/sh\n# disco-vm agent stub 0\n"), held)
	if got, _ := os.ReadFile(filepath.Join(dir, filepath.Base(agentCopy))); !bytes.Equal(got, held) {
		t.Fatalf("the copy is %q; an older stub took its place", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, filepath.Base(agentPath))); string(got) != agentStub {
		t.Fatal("the daemon's program is not the stub")
	}
}

// The install script parses, the stub inside it whole.
func TestInstallScriptsParse(t *testing.T) {
	for name, script := range map[string]string{"stub": agentStub, "adopt": adoptScript(agentDir)} {
		if out, err := exec.CommandContext(t.Context(), "/bin/sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
	}
	if !strings.HasPrefix(agentStub, "#!/bin/sh\n"+stubMark+"\n") {
		t.Fatal("the stub does not open with its mark on its second line")
	}
}

func agentFileOnHost(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disco-vm")
	if err := os.WriteFile(p, []byte("this host's disco-vm"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckAgentBinaryRefusesWhatIsNotDiscoVM(t *testing.T) {
	if err := checkAgentBinary(agentFileOnHost(t)); err == nil {
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
