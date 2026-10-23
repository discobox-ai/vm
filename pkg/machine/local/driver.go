package local

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Driver keeps a Hypervisor's layers, tags, and instances in a directory.
type Driver struct {
	hv      Hypervisor
	dir     string
	project string
	shim    []string
}

var _ machine.Driver = (*Driver)(nil)

// New is a driver for hv, keeping its state in cfg.Dir (machine.DefaultDir
// when empty), for cfg's project.
func New(cfg machine.Config, hv Hypervisor) (*Driver, error) {
	dir := cfg.Dir
	if dir == "" {
		dir = machine.DefaultDir()
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	project := cfg.Project
	if project == "" {
		project = machine.DefaultProject
	}
	return &Driver{hv: hv, dir: dir, project: project, shim: cfg.ShimCommand}, nil
}

// Hypervisor is the hypervisor the driver keeps state for.
func (d *Driver) Hypervisor() Hypervisor { return d.hv }

// Dir is where the driver keeps its state.
func (d *Driver) Dir() string { return d.dir }

func (d *Driver) Name() string                       { return d.hv.Name() }
func (d *Driver) Capabilities() machine.Capabilities { return d.hv.Capabilities() }
func (d *Driver) Check(ctx context.Context) error    { return d.hv.Check(ctx) }

// Bases is the hypervisor's.
func (d *Driver) Bases(ctx context.Context) ([]machine.Base, error) { return d.hv.Bases(ctx) }

const (
	layerFile    = "layer.json"
	instanceFile = "instance.json"
	payloadDir   = "payload"
	machineDir   = "machine"
)

func (d *Driver) layersDir() string            { return filepath.Join(d.dir, "images", "layers") }
func (d *Driver) layerDir(id string) string    { return filepath.Join(d.layersDir(), id) }
func (d *Driver) tagsDir() string              { return filepath.Join(d.dir, "images", "tags", d.project) }
func (d *Driver) instancesDir() string         { return filepath.Join(d.dir, "instances") }
func (d *Driver) instanceDir(id string) string { return filepath.Join(d.instancesDir(), id) }
func (d *Driver) namesDir() string             { return filepath.Join(d.instancesDir(), ".names", d.project) }

// layerRecord is a layer's layer.json.
type layerRecord struct {
	machine.Layer
	Project string `json:"project"`
	// Generation is made at random when the layer is published, and
	// ParentGeneration is its parent's when it was: a child of a parent made
	// again, after one was lost, was built on a disk that is gone.
	Generation       string `json:"generation,omitempty"`
	ParentGeneration string `json:"parentGeneration,omitempty"`
}

// instanceRecord is an instance's instance.json.
type instanceRecord struct {
	machine.Instance
	Project string `json:"project"`
}

// readLayer reads a published layer of the driver's project.
func (d *Driver) readLayer(id string) (machine.Layer, error) {
	rec, err := d.readRecord(id)
	return rec.Layer, err
}

func (d *Driver) readRecord(id string) (layerRecord, error) {
	if !validID(id) {
		return layerRecord{}, fmt.Errorf("layer %s: %w", id, machine.ErrNotFound)
	}
	var rec layerRecord
	if err := fsutil.ReadJSON(filepath.Join(d.layerDir(id), layerFile), &rec); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return layerRecord{}, fmt.Errorf("layer %s: %w", id, machine.ErrNotFound)
		}
		return layerRecord{}, err
	}
	if rec.Project != d.project || rec.ID != id {
		return layerRecord{}, fmt.Errorf("layer %s: %w", id, machine.ErrNotFound)
	}
	return rec, nil
}

// validID refuses an ID that is not a plain directory name.
func validID(id string) bool {
	return id != "" && !strings.ContainsAny(id, `/\.:`) && filepath.Base(id) == id
}

