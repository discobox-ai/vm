package boxd

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// TestAnAPIKeyCannotIssue: boxd refuses key management to an API key, which
// an issued key is, so the driver reports ErrCannotIssue, and lists and
// revokes nothing with one.
func TestAnAPIKeyCannotIssue(t *testing.T) {
	d, fake := fakeDriver(t, t.TempDir())
	ctx := t.Context()
	key, err := d.IssueKey(ctx, machine.IssueRequest{Name: "pool", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	keyed := fakeKeyed(t, fake, machine.DefaultProject, machine.StaticKey(key))
	if _, err := keyed.IssueKey(ctx, machine.IssueRequest{Name: "again", TTL: time.Hour}); !errors.Is(err, machine.ErrCannotIssue) {
		t.Fatalf("an issued key issuing: %v, want ErrCannotIssue", err)
	}
	if _, err := keyed.IssuedKeys(ctx, ""); !errors.Is(err, machine.ErrUnauthorized) {
		t.Fatalf("an issued key listing keys: %v, want ErrUnauthorized", err)
	}
	if err := keyed.RevokeKey(ctx, key); !errors.Is(err, machine.ErrUnauthorized) {
		t.Fatalf("an issued key revoking itself: %v, want ErrUnauthorized", err)
	}
	if _, err := d.IssueKey(ctx, machine.IssueRequest{Name: "forever"}); err == nil {
		t.Fatal("a key with no TTL was issued")
	}
}

// TestIssuedKeysAreTheProjects: a key is named for its project, listed and
// revoked only by its project's driver, and a key disco-vm did not issue is
// never revoked.
func TestIssuedKeysAreTheProjects(t *testing.T) {
	d, fake := fakeDriver(t, t.TempDir())
	ctx := t.Context()
	key, err := d.IssueKey(ctx, machine.IssueRequest{Name: "pool", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	name := fake.keys.byID[key.ID].name
	fake.keys.byID["key_mine"] = &fakeKey{id: "key_mine", name: "laptop", secret: "bxd_laptop", kind: "member"}
	fake.mu.Unlock()
	if name != "dvm-default--pool" {
		t.Fatalf("the key is named %q on boxd", name)
	}
	other := fakeKeyed(t, fake, "other", machine.StaticKey(machine.Key{Secret: fakeSession}))
	if keys, err := other.IssuedKeys(ctx, ""); err != nil || len(keys) != 0 {
		t.Fatalf("another project's keys: %v, %v", keys, err)
	}
	if err := other.RevokeKey(ctx, key); err == nil {
		t.Fatal("another project revoked the key")
	}
	if err := d.RevokeKey(ctx, machine.Key{ID: "key_mine"}); err == nil {
		t.Fatal("a key disco-vm did not issue was revoked")
	}
	if keys, err := d.IssuedKeys(ctx, "pool"); err != nil || len(keys) != 1 || keys[0].ID != key.ID || keys[0].Expires.IsZero() {
		t.Fatalf("the project's keys named pool: %v, %v", keys, err)
	}
	if err := d.RevokeKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, ok := fake.keys.byID["key_mine"]; !ok || len(fake.keys.byID) != 1 {
		t.Fatalf("after revoking: %d keys, the caller's own kept %t", len(fake.keys.byID), ok)
	}
}

// TestTheKeyIsAskedOnEveryCall: a key replaced in its source is used from the
// next call, with no new driver.
func TestTheKeyIsAskedOnEveryCall(t *testing.T) {
	d, fake := fakeDriver(t, t.TempDir())
	ctx := t.Context()
	first, err := d.IssueKey(ctx, machine.IssueRequest{Name: "first", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.IssueKey(ctx, machine.IssueRequest{Name: "second", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var current atomic.Pointer[machine.Key]
	current.Store(&first)
	keyed := fakeKeyed(t, fake, machine.DefaultProject, func(context.Context) (machine.Key, error) { return *current.Load(), nil })
	if _, err := keyed.Instances(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.RevokeKey(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := keyed.Instances(ctx); !errors.Is(err, machine.ErrUnauthorized) {
		t.Fatalf("a revoked key: %v, want ErrUnauthorized", err)
	}
	current.Store(&second)
	if _, err := keyed.Instances(ctx); err != nil {
		t.Fatalf("after the key was replaced: %v", err)
	}
	none := fakeKeyed(t, fake, machine.DefaultProject, nil)
	if _, err := none.Instances(ctx); !errors.Is(err, errNoCredentials) {
		t.Fatalf("a driver with no key: %v, want errNoCredentials", err)
	}
}

// TestIsolatedInstances: an isolated instance, and one on networks, are made
// so, from a base and from an image, and a snapshot of an isolated one stays
// isolated.
func TestIsolatedInstances(t *testing.T) {
	d, fake := fakeDriver(t, t.TempDir())
	ctx := t.Context()
	e, err := engine.Open(d, "")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := e.Create(ctx, fake.defaultImage, engine.CreateOptions{Name: "iso", Isolated: true, Networks: []string{"pool"}})
	if err != nil {
		t.Fatal(err)
	}
	if !inst.Isolated || len(inst.Networks) != 1 || inst.Networks[0] != "pool" {
		t.Fatalf("created %+v", inst)
	}
	img, err := e.Snapshot(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Create(ctx, img.ID, engine.CreateOptions{Name: "open", Networks: []string{"pool"}}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("networks on a restore not asked isolated: %v, want ErrUnsupported", err)
	}
	clone, err := e.Create(ctx, img.ID, engine.CreateOptions{Name: "clone", Isolated: true, Networks: []string{"pool", "web"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), clone) })
	if got, err := d.Instance(ctx, "clone"); err != nil || !got.Isolated || len(got.Networks) != 2 {
		t.Fatalf("an instance of an isolated snapshot: %+v, %v", got, err)
	}
}

// TestDebugHidesAKeysSecret: --debug prints CreateApiKey's answer without
// the key.
func TestDebugHidesAKeysSecret(t *testing.T) {
	resp := &boxdapi.CreateApiKeyResponse{Id: "key_1", ApiKey: "bxd_very-secret"}
	if text := debugText(resp); strings.Contains(text, "very-secret") || !strings.Contains(text, "key_1") {
		t.Fatalf("debug text %s", text)
	}
	if resp.GetApiKey() != "bxd_very-secret" {
		t.Fatal("debugText changed the answer")
	}
}
