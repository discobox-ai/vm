package engine_test

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
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
// service is this test's server, listening where the file says. Its peers,
// host processes too, reach it at its ingress URL, a loopback port: that is
// its pool URL. With no loopback, as on vz, libkrun, and hcs, both scopes'
// URL is an owner-only local socket the shim serves, and an ingress asked
// off is withheld while the pool's stays (ADR 26-11-14-610).
func TestURL(t *testing.T) {
	for _, c := range []struct {
		name       string
		noLoopback bool
		urls       machine.URLRequests
		prefix     string
	}{
		{name: "loopback", prefix: "http://127.0.0.1:"},
		{name: "no-loopback", noLoopback: true, prefix: map[bool]string{true: "npipe:", false: "unix:"}[runtime.GOOS == "windows"]},
		{name: "no-loopback-no-ingress", noLoopback: true, urls: machine.URLRequests{Ingress: machine.URLRequest{Mode: machine.URLOff, Strict: true}},
			prefix: map[bool]string{true: "npipe:", false: "unix:"}[runtime.GOOS == "windows"]},
	} {
		t.Run(c.name, func(t *testing.T) { testURL(t, c.noLoopback, c.urls, c.prefix) })
	}
}

func testURL(t *testing.T, noLoopback bool, urls machine.URLRequests, prefix string) {
	t.Helper()
	ctx := context.Background()
	open, _ := fakeRootConfig(t, noLoopback)
	e := open(machine.DefaultPool)
	if _, err := e.Install(ctx, "app", engine.InstallOptions{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	inst, err := e.Create(ctx, engine.CreateOptions{From: "app", Name: "one", URLs: urls})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })
	for _, scope := range machine.URLScopes {
		if _, err := e.URL(ctx, inst, scope); err == nil || !strings.Contains(err.Error(), "not running") {
			t.Fatalf("the %s URL of a stopped instance: %v", scope, err)
		}
	}
	noIngress := urls.Ingress.Mode == machine.URLOff
	var seen []string
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
		pool, err := e.URL(ctx, inst, machine.URLPool)
		if err != nil || !strings.HasPrefix(pool, prefix) {
			t.Fatalf("pool URL %q, %v; want one starting %s", pool, err, prefix)
		}
		ingress, err := e.URL(ctx, inst, machine.URLIngress)
		switch {
		case noIngress && !errors.Is(err, machine.ErrUnsupported):
			t.Fatalf("the ingress URL of an instance whose ingress is asked off: %q, %v; want ErrUnsupported", ingress, err)
		case !noIngress && (err != nil || ingress != pool):
			t.Fatalf("ingress URL %q, %v; want the pool's, %s", ingress, err, pool)
		}
		if path, ok := strings.CutPrefix(pool, "unix://"); ok {
			if info, err := os.Stat(path); err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
				t.Fatalf("the service socket %s: %v, %v; want an owner-only socket", path, info.Mode(), err)
			}
		}
		if body := get(ctx, t, pool); body != "hello from the service" {
			t.Fatalf("GET %s: %q", pool, body)
		}
		_ = server.Close()
		seen = append(seen, pool)
		if err := e.Stop(ctx, inst, 30*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if !noLoopback && seen[0] == seen[1] {
		t.Logf("two starts served the same port, %s, by chance", seen[0])
	}
}

// get reads the body of GET / at a service URL, which machine.DialURL
// reaches whatever its scheme.
func get(ctx context.Context, t *testing.T, url string) string {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return machine.DialURL(ctx, url) },
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://service/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
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
	open, binary := fakeRoot(t)
	e := open(machine.DefaultPool)
	if _, err := e.Install(context.Background(), "app", engine.InstallOptions{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	return e, binary
}

// fakeRoot is a fake driver's root whose shims are a disco-vm built for this
// test, as a program that embeds the engine names its own (ShimCommand), and
// opens an engine on any pool of it.
func fakeRoot(t *testing.T) (func(pool string) *engine.Engine, string) {
	t.Helper()
	return fakeRootConfig(t, false)
}

// fakeRootConfig is fakeRoot, with guests that do not share the host's
// loopback where noLoopback says (fake.Config.NoLoopback).
func fakeRootConfig(t *testing.T, noLoopback bool) (func(pool string) *engine.Engine, string) {
	t.Helper()
	config := "{}"
	if noLoopback {
		config = `{"noLoopback":true}`
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "disco-vm")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../cmd/disco-vm").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	root := filepath.Join(dir, "root")
	open := func(pool string) *engine.Engine {
		t.Helper()
		driver, err := local.New(machine.Config{
			Dir:          root,
			Pool:         pool,
			ShimCommand:  []string{binary, "--root", root, "--driver", "fake", "--driver-config", config, "shim"},
			DriverConfig: json.RawMessage(config),
		}, &fake.Hypervisor{Agent: binary, NoLoopback: noLoopback})
		if err != nil {
			t.Fatal(err)
		}
		e, err := engine.Open(driver, pool)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	return open, binary
}
