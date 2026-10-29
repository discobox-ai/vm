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

// Driver keeps a Hypervisor's images, tags, and instances in a directory.
type Driver struct {
	hv      Hypervisor
	dir     string
	project string
	shim    []string
	keys    machine.KeySource
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
	return &Driver{hv: hv, dir: dir, project: project, shim: cfg.ShimCommand, keys: cfg.Keys}, nil
}

// Hypervisor is the hypervisor the driver keeps state for.
func (d *Driver) Hypervisor() Hypervisor { return d.hv }

// Dir is where the driver keeps its state.
func (d *Driver) Dir() string { return d.dir }

func (d *Driver) Name() string                       { return d.hv.Name() }
func (d *Driver) Capabilities() machine.Capabilities { return d.hv.Capabilities() }

// Check is the hypervisor's, and the driver's key.
func (d *Driver) Check(ctx context.Context) error {
	if err := d.authorize(ctx); err != nil {
		return err
	}
	return d.hv.Check(ctx)
}

// Bases is the hypervisor's.
func (d *Driver) Bases(ctx context.Context) ([]machine.Base, error) {
	if err := d.authorize(ctx); err != nil {
		return nil, err
	}
	return d.hv.Bases(ctx)
}

// Base is the base the hypervisor lists that ref names.
func (d *Driver) Base(ctx context.Context, ref string) (machine.Base, bool, error) {
	if err := d.authorize(ctx); err != nil {
		return machine.Base{}, false, err
	}
	bases, err := d.hv.Bases(ctx)
	if err != nil {
		return machine.Base{}, false, err
	}
	b, found := machine.FindBase(bases, ref)
	return b, found, nil
}

const (
	// imageFile is an image's record. It keeps the name it had when images
	// were layers, as does the directory the images are in.
	imageFile    = "layer.json"
	instanceFile = "instance.json"
	payloadDir   = "payload"
	machineDir   = "machine"
)

func (d *Driver) imagesDir() string            { return filepath.Join(d.dir, "images", "layers") }
func (d *Driver) imageDir(id string) string    { return filepath.Join(d.imagesDir(), id) }
func (d *Driver) tagsDir() string              { return filepath.Join(d.dir, "images", "tags", d.project) }
func (d *Driver) instancesDir() string         { return filepath.Join(d.dir, "instances") }
func (d *Driver) instanceDir(id string) string { return filepath.Join(d.instancesDir(), id) }
func (d *Driver) namesDir() string             { return filepath.Join(d.instancesDir(), ".names", d.project) }

// imageRecord is an image's layer.json.
type imageRecord struct {
	machine.Image
	Project string `json:"project"`
	// Parent is the image whose disk this one's differences over, kept only
	// for a hypervisor that Chains. It is the driver's alone: above the
	// seam an image has no parent.
	Parent string `json:"parent,omitempty"`
}

// instanceRecord is an instance's instance.json.
type instanceRecord struct {
	machine.Instance
	Project string `json:"project"`
	// Layer is the image of an instance made when images were layers,
	// read as its ImageID.
	Layer string `json:"layer,omitempty"`
}

func (d *Driver) readRecord(id string) (imageRecord, error) {
	if !validID(id) {
		return imageRecord{}, fmt.Errorf("image %s: %w", id, machine.ErrNotFound)
	}
	var rec imageRecord
	if err := fsutil.ReadJSON(filepath.Join(d.imageDir(id), imageFile), &rec); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return imageRecord{}, fmt.Errorf("image %s: %w", id, machine.ErrNotFound)
		}
		return imageRecord{}, err
	}
	if rec.ID != id {
		return imageRecord{}, fmt.Errorf("image %s: %w", id, machine.ErrNotFound)
	}
	return rec, nil
}

// readImage reads a published image of the driver's project.
func (d *Driver) readImage(id string) (machine.Image, error) {
	rec, err := d.readRecord(id)
	if err == nil && rec.Project != d.project {
		err = fmt.Errorf("image %s: %w", id, machine.ErrNotFound)
	}
	return rec.Image, err
}

