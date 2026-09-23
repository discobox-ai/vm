package hcs

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// The display is the VM's basic video console (see videoMonitor), the way
// sandboxi proved it on a pure-HCS VM: the VM worker serves RDP on a named
// pipe, and the viewer is the stock mstsc. mstsc cannot open a pipe, so a
// loopback port is relayed to it. The console takes one viewer at a time; a
// second connection takes it over from the first.

const (
	consoleWidth, consoleHeight = 1920, 1080
)

// consolePipe is where a VM's console is served, derived from its ID so the
// document that creates it and the viewer that opens it agree.
func consolePipe(id string) string { return `\\.\pipe\disco-vm-console-` + id }

// currentUserSID is who may open the console: whoever runs disco-vm.
func currentUserSID() string {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return ""
	}
	return user.User.Sid.String()
}

// viewer is an open console window.
type viewer struct {
	mstsc *exec.Cmd
	dir   string
	done  chan struct{}
	once  sync.Once
}

// openViewer relays a loopback port to the VM's console pipe and opens mstsc
// on it, titled title. It returns once mstsc is started.
func openViewer(id, title string) (*viewer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		defer listener.Close()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		pipe, err := winio.DialPipeContext(ctx, consolePipe(id))
		cancel()
		if err != nil {
			conn.Close()
			return
		}
		relay(conn, pipe)
	}()

	dir, err := os.MkdirTemp("", "disco-vm-console-")
	if err != nil {
		listener.Close()
		return nil, err
	}
	// mstsc titles its window after the file.
	safe := strings.Map(func(r rune) rune {
		if r < 128 && (r == ' ' || r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			return r
		}
		return '_'
	}, title)
	rdp := filepath.Join(dir, safe+".rdp")
	// The console speaks only standard RDP security: CredSSP must be off, or
	// mstsc is refused and quits. The session lives as long as the VM, so
	// there is nothing to reconnect to after it.
	body := strings.Join([]string{
		"full address:s:" + listener.Addr().String(),
		"screen mode id:i:1",
		fmt.Sprintf("desktopwidth:i:%d", consoleWidth),
		fmt.Sprintf("desktopheight:i:%d", consoleHeight),
		"smart sizing:i:1",
		"enablecredsspsupport:i:0",
		"authentication level:i:0",
		"negotiate security layer:i:1",
		"prompt for credentials:i:0",
		"autoreconnection enabled:i:0",
	}, "\r\n")
	if err := os.WriteFile(rdp, []byte(body), 0o600); err != nil {
		listener.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	cmd := exec.Command("mstsc.exe", rdp)
	if err := cmd.Start(); err != nil {
		listener.Close()
		os.RemoveAll(dir)
		return nil, fmt.Errorf("hcs: open the console window: %w", err)
	}
	v := &viewer{mstsc: cmd, dir: dir, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		listener.Close()
		os.RemoveAll(dir)
		close(v.done)
	}()
	return v, nil
}

// Done is closed when the window is closed.
func (v *viewer) Done() <-chan struct{} { return v.done }

// Close closes the window.
func (v *viewer) Close() {
	v.once.Do(func() { _ = v.mstsc.Process.Kill() })
	<-v.done
}

// relay copies both ways until either side ends, then closes both.
func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	half := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		_ = dst.Close()
		_ = src.Close()
	}
	go half(a, b)
	go half(b, a)
	wg.Wait()
}
