package machine

import (
	"context"
	"io"
	"net"
)

// Guest is what a caller does inside a running guest: run processes, copy
// files in and out, ask about it, and shut it down in order. Every Machine
// is one. A driver implements it however its hypervisor allows: vz, hcs,
// docker, and fake through the disco-vm agent in the guest (guest.Client over
// the driver's private channel), boxd through its own API, with nothing of
// disco-vm's in the guest.
type Guest interface {
	// Exec starts a process. ctx bounds starting it; the process lives until
	// it exits or the Process is closed.
	Exec(ctx context.Context, req ExecRequest) (Process, error)
	// CopyTo extracts a tar stream into the guest directory dir, creating
	// it. What it unpacks is owned by the guest's administrator (root,
	// SYSTEM).
	CopyTo(ctx context.Context, dir string, tarStream io.Reader) error
	// CopyFrom returns the guest path as a tar stream rooted at its base
	// name. A path that does not exist is an error here, not an empty
	// stream.
	CopyFrom(ctx context.Context, path string) (io.ReadCloser, error)
	// Info describes the guest.
	Info(ctx context.Context) (GuestInfo, error)
	// Shutdown asks the guest to power off (or reboot) in order. It returns
	// once the guest has accepted; the caller watches the machine stop.
	Shutdown(ctx context.Context, reboot bool) error
}

// ExecRequest starts one process in the guest.
type ExecRequest struct {
	Argv []string `json:"argv"`
	// Env is added to the process's login environment, as KEY=VALUE.
	Env []string `json:"env,omitempty"`
	// Dir is the working directory; empty is the guest's default, / on Linux
	// and macOS.
	Dir string `json:"dir,omitempty"`
	// User runs the process as another account than the guest's
	// administrator (root, SYSTEM). macOS needs it: Homebrew refuses to run
	// as root.
	User string `json:"user,omitempty"`
	TTY  bool   `json:"tty,omitempty"`
	// Rows and Cols size the terminal when TTY is set.
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// Process is a running Exec.
type Process interface {
	// Write sends bytes to the process's stdin.
	Write(p []byte) (int, error)
	// CloseStdin closes the process's stdin. A TTY has no end of input: its
	// session ends when its process does.
	CloseStdin() error
	// Resize sets the process's terminal size.
	Resize(rows, cols uint16) error
	// Wait copies the process's output until it exits and returns its exit
	// code. With a TTY, stdout carries stderr too.
	Wait(stdout, stderr io.Writer) (int, error)
	// Close ends the session; a running process is killed.
	Close() error
}

// GuestInfo describes a guest.
type GuestInfo struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname"`
	// Version is the disco-vm agent's, in a guest that has one.
	Version string `json:"version"`
	// Fake is set when the guest is the fake driver's, a directory on the
	// host.
	Fake bool `json:"fake,omitempty"`
}

// Run executes a process to completion: stdin is copied in and closed, output
// copied out, and the exit code returned. Canceling ctx kills the process.
func Run(ctx context.Context, g Guest, req ExecRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	p, err := g.Exec(ctx, req)
	if err != nil {
		return -1, err
	}
	defer p.Close()
	stop := context.AfterFunc(ctx, func() { _ = p.Close() })
	defer stop()
	if stdin != nil {
		go func() {
			_, _ = io.Copy(p, stdin)
			_ = p.CloseStdin()
		}()
	} else {
		_ = p.CloseStdin()
	}
	code, err := p.Wait(stdout, stderr)
	if err != nil && ctx.Err() != nil {
		return -1, ctx.Err()
	}
	return code, err
}

// Dialer is a running machine whose guest ports the host can reach. Every
// machine of a driver that does not list Capabilities.Remote implements it:
// the engine's shim serves the instance's URL through it. A remote driver's
// service gives the URL itself, and keeps whatever channel it has to the
// guest to itself.
type Dialer interface {
	// Dial opens a stream to a guest port: vsock on vz, hvsocket on HCS with
	// the service ID derived from the port by the vsock template, the host's
	// loopback on fake.
	Dial(ctx context.Context, port uint32) (net.Conn, error)
}