// validID refuses an ID that is not a plain directory name.
func validID(id string) bool {
	return id != "" && !strings.ContainsAny(id, `/\.:`) && filepath.Base(id) == id
}

// Image is an image whose disk can boot: its own, and, for a hypervisor that
// Chains, every disk it differences over, are all there.
func (d *Driver) Image(ctx context.Context, id string) (machine.Image, error) {
	if err := d.authorize(ctx); err != nil {
		return machine.Image{}, err
	}
	img, err := d.readImage(id)
	if err != nil {
		return machine.Image{}, err
	}
	if _, err := d.chain(id); err != nil {
		return machine.Image{}, err
	}
	return img, nil
}

func (d *Driver) Images(ctx context.Context) ([]machine.Image, error) {
	if err := d.authorize(ctx); err != nil {
		return nil, err
	}
	records, err := d.records()
	if err != nil {
		return nil, err
	}
	var out []machine.Image
	for _, rec := range records {
		if rec.Project == d.project {
			out = append(out, rec.Image)
		}
	}
	return out, nil
}

// records reads every published image's record, of every project: the disks
// of every project share the directory. A record that cannot be read is an
// error, not one left out: DeleteImage reads this list to learn which disks
// another image needs, and a listing that fails is no answer.
func (d *Driver) records() ([]imageRecord, error) {
	entries, err := os.ReadDir(d.imagesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []imageRecord
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		rec, err := d.readRecord(entry.Name())
		switch {
		case err == nil:
			out = append(out, rec)
		case errors.Is(err, machine.ErrNotFound):
			// A directory with no record is one being made or deleted.
		default:
			return nil, err
		}
	}
	return out, nil
}

// chain is an image and the images its disk differences over, as the
// hypervisor sees them, base first. An image whose parent is gone is not
// found.
func (d *Driver) chain(id string) ([]Layer, error) {
	var chain []Layer
	child := ""
	for id != "" {
		rec, err := d.readRecord(id)
		if err != nil {
			if child != "" {
				return nil, fmt.Errorf("image %s: the image its disk differences over, %s: %w", short(child), short(id), err)
			}
			return nil, err
		}
		chain = append([]Layer{{ID: id, Dir: filepath.Join(d.imageDir(id), payloadDir)}}, chain...)
		child, id = id, d.parent(rec)
	}
	return chain, nil
}

// parent is the image a record's disk differences over, for a hypervisor
// that Chains. A record from when images were layers names a parent for every
// hypervisor, which one that does not chain never needed.
func (d *Driver) parent(rec imageRecord) string {
	if !d.hv.Chains() {
		return ""
	}
	return rec.Parent
}

// pending is an image being written, under a temporary name.
type pending struct {
	d   *Driver
	rec imageRecord
	tmp string
}

func (d *Driver) begin(img machine.Image, parent string) (*pending, error) {
	if !validID(img.ID) {
		return nil, fmt.Errorf("local: image ID %q is not a directory name", img.ID)
	}
	tmp := filepath.Join(d.imagesDir(), ".tmp-"+short(img.ID)+"-"+fsutil.RandomHex(4))
	if err := os.MkdirAll(filepath.Join(tmp, payloadDir), 0o755); err != nil {
		return nil, err
	}
	rec := imageRecord{Image: img, Project: d.project}
	if d.hv.Chains() {
		rec.Parent = parent
	}
	return &pending{d: d, rec: rec, tmp: tmp}, nil
}

func (p *pending) layer() Layer { return Layer{ID: p.rec.ID, Dir: filepath.Join(p.tmp, payloadDir)} }

