package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/discobox-ai/vm/pkg/machine"
)

// Orphans lists what the driver holds that this store has no record of: a
// machine no instance names, a snapshot or image no layer or tag names. It
// asks the driver's service every time, so that nothing the store lost track
// of can stay out of sight; ls and images list them with the
// instances and images.
func (e *Engine) Orphans(ctx context.Context) ([]machine.Orphan, error) {
	known := machine.Known{Project: e.Project}
	instances, err := e.list(true)
	if err != nil {
		return nil, err
	}
	for _, inst := range instances {
		known.Instances = append(known.Instances, machine.InstanceSpec{
			ID: inst.ID, Project: inst.Project, Dir: filepath.Join(e.instanceDir(inst.ID), "machine"), GuestOS: inst.GuestOS,
		})
	}
	layers, err := e.Images.Layers()
	if err != nil {
		return nil, err
	}
	for _, layer := range layers {
		known.Layers = append(known.Layers, e.Images.MachineLayer(layer.ID))
	}
	images, err := e.Images.Images(false)
	if err != nil {
		return nil, err
	}
	for _, img := range images {
		if img.Ref != "" {
			known.Tags = append(known.Tags, machine.Tag{Layer: e.Images.MachineLayer(img.Layer.ID), Ref: img.Ref})
		}
	}
	return e.Driver.Orphans(ctx, known)
}

// ErrNotOrphan reports a name that is not one of the project's orphans.
var ErrNotOrphan = errors.New("not an orphan")

// RemoveOrphan deletes the orphan of kind named name (or with that ID). It
// asks the driver again first, so that only what is an orphan of this project
// now, and so nothing the store records, can be deleted this way. A running
// machine is destroyed only with force, as Remove stops an instance only with
// force.
func (e *Engine) RemoveOrphan(ctx context.Context, kind machine.OrphanKind, name string, force bool) (machine.Orphan, error) {
	orphans, err := e.Orphans(ctx)
	if err != nil {
		return machine.Orphan{}, err
	}
	for _, o := range orphans {
		if o.Kind != kind || (o.Name != name && o.ID != name) {
			continue
		}
		if o.Kind == machine.OrphanMachine && o.Status == "running" && !force {
			return o, fmt.Errorf("orphan %s is running; remove it with -f", o.Name)
		}
		return o, e.Driver.RemoveOrphan(ctx, o)
	}
	return machine.Orphan{}, fmt.Errorf("%q is not a %s orphan of project %s: %w", name, kind, e.Project, ErrNotOrphan)
}
