package guest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
)

// Dialer opens a new stream to the agent: a driver's private channel to the
// guest's AgentPort, or the engine's dial into a running instance's shim,
// which relays to its machine (Relay).
type Dialer func(ctx context.Context) (net.Conn, error)

// Client talks to one guest's agent. It is the machine.Guest of every driver
// that reaches its guest through the agent.
type Client struct {
	dial Dialer
	http *http.Client
}

// NewClient returns a client that opens its connections with dial. It keeps
// none between calls: a connection can be a hypervisor socket, a docker exec,
// or a shim's relay, and a client is handed out (machine.Guest) with nothing
// to release it, so every call dials and closes its own.
func NewClient(dial Dialer) *Client {
	transport := &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		DisableKeepAlives: true,
	}
	return &Client{dial: dial, http: &http.Client{Transport: transport}}
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Response, error) {
	u := "http://guest" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("guest: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

// Health succeeds once the agent answers.
func (c *Client) Health(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/v1/health", nil, nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// WaitReady polls Health until it succeeds or ctx ends. A booting guest
// refuses or drops connections until its agent starts, so every failure before
// then is expected and only the last one is reported.
func (c *Client) WaitReady(ctx context.Context, stopped <-chan struct{}) error {
	var last error
	for {
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		last = c.Health(attempt)
		cancel()
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest agent did not answer: %w (last: %w)", ctx.Err(), last)
		case <-stopped:
			return errors.New("guest stopped before its agent answered")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

var _ machine.Guest = (*Client)(nil)

// Info describes the guest.
func (c *Client) Info(ctx context.Context) (machine.GuestInfo, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/info", nil, nil)
	if err != nil {
		return machine.GuestInfo{}, err
	}
	defer resp.Body.Close()
	var info machine.GuestInfo
	return info, json.NewDecoder(resp.Body).Decode(&info)
}

// Shutdown asks the guest to power off (or reboot) in order. It returns once
// the guest has accepted; the caller watches the machine stop.
func (c *Client) Shutdown(ctx context.Context, reboot bool) error {
	query := url.Values{}
	if reboot {
		query.Set("reboot", "1")
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/shutdown", query, nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// CopyTo extracts a tar stream into the guest directory dir.
func (c *Client) CopyTo(ctx context.Context, dir string, tarStream io.Reader) error {
	resp, err := c.do(ctx, http.MethodPut, "/v1/files", url.Values{"path": {dir}}, tarStream)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// CopyFrom returns the guest path as a tar stream rooted at its base name.
func (c *Client) CopyFrom(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/files", url.Values{"path": {path}}, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// process is a running exec.
type process struct {
	conn net.Conn
	in   *bufio.Reader
	mu   sync.Mutex
}

// Exec starts a process. The connection is the process: closing it kills the
// process in the guest.
func (c *Client) Exec(ctx context.Context, req machine.ExecRequest) (machine.Process, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://guest/v1/exec", bytes.NewReader(body))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	httpReq.Header.Set("Connection", "Upgrade")
	httpReq.Header.Set("Upgrade", UpgradeExec)
	httpReq.Header.Set("Content-Type", "application/json")
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := httpReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	in := bufio.NewReader(conn)
	resp, err := http.ReadResponse(in, httpReq) //nolint:bodyclose // on 101 the body is the upgraded conn, which Process owns; otherwise conn is closed
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = conn.Close()
		return nil, fmt.Errorf("guest: exec: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	_ = conn.SetDeadline(time.Time{})
	return &process{conn: conn, in: in}, nil
}

func (p *process) send(typ byte, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return WriteFrame(p.conn, typ, payload)
}

// Write sends bytes to the process's stdin.
func (p *process) Write(b []byte) (int, error) {
	written := 0
	for written < len(b) {
		n := min(len(b)-written, maxFrame)
		if err := p.send(FrameStdin, b[written:written+n]); err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}

// CloseStdin closes the process's stdin.
func (p *process) CloseStdin() error { return p.send(FrameStdinEOF, nil) }

// Resize sets the process's terminal size.
func (p *process) Resize(rows, cols uint16) error {
	return p.send(FrameResize, resizePayload(rows, cols))
}

// Wait copies the process's output until it exits and returns its exit code.
func (p *process) Wait(stdout, stderr io.Writer) (int, error) {
	for {
		typ, payload, err := ReadFrame(p.in)
		if err != nil {
			return -1, fmt.Errorf("guest: exec stream: %w", err)
		}
		switch typ {
		case FrameStdout:
			if stdout != nil {
				_, _ = stdout.Write(payload)
			}
		case FrameStderr:
			if stderr != nil {
				_, _ = stderr.Write(payload)
			}
		case FrameExit:
			if len(payload) != 4 {
				return -1, errors.New("guest: malformed exit frame")
			}
			return int(int32(uint32(payload[0])<<24 | uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3]))), nil
		case FrameError:
			return -1, fmt.Errorf("guest: exec: %s", payload)
		default:
			return -1, fmt.Errorf("guest: unexpected frame type %d", typ)
		}
	}
}

// Close ends the session; a running process is killed.
func (p *process) Close() error { return p.conn.Close() }