// publish records the image and renames it into place.
func (p *pending) publish() (machine.Image, error) {
	if p.rec.Created.IsZero() {
		p.rec.Created = time.Now().UTC()
	}
	if err := fsutil.WriteJSON(filepath.Join(p.tmp, imageFile), p.rec); err != nil {
		p.abort()
		return machine.Image{}, err
	}
	if err := os.Rename(p.tmp, p.d.imageDir(p.rec.ID)); err != nil {
		p.abort()
		return machine.Image{}, err
	}
	return p.rec.Image, nil
}

func (p *pending) abort() { _ = os.RemoveAll(p.tmp) }

func (d *Driver) Install(ctx context.Context, spec machine.InstallSpec, img machine.Image) (machine.Image, error) {
	if err := d.authorize(ctx); err != nil {
		return machine.Image{}, err
	}
	if !d.hv.Capabilities().Install {
		return machine.Image{}, fmt.Errorf("%s installs nothing from media: %w", d.hv.Name(), machine.ErrUnsupported)
	}
	p, err := d.begin(img, "")
	if err != nil {
		return machine.Image{}, err
	}
	err = d.hv.Install(ctx, InstallSpec{InstallSpec: spec, CacheDir: filepath.Join(d.dir, "cache", d.hv.Name())}, p.layer())
	if err != nil {
		p.abort()
		return machine.Image{}, err
	}
	return p.publish()
}

// DeleteImage refuses an image whose disk another image's differences over,
// in any project, since every project's disks share the directory; otherwise
// it renames the image out of the listing, then removes it.
func (d *Driver) DeleteImage(ctx context.Context, id string) error {
	if err := d.authorize(ctx); err != nil {
		return err
	}
	if _, err := d.readImage(id); errors.Is(err, machine.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	records, err := d.records()
	if err != nil {
		return err
	}
	for _, rec := range records {
		if d.parent(rec) == id {
			return fmt.Errorf("image %s: image %s's disk differences over its disk; remove that one first: %w", short(id), short(rec.ID), machine.ErrInUse)
		}
	}
	gone := filepath.Join(d.imagesDir(), ".del-"+short(id)+"-"+fsutil.RandomHex(4))
	if err := os.Rename(d.imageDir(id), gone); err != nil {
		return err
	}
	return os.RemoveAll(gone)
}

// tagFile is a tag's file: its ref in hex, since a ref's ':' and '/' are not
// file names everywhere.
func (d *Driver) tagFile(ref string) string {
	return filepath.Join(d.tagsDir(), hex.EncodeToString([]byte(ref))+".json")
}

func (d *Driver) Tags(ctx context.Context) ([]machine.Tag, error) {
	if err := d.authorize(ctx); err != nil {
		return nil, err
	}
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
		// A tag from when images were layers names its image "layer".
		var t struct {
			machine.Tag
			Layer string `json:"layer"`
		}
		if err := fsutil.ReadJSON(filepath.Join(d.tagsDir(), entry.Name()), &t); err == nil && t.Ref != "" {
			if t.Image == "" {
				t.Image = t.Layer
			}
			out = append(out, t.Tag)
		}
	}
	slices.SortFunc(out, func(a, b machine.Tag) int { return strings.Compare(a.Ref, b.Ref) })
	return out, nil
}

func (d *Driver) Tag(ctx context.Context, ref, image string) error {
	if err := d.authorize(ctx); err != nil {
		return err
	}
	if _, err := d.readImage(image); err != nil {
		return err
	}
	if err := os.MkdirAll(d.tagsDir(), 0o755); err != nil {
		return err
	}
	return fsutil.WriteJSON(d.tagFile(ref), machine.Tag{Ref: ref, Image: image})
}

