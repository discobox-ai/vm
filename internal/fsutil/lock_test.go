package fsutil_test

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
)

// holderEnv names the file the test binary, run as a holder, locks before it
// says so on stdout and waits to be killed.
const holderEnv = "FSUTIL_TEST_HOLD"

func TestMain(m *testing.M) {
	if path := os.Getenv(holderEnv); path != "" {
		if _, err := fsutil.TryLock(path); err != nil {
			os.Stdout.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("held\n")
		select {}
	}
	os.Exit(m.Run())
}

// A lock is held against another open of the file in this process and in
// another, until it is released or its holder dies however it dies.
func TestTryLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "lock")
	release, err := fsutil.TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsutil.TryLock(path); !errors.Is(err, fsutil.ErrLocked) {
		t.Fatalf("a second lock in this process: %v, want ErrLocked", err)
	}
	release()
	again, err := fsutil.TryLock(path)
	if err != nil {
		t.Fatalf("a released lock: %v", err)
	}
	again()

	holder := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$") //nolint:gosec // G204: this test binary
	holder.Env = append(os.Environ(), holderEnv+"="+path)
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "held\n" {
		t.Fatalf("the holder said %q: %v", line, err)
	}
	if _, err := fsutil.TryLock(path); !errors.Is(err, fsutil.ErrLocked) {
		t.Fatalf("a lock another process holds: %v, want ErrLocked", err)
	}
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for {
		release, err := fsutil.TryLock(path)
		if err == nil {
			release()
			break
		}
		if !errors.Is(err, fsutil.ErrLocked) || time.Now().After(deadline) {
			t.Fatalf("a lock whose holder was killed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
