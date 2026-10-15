package engine_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/guest"
)

// A guest process reaches a host service through a forward: it connects out
// to the host on a port, and the instance's shim splices that into a Unix
// socket on the host, in both directions.
func TestForward(t *testing.T) {
	ctx := context.Background()
	e, binary := fakeEngine(t)

	// A short directory: a Unix socket path is limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "dvf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "host.sock")
	l, err := (&net.ListenConfig{}).Listen(ctx, "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	fromGuest := make(chan string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintln(conn, "hello from the host")
		data, _ := io.ReadAll(conn)
		fromGuest <- string(data)
	}()

	inst, err := e.Create(ctx, "app", engine.CreateOptions{Name: "fw", Forwards: []engine.Forward{{Port: 7401, Socket: socket}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	// A fake guest is a host process, so the guest's disco-vm is this build.
	argv := []string{"/bin/sh", "-c", `echo "hello from the guest" | "$0" dial-host 7401`, binary}
	if runtime.GOOS == "windows" {
		// No space before the pipe: cmd would send it.
		argv = []string{"cmd", "/c", "echo hello from the guest| " + binary + " dial-host 7401"}
	}
	client := e.Guest(inst)
	defer client.Close()
	var out bytes.Buffer
	if code, err := client.Run(ctx, guest.ExecRequest{Argv: argv}, nil, &out, &out); err != nil || code != 0 {
		t.Fatalf("dial-host: exit %d, %v\n%s", code, err, out.String())
	}
	if !strings.Contains(out.String(), "hello from the host") {
		t.Fatalf("the guest heard %q", out.String())
	}
	select {
	case got := <-fromGuest:
		if !strings.Contains(got, "hello from the guest") {
			t.Fatalf("the host heard %q", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the host socket never heard from the guest")
	}
}