// Layer is a layer whose disk can boot: its own and its ancestors' are all
// there. A layer whose parent was deleted behind the driver's back is not
// found, so a build makes it again, since a disk that differences over its
// parent's (hcs) cannot be trusted over a parent made again.
func (d *Driver) Layer(_ context.Context, id string) (machine.Layer, error) {
	layer, err := d.readLayer(id)
	if err != nil {
		return machine.Layer{}, err
	}
	if _, err := d.chain(id); err != nil {
		return machine.Layer{}, err
	}
	return layer, nil
}

func (d *Driver) Layers(context.Context) ([]machine.Layer, error) {
	entries, err := os.ReadDir(d.layersDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []machine.Layer
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		// A directory with no record is one being made or deleted.
		if layer, err := d.readLayer(entry.Name()); err == nil {
			out = append(out, layer)
		}
	}
	return out, nil
}

// chain is a layer and its ancestors as the hypervisor sees them, base first.
// A layer whose parent is gone, or was made again since, is not found.
func (d *Driver) chain(id string) ([]Layer, error) {
	var chain []Layer
	child := ""
	want := ""
	for id != "" {
		rec, err := d.readRecord(id)
		if err != nil {
			if child != "" {
				return nil, fmt.Errorf("layer %s: its parent %s: %w", short(child), short(id), err)
			}
			return nil, err
		}
		// A record with a parent and no generation of it is from nowhere a
		// layer is published: not trusted.
		if (child != "" && want == "") || (want != "" && rec.Generation != want) {
			return nil, fmt.Errorf("layer %s was not built on the parent %s there now: %w", short(child), short(id), machine.ErrNotFound)
		}
		chain = append([]Layer{{ID: id, Dir: filepath.Join(d.layerDir(id), payloadDir)}}, chain...)
		child, want, id = id, rec.ParentGeneration, rec.Parent
	}
	return chain, nil
}

// pending is a layer being written, under a temporary name.
type pending struct {
	d    *Driver
	meta machine.Layer
	tmp  string
}

func (d *Driver) begin(meta machine.Layer) (*pending, error) {
	if !validID(meta.ID) {
		return nil, fmt.Errorf("local: layer ID %q is not a directory name", meta.ID)
	}
	tmp := filepath.Join(d.layersDir(), ".tmp-"+short(meta.ID)+"-"+fsutil.RandomHex(4))
	if err := os.MkdirAll(filepath.Join(tmp, payloadDir), 0o755); err != nil {
		return nil, err
	}
	return &pending{d: d, meta: meta, tmp: tmp}, nil
}

func (p *pending) layer() Layer { return Layer{ID: p.meta.ID, Dir: filepath.Join(p.tmp, payloadDir)} }

// publish records the layer and renames it into place. A layer of the same ID
// published first is identical by construction, and is the one kept.
func (p *pending) publish() (machine.Layer, error) {
	if p.meta.Created.IsZero() {
		p.meta.Created = time.Now().UTC()
	}
	rec := layerRecord{Layer: p.meta, Project: p.d.project, Generation: fsutil.RandomHex(8)}
	if p.meta.Parent != "" {
		parent, err := p.d.readRecord(p.meta.Parent)
		if err != nil {
			p.abort()
			return machine.Layer{}, err
		}
		rec.ParentGeneration = parent.Generation
	}
	if err := fsutil.WriteJSON(filepath.Join(p.tmp, layerFile), rec); err != nil {
		p.abort()
		return machine.Layer{}, err
	}
	dir := p.d.layerDir(p.meta.ID)
	if _, err := p.d.Layer(context.Background(), p.meta.ID); errors.Is(err, machine.ErrNotFound) {
		// A layer recorded here whose parent is gone is made again, so the
		// one there is moved out of the way first.
		if _, err := p.d.readLayer(p.meta.ID); err == nil {
			if err := p.d.DeleteLayer(context.Background(), p.meta.ID); err != nil {
				p.abort()
				return machine.Layer{}, err
			}
		}
	}
	if err := os.Rename(p.tmp, dir); err != nil {
		p.abort()
		if layer, readErr := p.d.readLayer(p.meta.ID); readErr == nil {
			return layer, nil
		}
		// A directory there with no record is what a crash left between
		// writing a layer and recording it, or a deletion that stopped
		// partway; nothing can be using it.
		if _, statErr := os.Stat(filepath.Join(dir, layerFile)); errors.Is(statErr, fs.ErrNotExist) {
			return machine.Layer{}, fmt.Errorf("local: %s holds a layer with no record; remove it and build again: %w", dir, err)
		}
		return machine.Layer{}, err
	}
	return p.meta, nil
}

