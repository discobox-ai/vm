// Package guest is the protocol between disco-vm on the host and the disco-vm
// agent inside a guest, and both of its ends.
//
// The agent is the same disco-vm binary, built for the guest's OS, run as
// `disco-vm guest`. It listens on one stream port: AF_VSOCK in macOS and Linux
// guests, and hvsocket in Windows guests, whose service ID is the port mapped
// through the standard vsock template (winio.VsockServiceID). A port number
// therefore means the same thing on every hypervisor, and a driver's Dial takes
// a port, never a GUID.
//
// HTTP/1.1 is the application protocol, as it is for every discobox guest
// service. Exec upgrades its connection to a framed byte stream so a process's
// stdio, exit code, and terminal size share one connection:
//
//	POST /v1/exec      ExecRequest → 101 disco-vm-exec, then frames
//	GET  /v1/files     ?path=P → tar of P, rooted at P's base name
//	PUT  /v1/files     ?path=D, tar body → extracted into directory D
//	POST /v1/shutdown  ?reboot=1 → 202, then the guest powers off or reboots
//	GET  /v1/info      → Info
//	GET  /v1/health    → 200 once the agent can serve
package guest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// AgentPort is the agent's port on every guest. It is above 1024, as discobox
// requires of guest vsock ports, and clear of discobox's own 3001-3004.
const AgentPort uint32 = 7300

// DisplayPort is reserved for a framebuffer stream (guestfb on Windows).
const DisplayPort uint32 = 7301

// UpgradeExec is the Upgrade token of an exec connection.
const UpgradeExec = "disco-vm-exec"

// ExecRequest starts one process in the guest.
type ExecRequest struct {
	Argv []string `json:"argv"`
	// Env is added to the agent's own environment, as KEY=VALUE.
	Env []string `json:"env,omitempty"`
	Dir string   `json:"dir,omitempty"`
	// User runs the process as another account than the agent's own (root
	// or SYSTEM). macOS needs it: Homebrew refuses to run as root.
	User string `json:"user,omitempty"`
	TTY  bool   `json:"tty,omitempty"`
	// Rows and Cols size the terminal when TTY is set.
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// Info describes the guest as its agent sees it.
type Info struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
	// Fake is set when the agent is serving the fake driver, whose "guest" is
	// a directory on the host.
	Fake bool `json:"fake,omitempty"`
}

// Frame types. Host-to-guest types are below 10 and guest-to-host types are
// 10 and above, so a frame read in the wrong direction is caught as a
// protocol error rather than misread.
const (
	FrameStdin    byte = 0 // payload: bytes for the process's stdin
	FrameStdinEOF byte = 1 // no payload: close the process's stdin
	FrameResize   byte = 2 // payload: rows uint16, cols uint16, big-endian

	FrameStdout byte = 10 // payload: bytes; with a TTY this carries stderr too
	FrameStderr byte = 11 // payload: bytes
	FrameExit   byte = 12 // payload: exit code int32, big-endian; always last
	FrameError  byte = 13 // payload: message; the process could not be run
)

// maxFrame bounds a frame so a corrupt length cannot allocate gigabytes.
const maxFrame = 1 << 20

// WriteFrame writes one frame: type, big-endian uint32 length, payload.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	var header [5]byte
	header[0] = typ
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > maxFrame {
		return 0, nil, fmt.Errorf("guest: frame of %d bytes exceeds %d", size, maxFrame)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return header[0], payload, nil
}

func resizePayload(rows, cols uint16) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:], rows)
	binary.BigEndian.PutUint16(b[2:], cols)
	return b[:]
}

func exitPayload(code int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(int32(code)))
	return b[:]
}
