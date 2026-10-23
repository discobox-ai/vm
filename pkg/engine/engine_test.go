package engine_test

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// fakeDriver is the fake driver for a project, keeping its state in root.
func fakeDriver(t *testing.T, root, project string) machine.Driver {
	t.Helper()
	d, err := local.New(machine.Config{Dir: root, Project: project}, &fake.Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A name a build's instances have is not one an instance can be given.
func TestCreateRefusesABuildsName(t *testing.T) {
	ctx := context.Background()
	d := fakeDriver(t, t.TempDir(), "")
	guestOS := machine.OS(runtime.GOOS)
	layer, err := d.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "latest"}, machine.Layer{ID: "0123456789abcdef", GuestOS: guestOS})
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.Open(d, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Create(ctx, layer.ID, engine.CreateOptions{Name: machine.TemporaryPrefix + "x"}); err == nil {
		t.Fatal("an instance was given a build's name")
	}
	inst, err := e.Create(ctx, layer.ID, engine.CreateOptions{Temporary: true})
	if err != nil || !inst.Temporary() {
		t.Fatalf("a build's instance = %+v, %v", inst, err)
	}
	if list, err := e.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("list = %+v, %v; a build's instance is not listed", list, err)
	}
}

func TestOpenRefusesABadProject(t *testing.T) {
	for _, project := range []string{"Foo", "a--b", "-a", "a-", "a_b", strings.Repeat("a", 33)} {
		if _, err := engine.Open(fakeDriver(t, t.TempDir(), ""), project); err == nil {
			t.Errorf("project %q was accepted", project)
		}
	}
}
