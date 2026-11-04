package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
)

// noVolumes is a driver that attaches no volume.
type noVolumes struct{ machine.Driver }

func (n noVolumes) Capabilities() machine.Capabilities {
	caps := n.Driver.Capabilities()
	caps.Volumes = nil
	return caps
}

// A save to the key an instance started from is guarded by the generation it
// was cloned from, unless forced; a save to another key is not; and another
// pool sees none of the caches. (internal/e2e refuses a running one.)
func TestSaveCacheGuards(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	e, err := engine.Open(fakeDriver(t, root, ""), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Install(ctx, "base", engine.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	create := func(name, key string) engine.Instance {
		t.Helper()
		inst, err := e.Create(ctx, engine.CreateOptions{From: "base", Name: name, Cache: &machine.CacheVolume{Key: key}})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return inst
	}
	a, b := create("a", "k"), create("b", "k")
	first, err := e.SaveCache(ctx, a, engine.SaveOptions{})
	if err != nil || first.Key != "k" || first.Generation == "" {
		t.Fatalf("save a = %+v, %v", first, err)
	}
	if _, err := e.SaveCache(ctx, b, engine.SaveOptions{}); !errors.Is(err, machine.ErrCacheMoved) {
		t.Fatalf("a save from an instance cloned before the key moved: %v, want ErrCacheMoved", err)
	}
	forced, err := e.SaveCache(ctx, b, engine.SaveOptions{Force: true})
	if err != nil || forced.Generation == first.Generation {
		t.Fatalf("a forced save = %+v, %v", forced, err)
	}
	if _, err := e.SaveCache(ctx, b, engine.SaveOptions{Key: "other"}); err != nil {
		t.Fatalf("a save to another key: %v", err)
	}
	c := create("c", "k")
	if c.Cache == nil || c.Cache.Generation != forced.Generation {
		t.Fatalf("an instance of k reports %+v, want generation %s", c.Cache, forced.Generation)
	}
	caches, err := e.Caches(ctx)
	if err != nil || len(caches) != 2 || caches[0].Key != "k" || caches[1].Key != "other" {
		t.Fatalf("caches = %+v, %v", caches, err)
	}

	other, err := engine.Open(fakeDriver(t, root, "other"), "other")
	if err != nil {
		t.Fatal(err)
	}
	if caches, err := other.Caches(ctx); err != nil || len(caches) != 0 {
		t.Fatalf("another pool lists caches %+v: %v", caches, err)
	}
	if _, err := other.Install(ctx, "base", engine.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	inst, err := other.Create(ctx, engine.CreateOptions{From: "base", Name: "a", Cache: &machine.CacheVolume{Key: "k"}})
	if err != nil || inst.Cache == nil || inst.Cache.Generation != "" {
		t.Fatalf("another pool's instance of k = %+v, %v; want no generation", inst.Cache, err)
	}
	if _, err := e.Create(ctx, engine.CreateOptions{From: "base", Name: "bad", Cache: &machine.CacheVolume{Key: "Bad_Key"}}); err == nil {
		t.Fatal("a cache key that is not one was taken")
	}
}

// A driver that attaches no volume is refused one, never given a directory
// on the root disk instead.
func TestVolumesRefused(t *testing.T) {
	ctx := context.Background()
	e, err := engine.Open(noVolumes{fakeDriver(t, t.TempDir(), "")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Install(ctx, "base", engine.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]engine.CreateOptions{
		"data":  {Name: "data", Data: &machine.DataVolume{}},
		"cache": {Name: "cache", Cache: &machine.CacheVolume{Key: "k"}},
	} {
		if _, err := e.Create(ctx, withFrom(opts, "base")); !errors.Is(err, machine.ErrUnsupported) {
			t.Errorf("a %s volume on a driver that attaches none: %v, want ErrUnsupported", name, err)
		}
	}
}
