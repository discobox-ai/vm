package local_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
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
	installed, err := d.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "latest"}, "installed")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "one", From: &installed})
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
// before, serving on a loopback port, is still attached through it, to be
// stopped.
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
	installed, err := d.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "latest"}, "installed")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "sock", From: &installed})
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

	// Start gave the shim its driver's configuration in an owner-only file
	// beside the instance, never on its command line (ADR 26-11-11-215 §6).
	configPath := filepath.Join(root, "instances", inst.ID, "driver-config.json")
	if data, err := os.ReadFile(configPath); err != nil || string(data) != "{}" {
		t.Fatalf("the shim's configuration: %q, %v", data, err)
	}
	if info, err := os.Stat(configPath); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("the shim's configuration file: %v, %v", info.Mode(), err)
	}
	// A shim whose driver is made from another configuration than its
	// starter's is refused, and boots nothing.
	other, err := local.New(machine.Config{Dir: root, ShimCommand: []string{binary, "--root", root, "--driver", "fake", "--driver-config", `{"noCommit":true}`, "shim"}}, &fake.Hypervisor{Agent: binary})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Start(startCtx, inst, machine.BootOptions{}); err == nil || !strings.Contains(err.Error(), "is not the one instance") {
		t.Fatalf("a shim made from another configuration: %v", err)
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

// A shim outlives the engine that started it, so a newer engine meets an
// older shim (discobox ADR 26-11-11-963 §7). One of another build and this
// build's protocol is used as any other. One of another protocol, which every
// shim from before shim.json named its protocol is, reads as running,
// refuses every guest operation saying why, stops in order through the part
// of a shim that lasts, and the instance boots again on its own disk under a
// shim of this build.
func TestOlderShim(t *testing.T) {
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
	e, err := engine.Open(d, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	installed, err := d.Install(ctx, machine.InstallSpec{GuestOS: machine.OS(runtime.GOOS), Media: "latest"}, "installed")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "kept", From: &installed})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	m, err := d.Attach(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CopyTo(ctx, "/kept", tarOf(t, "mark", "before the restart")); err != nil {
		t.Fatal(err)
	}

	statePath := filepath.Join(root, "instances", inst.ID, "shim.json")
	var state map[string]any
	data, err := os.ReadFile(statePath)
	if err == nil {
		err = json.Unmarshal(data, &state)
	}
	if err != nil {
		t.Fatal(err)
	}
	if build, _ := state["build"].(string); state["protocol"] != float64(2) || state["guestProtocol"] != float64(1) || build == "" {
		t.Fatalf("shim.json = %v, want its protocols and build", state)
	}
	rewrite := func(change func(map[string]any)) {
		t.Helper()
		older := maps.Clone(state)
		change(older)
		data, err := json.Marshal(older)
		if err == nil {
			err = os.WriteFile(statePath, data, 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	// An older build that speaks this build's protocols is used.
	rewrite(func(s map[string]any) { s["build"] = "v0.0.0-older" })
	if got := e.State(ctx, inst); got != engine.Running {
		t.Fatalf("an older shim of this protocol reads as %s", got)
	}
	m, err = d.Attach(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Info(ctx); err != nil {
		t.Fatalf("guest operations through an older shim of this protocol: %v", err)
	}
	conn, err := m.Dial(ctx, guest.AgentPort)
	if err != nil {
		t.Fatalf("a stream through an older shim of this protocol: %v", err)
	}
	_ = conn.Close()

	// A shim from before shim.json named its protocol is attached only to be
	// stopped.
	rewrite(func(s map[string]any) {
		delete(s, "protocol")
		delete(s, "guestProtocol")
		delete(s, "build")
	})
	if got := e.State(ctx, inst); got != engine.Running {
		t.Fatalf("an instance under a shim of another protocol reads as %s, want running", got)
	}
	m, err = d.Attach(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Info(ctx)
	if !errors.Is(err, machine.ErrStale) || !strings.Contains(err.Error(), "instance kept: its shim (build unknown) speaks shim protocol 1 and guest protocol 1, and this build") {
		t.Fatalf("a guest operation through a shim of another protocol: %v, want ErrStale naming it", err)
	}
	if _, err := m.Exec(ctx, machine.ExecRequest{Argv: []string{"true"}}); !errors.Is(err, machine.ErrStale) {
		t.Fatalf("exec through a shim of another protocol: %v", err)
	}
	if _, err := m.Dial(ctx, guest.AgentPort); !errors.Is(err, machine.ErrStale) {
		t.Fatalf("a stream through a shim of another protocol: %v", err)
	}
	if err := m.Shutdown(ctx, true); !errors.Is(err, machine.ErrStale) {
		t.Fatalf("a reboot through a shim of another protocol: %v", err)
	}
	if err := e.Stop(ctx, inst, 30*time.Second); err != nil {
		t.Fatalf("stop through a shim of another protocol: %v", err)
	}
	if got := e.State(ctx, inst); got != engine.Stopped {
		t.Fatalf("after its stop the instance reads as %s", got)
	}
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	g, err := e.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := g.CopyFrom(ctx, "/kept/mark")
	if err != nil {
		t.Fatalf("the file written before the shim was replaced: %v", err)
	}
	defer stream.Close()
	if got := untarOne(t, stream); got != "before the restart" {
		t.Fatalf("the file written before the shim was replaced holds %q", got)
	}
	if err := e.Stop(ctx, inst, 30*time.Second); err != nil {
		t.Fatal(err)
	}
}

func tarOf(t *testing.T, name, body string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func untarOne(t *testing.T, r io.Reader) string {
	t.Helper()
	tr := tar.NewReader(r)
	if _, err := tr.Next(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
