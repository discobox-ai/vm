package machine_test

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
)

// dialTimeout bounds each case of TestDialURL, so a dial or read that
// blocks fails in seconds and names its URL instead of running to the test
// binary's timeout.
const dialTimeout = 10 * time.Second

// DialURL reaches a service at each URL form a driver gives: an HTTP URL's
// host and port, and a Unix socket's path, here and on Windows; a named
// pipe, refused off Windows; and nothing else.
func TestDialURL(t *testing.T) {
	ctx := context.Background()
	tcp, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	sock := filepath.Join(t.TempDir(), "s.sock")
	unix, err := (&net.ListenConfig{}).Listen(ctx, "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close()
	for _, c := range []struct {
		name, url string
		l         net.Listener
	}{
		{"http", "http://" + tcp.Addr().String(), tcp},
		{"unix", machine.SocketURL(sock), unix},
	} {
		t.Run(c.name, func(t *testing.T) {
			go func() {
				conn, err := c.l.Accept()
				if err == nil {
					_, _ = conn.Write([]byte("hi"))
					_ = conn.Close()
				}
			}()
			ctx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			conn, err := machine.DialURL(ctx, c.url)
			if err != nil {
				t.Fatalf("dial %s: %v", c.url, err)
			}
			defer conn.Close()
			// Read the greeting's two bytes, not to EOF: on Windows a read
			// can get the peer's last bytes and then never see its close
			// when it writes and closes at once (a windows CI run read "hi"
			// and waited for EOF to the test timeout).
			_ = conn.SetDeadline(time.Now().Add(dialTimeout))
			got := make([]byte, 2)
			if _, err := io.ReadFull(conn, got); err != nil || string(got) != "hi" {
				t.Fatalf("dial %s read %q: %v", c.url, got, err)
			}
		})
	}
	if runtime.GOOS != "windows" {
		if _, err := machine.DialURL(ctx, machine.PipeURL("disco-vm-test")); !errors.Is(err, machine.ErrUnsupported) {
			t.Fatalf("a named pipe off Windows: %v, want ErrUnsupported", err)
		}
	}
	for _, bad := range []string{"ftp://host/", "unix://host/path", "unix:", "npipe://host/pipe/x", "npipe:////./other/x", "://"} {
		if _, err := machine.DialURL(ctx, bad); err == nil {
			t.Errorf("dial %q succeeded", bad)
		}
	}
}
