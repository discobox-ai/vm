package engine_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// The embedder makes a pool's VMs from a committed VM of a scope of its own
// and gives each one forward to a machine of a third scope, the shape of
// discobox's darwin pool (its ADR 26-11-11-963 §§5, 9): the sandbox carries
// the base's proof, and reaches the pool machine through the forward. What a
// pool asks of its engine names neither. discobox's forward goes to the pool
// machine's service port; this one goes to its agent's, since the agent's is
// the one guest port of fake's that is not the host's loopback, and the
// agent answers with the proof.
func TestTheEmbedderReachesAcrossPools(t *testing.T) {
	ctx := context.Background()
	open, binary := fakeRoot(t)

	server := open("server")
	base, err := server.Install(ctx, "darwin-base", engine.InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	provider := open("provider")
	if _, err := provider.Install(ctx, "linux-base", engine.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	poolMachine, err := provider.Create(ctx, engine.CreateOptions{From: "linux-base", Name: "pool-a"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Remove(context.Background(), poolMachine, true) })
	if err := provider.Start(ctx, poolMachine, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	pg, err := provider.Guest(ctx, poolMachine)
	if err != nil {
		t.Fatal(err)
	}
	const proof = "the pool machine's agent"
	if err := pg.CopyTo(ctx, "/scratch", tarOf(t, "proof.txt", proof)); err != nil {
		t.Fatal(err)
	}

	pool := open("pool-a")
	pool.From = &base
	pool.Forwards = []machine.Forward{{Port: 7402, Pool: "provider", Instance: "pool-a", InstancePort: guest.AgentPort}}
	sandbox, err := pool.Create(ctx, engine.CreateOptions{Name: "sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Remove(context.Background(), sandbox, true) })
	if sandbox.Pool != "pool-a" || sandbox.From != "darwin-base" || sandbox.FromPool != "server" ||
		len(sandbox.Forwards) != 1 || sandbox.Forwards[0] != pool.Forwards[0] {
		t.Fatalf("the sandbox is %+v", sandbox)
	}
	if found, err := pool.Get(ctx, "sandbox"); err != nil || found.FromPool != "server" || len(found.Forwards) != 1 {
		t.Fatalf("the sandbox is found %+v: %v", found, err)
	}
	if err := pool.Start(ctx, sandbox, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	g, err := pool.Guest(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	request := "GET /v1/files?path=/scratch/proof.txt HTTP/1.0\r\nHost: guest\r\n\r\n"
	argv := []string{"/bin/sh", "-c", `printf "$1" | "$0" dial-host 7402`, binary, strings.ReplaceAll(request, "\r\n", `\r\n`)}
	if runtime.GOOS == "windows" {
		argv = []string{"powershell", "-NoProfile", "-Command", "\"" + strings.ReplaceAll(request, "\r\n", "`r`n") + "\" | & '" + binary + "' dial-host 7402"}
	}
	var out bytes.Buffer
	if _, err := machine.Run(ctx, g, machine.ExecRequest{Argv: argv}, nil, &out, &out); err != nil {
		t.Fatalf("dial-host: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), proof) {
		t.Fatalf("the sandbox did not reach the pool machine's agent through its forward; it read %q", out.String())
	}

	// An upgrade to no From takes the embedder's too.
	kept, err := pool.Create(ctx, engine.CreateOptions{Name: "kept", Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Remove(context.Background(), kept, true) })
	if upgraded, err := pool.Upgrade(ctx, "kept", engine.UpgradeOptions{}); err != nil || upgraded.From != "darwin-base" || upgraded.FromPool != "server" {
		t.Fatalf("an upgrade to the embedder's From: %+v, %v", upgraded, err)
	}

	// What a pool asks names its own pool's VMs alone: the base is not one.
	if _, err := pool.Create(ctx, engine.CreateOptions{From: "darwin-base", Name: "named"}); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("a pool's create from the server's base: %v, want ErrNotFound", err)
	}
	if list, err := pool.List(ctx); err != nil || len(list) != 2 {
		t.Fatalf("the pool's VMs: %+v, %v; want the sandbox and kept alone", list, err)
	}
}

// What a pool's request carries, CreateOptions and UpgradeOptions, names no
// forward and no pool: only the embedder sets either, on the Engine.
func TestAPoolsRequestNamesNoForwardAndNoPool(t *testing.T) {
	for _, req := range []any{engine.CreateOptions{}, engine.UpgradeOptions{}} {
		typ := reflect.TypeOf(req)
		for f := range typ.Fields() {
			if strings.Contains(f.Name, "Pool") || strings.Contains(f.Name, "Forward") ||
				f.Type == reflect.TypeFor[[]machine.Forward]() || f.Type == reflect.TypeFor[*machine.Instance]() {
				t.Errorf("%s.%s (%s) lets a pool's request reach beyond its pool", typ.Name(), f.Name, f.Type)
			}
		}
	}
}

// noAcrossPools is a driver that reaches no other pool, over one that would.
type noAcrossPools struct{ machine.Driver }

func (n noAcrossPools) Capabilities() machine.Capabilities {
	caps := n.Driver.Capabilities()
	caps.AcrossPools = false
	return caps
}

// An engine whose driver reaches no other pool refuses the embedder's From
// and forwards of another pool before it asks the driver, and makes nothing.
func TestAcrossPoolsNeedsADriverThatReachesThem(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	server, err := engine.Open(fakeDriver(t, root, "server"), "server")
	if err != nil {
		t.Fatal(err)
	}
	base, err := server.Install(ctx, "base", engine.InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := engine.Open(noAcrossPools{fakeDriver(t, root, "pool-a")}, "pool-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Install(ctx, "own", engine.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	pool.From = &base
	if _, err := pool.Create(ctx, engine.CreateOptions{Name: "from"}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("a From of another pool: %v, want ErrUnsupported", err)
	}
	pool.From = nil
	pool.Forwards = []machine.Forward{{Port: 7402, Pool: "server", Instance: "peer", InstancePort: guest.AgentPort}}
	if _, err := pool.Create(ctx, engine.CreateOptions{From: "own", Name: "forward"}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("a forward to another pool: %v, want ErrUnsupported", err)
	}
	if list, err := pool.List(ctx); err != nil || len(list) != 1 {
		t.Fatalf("VMs after refusals, the installed one alone: %+v, %v", list, err)
	}
}

// tarOf is a tar stream of one file.
func tarOf(t *testing.T, name, content string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}
