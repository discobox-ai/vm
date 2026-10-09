package engine_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/build"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
)

// An image's service is inherited by the images built on it, and an instance
// of one reaches it through its endpoint. A fake guest is a host process, so
// its service is this test's server on the host's loopback.
func TestEndpoint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	binary := filepath.Join(dir, "disco-vm")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/disco-vm").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "hello from the service") }),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = server.Serve(l) }()
	t.Cleanup(func() { _ = server.Close() })
	port := l.Addr().(*net.TCPAddr).Port

	e, err := engine.Open(filepath.Join(dir, "root"), &fake.Driver{Agent: binary})
	if err != nil {
		t.Fatal(err)
	}
	e.Exe = binary
	install := "from:\n  install: {os: " + runtime.GOOS + ", media: latest}\n"
	layer := "layers:\n  - name: a\n    steps:\n      - run: echo %s\n"
	specs := map[string]string{
		"svc":   install + fmt.Sprintf("service: {port: %d}\n", port) + fmt.Sprintf(layer, "svc"),
		"child": "from:\n  image: svc\n" + fmt.Sprintf(layer, "child"),
		"plain": install + fmt.Sprintf(layer, "plain"),
	}
	for _, tag := range []string{"svc", "child", "plain"} {
		file := filepath.Join(dir, tag+".yaml")
		if err := os.WriteFile(file, []byte(specs[tag]), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := (&build.Builder{Engine: e, Out: io.Discard}).Build(ctx, build.Options{File: file, Tags: []string{tag}}); err != nil {
			t.Fatalf("build %s: %v", tag, err)
		}
	}

	inst, err := e.Create(ctx, "child", engine.CreateOptions{Name: "one"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })
	if inst.Service != uint32(port) {
		t.Fatalf("instance service %d, want the parent image's %d", inst.Service, port)
	}
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	endpoint, err := e.Endpoint(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.URL != "http://guest" || endpoint.Transport == nil {
		t.Fatalf("endpoint %+v, want http://guest and a transport", endpoint)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: endpoint.Transport}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "hello from the service" {
		t.Fatalf("GET through the endpoint: %s %q", resp.Status, body)
	}

	plain, err := e.Create(ctx, "plain", engine.CreateOptions{Name: "two"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), plain, true) })
	if _, err := e.Endpoint(ctx, plain); !errors.Is(err, engine.ErrNoService) {
		t.Fatalf("endpoint of an image with no service: %v", err)
	}
}
