package local_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// Every call of a driver given a key it did not issue is refused, before it
// reads or changes anything.
func TestEveryCallChecksTheKey(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	hv := &fake.Hypervisor{BaseName: "b"}
	owner, err := local.New(machine.Config{Dir: root}, hv)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := owner.Create(ctx, machine.InstanceSpec{Name: "kept"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := local.New(machine.Config{Dir: root, Keys: machine.StaticKey(machine.Key{Driver: "fake", Secret: "dvmk_not-issued"})}, hv) //nolint:gosec // G101: a key no driver issued
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]func() error{
		"Check":   func() error { return d.Check(ctx) },
		"Install": func() error { _, err := d.Install(ctx, machine.InstallSpec{GuestOS: inst.GuestOS}, "new"); return err },
		"Create":  func() error { _, err := d.Create(ctx, machine.InstanceSpec{Name: "new"}); return err },
		"CreateFrom": func() error {
			_, err := d.Create(ctx, machine.InstanceSpec{Name: "new", From: &inst})
			return err
		},
		"Instance":   func() error { _, err := d.Instance(ctx, inst.Name); return err },
		"Instances":  func() error { _, err := d.Instances(ctx); return err },
		"Destroy":    func() error { return d.Destroy(ctx, inst) },
		"Boot":       func() error { _, err := d.Boot(ctx, inst, machine.BootOptions{}); return err },
		"Start":      func() error { _, err := d.Start(ctx, inst, machine.BootOptions{}); return err },
		"Attach":     func() error { _, err := d.Attach(ctx, inst); return err },
		"Commit":     func() error { _, err := d.Commit(ctx, inst); return err },
		"Service":    func() error { _, err := d.Service(ctx, inst); return err },
		"IssueKey":   func() error { _, err := d.IssueKey(ctx, machine.IssueRequest{Name: "k", TTL: time.Hour}); return err },
		"IssuedKeys": func() error { _, err := d.IssuedKeys(ctx, ""); return err },
		"RevokeKey":  func() error { return d.RevokeKey(ctx, machine.Key{ID: "x"}) },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, machine.ErrUnauthorized) {
			t.Errorf("%s with a key not issued: %v, want ErrUnauthorized", name, err)
		}
	}
	if _, err := owner.Instance(ctx, inst.Name); err != nil {
		t.Fatalf("the instance after every refused call: %v", err)
	}
}

// A key issued for one pool reaches no other's instance by its ID, and
// an expired key's name is free again.
func TestAKeyIsItsPools(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	hv := &fake.Hypervisor{BaseName: "b"}
	other, err := local.New(machine.Config{Dir: root, Pool: "other"}, hv)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := other.Create(ctx, machine.InstanceSpec{Name: "theirs"})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := local.New(machine.Config{Dir: root}, hv)
	if err != nil {
		t.Fatal(err)
	}
	key, err := owner.IssueKey(ctx, machine.IssueRequest{Name: "pool", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	keyed, err := local.New(machine.Config{Dir: root, Keys: machine.StaticKey(key)}, hv)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyed.Destroy(ctx, theirs); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("destroying another pool's instance by its ID: %v, want ErrNotFound", err)
	}
	if _, err := keyed.Service(ctx, theirs); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("another pool's instance's service: %v, want ErrNotFound", err)
	}
	if _, err := other.Instance(ctx, "theirs"); err != nil {
		t.Fatalf("their instance: %v", err)
	}
	installed, err := other.Install(ctx, machine.InstallSpec{GuestOS: machine.OS(runtime.GOOS), Media: "none"}, "installed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyed.Create(ctx, machine.InstanceSpec{Name: "stolen", From: &installed}); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("a VM created from another pool's committed VM: %v, want ErrNotFound", err)
	}

	// An unreadable record authorizes nothing and breaks nothing.
	if err := os.WriteFile(filepath.Join(root, "keys", "junk.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := keyed.Instances(ctx); err != nil {
		t.Fatalf("with a junk record beside it: %v", err)
	}
	if keys, err := owner.IssuedKeys(ctx, ""); err != nil || len(keys) != 1 {
		t.Fatalf("issued keys beside a junk record: %v, %v", keys, err)
	}

	short, err := owner.IssueKey(ctx, machine.IssueRequest{Name: "short", TTL: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := owner.IssueKey(ctx, machine.IssueRequest{Name: "short", TTL: time.Hour}); err != nil {
		t.Fatalf("an expired key's name again: %v", err)
	}
	if keys, _ := owner.IssuedKeys(ctx, "short"); len(keys) != 1 || keys[0].ID == short.ID {
		t.Fatalf("keys named short: %v, want the new one alone", keys)
	}
}
