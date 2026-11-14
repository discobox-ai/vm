package machine_test

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

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
		url string
		l   net.Listener
	}{
		{"http://" + tcp.Addr().String(), tcp},
		{machine.SocketURL(sock), unix},
	} {
		go func() {
			conn, err := c.l.Accept()
			if err == nil {
				_, _ = conn.Write([]byte("hi"))
				_ = conn.Close()
			}
		}()
		conn, err := machine.DialURL(ctx, c.url)
		if err != nil {
			t.Fatalf("dial %s: %v", c.url, err)
		}
		got, _ := io.ReadAll(conn)
		_ = conn.Close()
		if string(got) != "hi" {
			t.Fatalf("dial %s read %q", c.url, got)
		}
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
