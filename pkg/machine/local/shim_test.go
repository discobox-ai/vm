package local_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// A program that embeds the engine starts its shims as a subcommand of its
// own (machine.Config.ShimCommand). Here that program is a script, which
// records how it was called and then runs the real shim.
func TestShimCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in program is a shell script")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "disco-vm")
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../../cmd/disco-vm").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	root := filepath.Join(dir, "root")
	mark := filepath.Join(dir, "called")
	script := filepath.Join(dir, "embedder")
	body := "#!/bin/sh\necho \"$@\" > " + mark + "\nshift\nexec " + binary + " --root " + root + " --driver fake shim \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: the script is run
		t.Fatal(err)
	}
	d, err := local.New(machine.Config{Dir: root, ShimCommand: []string{script, "vm-shim"}}, &fake.Hypervisor{Agent: binary})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	guestOS := machine.OS(runtime.GOOS)
	img, err := d.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "latest"}, machine.Image{ID: "0123456789abcdef", GuestOS: guestOS})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "one", ImageID: img.ID, GuestOS: guestOS})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	m, err := d.Start(startCtx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	called, _ := os.ReadFile(mark)
	if got := strings.TrimSpace(string(called)); got != "vm-shim "+inst.ID {
		t.Fatalf("the embedder was called with %q, want %q", got, "vm-shim "+inst.ID)
	}
	if _, err := m.Info(ctx); err != nil {
		t.Fatalf("the instance its shim runs does not answer: %v", err)
	}
}

func TestShimMainArgs(t *testing.T) {
	d, err := local.New(machine.Config{Dir: t.TempDir()}, &fake.Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"a", "b"}, {"--warm", "x"}} {
		if err := local.ShimMain(context.Background(), d, args); err == nil || !strings.Contains(err.Error(), "want [--gui] INSTANCE") {
			t.Errorf("ShimMain(%q) = %v, want a usage error", args, err)
		}
	}
}

// A shim serves its control API on an owner-only Unix socket it names in
// shim.json, not on a loopback port; and an instance whose shim is from
// before, serving on a loopback port, is still attached through it.
func TestShimSocket(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "disco-vm")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../../cmd/disco-vm").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	root := filepath.Join(dir, "root")
	d, err := local.New(machine.Config{Dir: root, ShimCommand: []string{binary, "--root", root, "--driver", "fake", "shim"}}, &fake.Hypervisor{Agent: binary})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	guestOS := machine.OS(runtime.GOOS)
	img, err := d.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "latest"}, machine.Image{ID: "0123456789abcdef", GuestOS: guestOS})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "sock", ImageID: img.ID, GuestOS: guestOS})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	m, err := d.Start(startCtx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "instances", inst.ID, "shim.json")
	var state struct {
		PID    int    `json:"pid"`
		Socket string `json:"socket"`
		Addr   string `json:"addr"`
		Token  string `json:"token"`
	}
	data, err := os.ReadFile(statePath)
	if err == nil {
		err = json.Unmarshal(data, &state)
	}
	if err != nil {
		t.Fatal(err)
	}
	if state.Socket == "" || state.Addr != "" {
		t.Fatalf("shim.json names socket %q and address %q, want a socket alone", state.Socket, state.Addr)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(state.Socket)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode(); mode&os.ModeSocket == 0 || mode.Perm() != 0o600 {
			t.Fatalf("the control socket is %v, want an owner-only socket", mode)
		}
	}
	if err := m.Kill(ctx); err != nil {
		t.Fatal(err)
	}

	// A shim from before serves on a loopback port, behind its token.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer old-token" || r.URL.Path != "/status" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	old, err := json.Marshal(struct {
		PID   int    `json:"pid"`
		Addr  string `json:"addr"`
		Token string `json:"token"`
	}{os.Getpid(), server.Listener.Addr().String(), "old-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Attach(ctx, inst); err != nil {
		t.Fatalf("attach through a shim from before the socket: %v", err)
	}
	_ = os.Remove(statePath)
}
