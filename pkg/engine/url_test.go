package engine_test

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// Every instance has one service, where the driver picks: the engine writes
// the address to the guest's service file at start, and the instance's URL
// reaches whatever listens there. A fake guest is a host process, so the
// service is this test's server, listening where the file says.
func TestURL(t *testing.T) {
	ctx := context.Background()
	e, _ := fakeEngine(t)
	inst, err := e.Create(ctx, "app", engine.CreateOptions{Name: "one"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })
	if _, err := e.URL(ctx, inst); err == nil {
		t.Fatal("a stopped instance has a loopback URL")
	}
	var urls []string
	for range 2 {
		if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
			t.Fatal(err)
		}
		address := serviceAddress(ctx, t, e, inst)
		l, err := guest.Listen(address)
		if err != nil {
			t.Fatalf("listen on the service file's %q: %v", address, err)
		}
		server := &http.Server{
			Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "hello from the service") }),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() { _ = server.Serve(l) }()
		url, err := e.URL(ctx, inst)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(url, "http://127.0.0.1:") {
			t.Fatalf("URL %q is not on the loopback", url)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "hello from the service" {
			t.Fatalf("GET %s: %s %q", url, resp.Status, body)
		}
		_ = server.Close()
		urls = append(urls, url)
		if err := e.Stop(ctx, inst, 30*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if urls[0] == urls[1] {
		t.Logf("two starts served the same port, %s, by chance", urls[0])
	}
}

// serviceAddress reads the guest's service file.
func serviceAddress(ctx context.Context, t *testing.T, e *engine.Engine, inst engine.Instance) string {
	t.Helper()
	g, err := e.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := g.CopyFrom(ctx, guest.ServiceFile(string(inst.GuestOS)))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	if _, err := tr.Next(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// fakeEngine is an engine on the fake driver whose guests run this build of
// disco-vm, which it returns, with an image tagged app.
func fakeEngine(t *testing.T) (*engine.Engine, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	binary := filepath.Join(dir, "disco-vm")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/disco-vm").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	root := filepath.Join(dir, "root")
	driver, err := local.New(machine.Config{
		Dir:         root,
		ShimCommand: []string{binary, "--root", root, "--driver", "fake", "shim"},
	}, &fake.Hypervisor{Agent: binary})
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.Open(driver, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Install(ctx, engine.InstallOptions{}, "app"); err != nil {
		t.Fatalf("install: %v", err)
	}
	return e, binary
}
