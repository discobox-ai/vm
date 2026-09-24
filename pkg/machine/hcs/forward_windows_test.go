package hcs

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// A guest process reaches the host through Machine.Listen: disco-vm dial-host
// in a real guest dials hvsocket to its parent partition.
func TestForward(t *testing.T) {
	base := os.Getenv(BaseEnv)
	if base == "" {
		t.Skipf("set %s to run on HCS", BaseEnv)
	}
	// This build's disco-vm, since the image's baked-in agent may predate
	// dial-host.
	exe := filepath.Join(t.TempDir(), "disco-vm.exe")
	build := exec.Command("go", "build", "-o", exe, "../../../cmd/disco-vm")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}

	d := &Driver{}
	ctx := context.Background()
	inst := machine.InstanceSpec{ID: "fwd", Dir: filepath.Join(t.TempDir(), "fwd"), GuestOS: machine.Windows,
		Chain: []machine.Layer{{ID: "base", Dir: base}}, Mode: machine.Cold}
	if err := d.Prepare(ctx, inst); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	m, err := d.Boot(ctx, inst, machine.BootOptions{CPUs: 4, Memory: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill(context.Background()) })
	client := guest.NewClient(func(ctx context.Context) (net.Conn, error) { return m.Dial(ctx, guest.AgentPort) })
	defer client.Close()
	bctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := client.WaitReady(bctx, m.Done()); err != nil {
		t.Fatal(err)
	}
	if !d.Capabilities().Forward {
		t.Fatal("hcs machines listen for the guest but the driver does not list Forward")
	}

	l, err := m.(machine.HostListener).Listen(7401)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fromGuest := make(chan string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			fromGuest <- "accept: " + err.Error()
			return
		}
		defer conn.Close()
		fmt.Fprintln(conn, "hello from the host")
		data, _ := io.ReadAll(conn)
		fromGuest <- string(data)
	}()

	if err := client.CopyTo(ctx, `C:\dv`, tarFile(t, exe)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := client.Run(ctx, guest.ExecRequest{Argv: []string{"cmd", "/c", `echo hello from the guest| C:\dv\disco-vm.exe dial-host 7401`}}, nil, &out, &out)
	if err != nil || code != 0 {
		t.Fatalf("dial-host in the guest: exit %d, %v\n%s", code, err, out.String())
	}
	if !strings.Contains(out.String(), "hello from the host") {
		t.Fatalf("the guest read %q", out.String())
	}
	select {
	case got := <-fromGuest:
		if !strings.Contains(got, "hello from the guest") {
			t.Fatalf("the host read %q", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the host never heard from the guest")
	}
}

func tarFile(t *testing.T, path string) io.Reader {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: filepath.Base(path), Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(data)
	_ = tw.Close()
	return &buf
}