func (d *Driver) Untag(ctx context.Context, ref string) error {
	if err := d.authorize(ctx); err != nil {
		return err
	}
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
		chain, err := d.chain(inst.ImageID)
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
	if err := d.authorize(ctx); err != nil {
		return machine.Instance{}, err
	}
	if err := machine.ValidateName(spec.Name); err != nil {
		return machine.Instance{}, err
	}
	if len(spec.Forwards) > 0 && !d.hv.Capabilities().Forward {
		return machine.Instance{}, fmt.Errorf("%s cannot forward a guest's connections to the host: %w", d.hv.Name(), machine.ErrUnsupported)
	}
	if spec.Isolated || len(spec.Networks) > 0 {
		return machine.Instance{}, fmt.Errorf("%s: isolation: %w", d.hv.Name(), machine.ErrUnsupported)
	}
	if spec.Egress != nil {
		return machine.Instance{}, fmt.Errorf("%s: egress: %w", d.hv.Name(), machine.ErrUnsupported)
	}
	if spec.Base == nil {
		if _, err := d.readImage(spec.ImageID); err != nil {
			return machine.Instance{}, err
		}
	}
	inst := machine.Instance{
		ID: fsutil.RandomHex(6), Name: spec.Name, Image: spec.Image, ImageID: spec.ImageID, Base: spec.Base,
		GuestOS: spec.GuestOS, CPUs: spec.CPUs, Memory: spec.Memory,
		Created: time.Now().UTC(), Forwards: spec.Forwards,
	}
	if inst.Base != nil {
		inst.ImageID = ""
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
	if rec.ImageID == "" {
		rec.ImageID = rec.Layer
	}
	return rec.Instance, nil
}

func (d *Driver) Instance(ctx context.Context, name string) (machine.Instance, error) {
	if err := d.authorize(ctx); err != nil {
		return machine.Instance{}, err
	}
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

func (d *Driver) Instances(ctx context.Context) ([]machine.Instance, error) {
	if err := d.authorize(ctx); err != nil {
		return nil, err
	}
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
	if err := d.authorizeInstance(ctx, inst); err != nil {
		return err
	}
	if shim, err := d.openShim(inst.ID); err == nil {
		_ = shim.kill(ctx)
	}
	dir := d.instanceDir(inst.ID)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return d.release(inst)
	}
	hs := InstanceSpec{ID: inst.ID, Dir: filepath.Join(dir, machineDir), GuestOS: inst.GuestOS, Base: inst.Base}
	if chain, err := d.chain(inst.ImageID); err == nil && inst.Base == nil {
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
	if err := d.authorizeInstance(ctx, inst); err != nil {
		return nil, err
	}
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

// Commit moves the stopped instance's disk into a new image, publishes it,
// and removes the instance. For a hypervisor that Chains, the new image's
// disk differences over the instance's image's, which it records as its
// parent.
func (d *Driver) Commit(ctx context.Context, inst machine.Instance, img machine.Image) (machine.Image, error) {
	if err := d.authorizeInstance(ctx, inst); err != nil {
		return machine.Image{}, err
	}
	if shim, err := d.openShim(inst.ID); err == nil && shim.status(ctx) == nil {
		return machine.Image{}, fmt.Errorf("instance %s is running; stop it first", inst.Name)
	}
	hs, err := d.spec(inst)
	if err != nil {
		return machine.Image{}, err
	}
	p, err := d.begin(img, inst.ImageID)
	if err != nil {
		return machine.Image{}, err
	}
	if err := d.hv.Commit(ctx, hs, p.layer()); err != nil {
		p.abort()
		return machine.Image{}, err
	}
	committed, err := p.publish()
	if err != nil {
		return machine.Image{}, err
	}
	_ = d.Destroy(ctx, inst)
	return committed, nil
}

// Service is the hypervisor's address in the guest, and the loopback URL the
// instance's shim serves while it runs, which is its PrivateURL too where
// the guests share the host's loopback.
func (d *Driver) Service(ctx context.Context, inst machine.Instance) (machine.Service, error) {
	if err := d.authorizeInstance(ctx, inst); err != nil {
		return machine.Service{}, err
	}
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
	if d.hv.SharesLoopback() {
		service.PrivateURL = service.URL
	}
	return service, nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
