//go:build darwin

package vz

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestAgentDriveOnMacOS has macOS's own cd9660 read the agent's drive back:
// attached read-only, the agent and its digest are there by their Rock
// Ridge names, the agent executable, under the drive's label.
func TestAgentDriveOnMacOS(t *testing.T) {
	agent := filepath.Join(t.TempDir(), "disco-vm")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\necho agent\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	iso, err := agentDrive(t.TempDir(), agent)
	if err != nil {
		t.Fatal(err)
	}
	// mount names the directory as it is, under /private.
	mnt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "hdiutil", "attach", "-readonly", "-nobrowse", "-mountpoint", mnt, iso).CombinedOutput(); err != nil {
		t.Fatalf("hdiutil attach: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("hdiutil", "detach", "-force", mnt).Run() })
	got, err := os.ReadFile(filepath.Join(mnt, driveAgent))
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := os.ReadFile(agent); !bytes.Equal(got, want) {
		t.Fatalf("the agent on the drive is %q", got)
	}
	if info, err := os.Stat(filepath.Join(mnt, driveAgent)); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("the agent on the drive: %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(mnt, driveDigest)); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(ctx, "mount").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(" on "+mnt+" (cd9660, ")) {
		t.Fatalf("the drive is not mounted as cd9660 at %s:\n%s", mnt, out)
	}
	if out, err := exec.CommandContext(ctx, "diskutil", "info", mnt).CombinedOutput(); err != nil || !bytes.Contains(out, []byte(driveLabel)) {
		t.Fatalf("diskutil does not name the drive %s: %v\n%s", driveLabel, err, out)
	}
}