func (p *pending) abort() { _ = os.RemoveAll(p.tmp) }

func (d *Driver) Install(ctx context.Context, spec machine.InstallSpec, layer machine.Layer) (machine.Layer, error) {
	layer.Parent = ""
	p, err := d.begin(layer)
	if err != nil {
		return machine.Layer{}, err
	}
	err = d.hv.Install(ctx, InstallSpec{InstallSpec: spec, CacheDir: filepath.Join(d.dir, "cache", d.hv.Name())}, p.layer())
	if err != nil {
		p.abort()
		return machine.Layer{}, err
	}
	return p.publish()
}

// DeleteLayer renames the layer out of the listing, then removes it.
func (d *Driver) DeleteLayer(_ context.Context, id string) error {
	if _, err := d.readLayer(id); errors.Is(err, machine.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	gone := filepath.Join(d.layersDir(), ".del-"+short(id)+"-"+fsutil.RandomHex(4))
	if err := os.Rename(d.layerDir(id), gone); err != nil {
		return err
	}
	return os.RemoveAll(gone)
}

// tagFile is a tag's file: its ref in hex, since a ref's ':' and '/' are not
// file names everywhere.
func (d *Driver) tagFile(ref string) string {
	return filepath.Join(d.tagsDir(), hex.EncodeToString([]byte(ref))+".json")
}

func (d *Driver) Tags(context.Context) ([]machine.Tag, error) {
	entries, err := os.ReadDir(d.tagsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []machine.Tag
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var t machine.Tag
		if err := fsutil.ReadJSON(filepath.Join(d.tagsDir(), entry.Name()), &t); err == nil && t.Ref != "" {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b machine.Tag) int { return strings.Compare(a.Ref, b.Ref) })
	return out, nil
}

func (d *Driver) Tag(_ context.Context, ref, layer string) error {
	if _, err := d.readLayer(layer); err != nil {
		return err
	}
	if err := os.MkdirAll(d.tagsDir(), 0o755); err != nil {
		return err
	}
	return fsutil.WriteJSON(d.tagFile(ref), machine.Tag{Ref: ref, Layer: layer})
}

func (d *Driver) Untag(_ context.Context, ref string) error {
	if err := os.Remove(d.tagFile(ref)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// spec is the hypervisor's view of an instance.
func (d *Driver) spec(inst machine.Instance) (InstanceSpec, error) {
	if !validID(inst.ID) {
		return InstanceSpec{}, fmt.Errorf("local: instance ID %q is not a directory name", inst.ID)
	}
	spec := InstanceSpec{ID: inst.ID, Dir: filepath.Join(d.instanceDir(inst.ID), machineDir), GuestOS: inst.GuestOS, Base: inst.Base}
	if inst.Base == nil {
		chain, err := d.chain(inst.Layer)
		if err != nil {
			return InstanceSpec{}, err
		}
		spec.Chain = chain
	}
	return spec, nil
}

// staleClaim is how old a name's claim with no instance behind it must be
// before another instance may take the name: a Create holds a claim only for
// the moment it takes to clone a disk.
const staleClaim = 10 * time.Minute

// claim takes a name in the project for id, or fails with ErrExists.
func (d *Driver) claim(name, id string) error {
	if err := os.MkdirAll(d.namesDir(), 0o755); err != nil {
		return err
	}
	path := filepath.Join(d.namesDir(), name)
	for range 2 {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = io.WriteString(f, id)
			return errors.Join(err, f.Close())
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		if _, err := d.Instance(context.Background(), name); err == nil || !errors.Is(err, machine.ErrNotFound) {
			return fmt.Errorf("an instance named %q: %w", name, machine.ErrExists)
		}
		info, err := os.Stat(path)
		if err != nil || time.Since(info.ModTime()) < staleClaim {
			return fmt.Errorf("an instance named %q is being made: %w", name, machine.ErrExists)
		}
		// What a crash between claiming the name and recording the
		// instance left: no instance has the name.
		_ = os.Remove(path)
	}
	return fmt.Errorf("an instance named %q: %w", name, machine.ErrExists)
}

func (d *Driver) Create(ctx context.Context, spec machine.InstanceSpec) (machine.Instance, error) {
	if err := machine.ValidateName(spec.Name); err != nil {
		return machine.Instance{}, err
	}
	if len(spec.Forwards) > 0 && !d.hv.Capabilities().Forward {
		return machine.Instance{}, fmt.Errorf("%s cannot forward a guest's connections to the host: %w", d.hv.Name(), machine.ErrUnsupported)
	}
	inst := machine.Instance{
		ID: fsutil.RandomHex(6), Name: spec.Name, Image: spec.Image, Layer: spec.Layer, Base: spec.Base,
		GuestOS: spec.GuestOS, CPUs: spec.CPUs, Memory: spec.Memory,
		Created: time.Now().UTC(), Forwards: spec.Forwards,
	}
	if inst.Base != nil {
		inst.Layer = ""
	}
	if err := d.claim(inst.Name, inst.ID); err != nil {
		return machine.Instance{}, err
	}
	hs, err := d.spec(inst)
	if err == nil {
		err = os.MkdirAll(d.instanceDir(inst.ID), 0o755)
	}
	if err == nil {
		err = d.hv.Prepare(ctx, hs)
	}
	if err == nil {
		// The record is the instance: until it is written, nothing lists it.
		err = fsutil.WriteJSON(filepath.Join(d.instanceDir(inst.ID), instanceFile), instanceRecord{Instance: inst, Project: d.project})
	}
	if err != nil {
		if hs.Dir != "" {
			_ = d.hv.Destroy(context.Background(), hs)
		}
		_ = os.RemoveAll(d.instanceDir(inst.ID))
		_ = os.Remove(filepath.Join(d.namesDir(), inst.Name))
		return machine.Instance{}, err
	}
	return inst, nil
}

// readInstance reads an instance of the driver's project by its ID.
func (d *Driver) readInstance(id string) (machine.Instance, error) {
	var rec instanceRecord
	if !validID(id) {
		return machine.Instance{}, fmt.Errorf("instance %s: %w", id, machine.ErrNotFound)
	}
	if err := fsutil.ReadJSON(filepath.Join(d.instanceDir(id), instanceFile), &rec); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return machine.Instance{}, fmt.Errorf("instance %s: %w", id, machine.ErrNotFound)
		}
		return machine.Instance{}, err
	}
	if rec.Project != d.project || rec.ID != id {
		return machine.Instance{}, fmt.Errorf("instance %s: %w", id, machine.ErrNotFound)
	}
	return rec.Instance, nil
}

func (d *Driver) Instance(_ context.Context, name string) (machine.Instance, error) {
	if err := machine.ValidateName(name); err != nil {
		return machine.Instance{}, fmt.Errorf("instance %q: %w", name, machine.ErrNotFound)
	}
	id, err := os.ReadFile(filepath.Join(d.namesDir(), name))
	if errors.Is(err, fs.ErrNotExist) {
		return machine.Instance{}, fmt.Errorf("instance %q: %w", name, machine.ErrNotFound)
	}
	if err != nil {
		return machine.Instance{}, err
	}
	inst, err := d.readInstance(strings.TrimSpace(string(id)))
	if err == nil && inst.Name != name {
		err = fmt.Errorf("instance %q: %w", name, machine.ErrNotFound)
	}
	return inst, err
}

func (d *Driver) Instances(context.Context) ([]machine.Instance, error) {
	entries, err := os.ReadDir(d.instancesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []machine.Instance
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if inst, err := d.readInstance(entry.Name()); err == nil {
			out = append(out, inst)
		}
	}
	slices.SortFunc(out, func(a, b machine.Instance) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// Destroy ends the instance's shim if one holds it, then removes its disk and
// everything else of it, then its record, and last its name. A removal that
// fails partway leaves the record, so the instance is still listed and can be
// removed again, and a name is never taken twice.
func (d *Driver) Destroy(ctx context.Context, inst machine.Instance) error {
	if shim, err := d.openShim(inst.ID); err == nil {
		_ = shim.kill(ctx)
	}
	dir := d.instanceDir(inst.ID)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return d.release(inst)
	}
	hs := InstanceSpec{ID: inst.ID, Dir: filepath.Join(dir, machineDir), GuestOS: inst.GuestOS, Base: inst.Base}
	if chain, err := d.chain(inst.Layer); err == nil && inst.Base == nil {
		hs.Chain = chain
	}
	if err := d.hv.Destroy(ctx, hs); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == instanceFile {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return d.release(inst)
}

// release gives up an instance's name, if it still names the instance.
func (d *Driver) release(inst machine.Instance) error {
	path := filepath.Join(d.namesDir(), inst.Name)
	if id, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(id)) == inst.ID {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (d *Driver) Boot(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	return d.boot(ctx, inst, opts)
}

func (d *Driver) boot(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (Machine, error) {
	if opts.GUI && !d.hv.Capabilities().Display {
		return nil, fmt.Errorf("%s has no display to show: %w", d.hv.Name(), machine.ErrUnsupported)
	}
	hs, err := d.spec(inst)
	if err != nil {
		return nil, err
	}
	title := opts.Title
	if title == "" {
		title = "disco-vm: " + inst.Name
	}
	return d.hv.Boot(ctx, hs, BootOptions{CPUs: inst.CPUs, Memory: inst.Memory, GUI: opts.GUI, Title: title, Console: opts.Console})
}

// Commit moves the stopped instance's disk into a new layer, publishes it, and
// removes the instance.
func (d *Driver) Commit(ctx context.Context, inst machine.Instance, layer machine.Layer) (machine.Layer, error) {
	if shim, err := d.openShim(inst.ID); err == nil && shim.status(ctx) == nil {
		return machine.Layer{}, fmt.Errorf("instance %s is running; stop it first", inst.Name)
	}
	hs, err := d.spec(inst)
	if err != nil {
		return machine.Layer{}, err
	}
	layer.Parent = inst.Layer
	p, err := d.begin(layer)
	if err != nil {
		return machine.Layer{}, err
	}
	if err := d.hv.Commit(ctx, hs, p.layer()); err != nil {
		p.abort()
		return machine.Layer{}, err
	}
	committed, err := p.publish()
	if err != nil {
		return machine.Layer{}, err
	}
	_ = d.Destroy(ctx, inst)
	return committed, nil
}

// Service is the hypervisor's address in the guest, and the loopback URL the
// instance's shim serves while it runs.
func (d *Driver) Service(ctx context.Context, inst machine.Instance) (machine.Service, error) {
	hs, err := d.spec(inst)
	if err != nil {
		return machine.Service{}, err
	}
	listen, _, err := d.hv.Service(hs)
	if err != nil {
		return machine.Service{}, err
	}
	service := machine.Service{Listen: listen}
	shim, err := d.openShim(inst.ID)
	if err != nil || shim.state.Service == "" || shim.status(ctx) != nil {
		return service, fmt.Errorf("instance %s: %w", inst.Name, machine.ErrNotRunning)
	}
	service.URL = "http://" + shim.state.Service
	return service, nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
