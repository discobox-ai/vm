package vz

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// writeAgentISO makes the agent's disk for a macOS guest's boot: an ISO 9660
// image named agentVolume holding agent as agentFile, which the guest's stub
// copies and runs. The guest mounts it read-only at boot with no partition
// map and no extended attributes carried from the host, and hdiutil, which
// makes it, is on every Mac. It replaces iso whole, or leaves it as it was.
func writeAgentISO(ctx context.Context, iso, agent string) error {
	src, err := os.MkdirTemp(filepath.Dir(iso), ".agent-iso-")
	if err != nil {
		return fmt.Errorf("vz: the agent's disk: %w", err)
	}
	defer func() { _ = os.RemoveAll(src) }()
	payload := filepath.Join(src, "payload")
	if err := os.Mkdir(payload, 0o755); err != nil {
		return fmt.Errorf("vz: the agent's disk: %w", err)
	}
	if err := copyExecutable(agent, filepath.Join(payload, agentFile)); err != nil {
		return fmt.Errorf("vz: the agent's disk: %w", err)
	}
	// hdiutil names its output itself when it has no .iso extension.
	tmp := filepath.Join(src, "agent.iso")
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/hdiutil", "makehybrid", "-quiet", "-iso", "-joliet",
		"-default-volume-name", agentVolume, "-o", tmp, payload)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vz: make the agent's disk with hdiutil: %w: %s", err, out)
	}
	if err := os.Rename(tmp, iso); err != nil {
		return fmt.Errorf("vz: the agent's disk: %w", err)
	}
	return nil
}

// copyExecutable copies the file at from to a new file at to, mode 0755.
func copyExecutable(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
