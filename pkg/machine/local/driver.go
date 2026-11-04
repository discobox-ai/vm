package local

import (
	"context"
	"encoding/json"
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
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Driver keeps a Hypervisor's VMs, committed or not, in a directory.
type Driver struct {
	hv   Hypervisor
	dir  string
	pool string
	shim []string
	keys machine.KeySource
}

var (
	_ machine.Driver = (*Driver)(nil)
	_ machine.Viewer = (*Driver)(nil)
)

// New is a driver for hv, keeping its state in cfg.Dir (machine.DefaultDir
// when empty), for cfg's pool.
func New(cfg machine.Config, hv Hypervisor) (*Driver, error) {
	dir := cfg.Dir
	if dir == "" {
		dir = machine.DefaultDir()
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	pool := cfg.Pool
	if pool == "" {
		pool = machine.DefaultPool
	}
	return &Driver{hv: hv, dir: dir, pool: pool, shim: cfg.ShimCommand, keys: cfg.Keys}, nil
}

// Hypervisor is the hypervisor the driver keeps state for.
func (d *Driver) Hypervisor() Hypervisor { return d.hv }

// Dir is where the driver keeps its state.
func (d *Driver) Dir() string { return d.dir }

// CacheDir is where the hypervisor caches what it downloads
// (cache/<hypervisor>), which Prepare is given as InstanceSpec.CacheDir. It
// may not exist yet.
func (d *Driver) CacheDir() string { return d.cacheDir() }

func (d *Driver) Name() string                       { return d.hv.Name() }
func (d *Driver) Capabilities() machine.Capabilities { return d.hv.Capabilities() }

// Check is the hypervisor's, and the driver's key.
func (d *Driver) Check(ctx context.Context) error {
	if err := d.authorize(ctx); err != nil {
		return err
	}
	return d.hv.Check(ctx)
}

// cacheDir is where the hypervisor caches what it downloads: media, and
// guest images.
func (d *Driver) cacheDir() string { return filepath.Join(d.dir, "cache", d.hv.Name()) }

const (
	instanceFile = "instance.json"
	// installingFile is the record of a VM an install is making, in its
	// directory while the install runs, before instance.json is written.
	installingFile = "installing.json"
	// fromName holds the ID of the VM an instance is created from, written
	// before its disk is, for the moment before its record is.
	fromName   = "from"
	payloadDir = "payload"
	machineDir = "machine"
)

func (d *Driver) instancesDir() string         { return filepath.Join(d.dir, "instances") }
func (d *Driver) instanceDir(id string) string { return filepath.Join(d.instancesDir(), id) }
func (d *Driver) machineDir(id string) string  { return filepath.Join(d.instanceDir(id), machineDir) }
func (d *Driver) namesDir() string             { return filepath.Join(d.instancesDir(), ".names", d.pool) }
func (d *Driver) fromFile(id string) string    { return filepath.Join(d.instanceDir(id), fromName) }
func (d *Driver) recordFile(id string) string  { return filepath.Join(d.instanceDir(id), instanceFile) }
func (d *Driver) writeRecord(rec instanceRecord) error {
	return fsutil.WriteJSON(d.recordFile(rec.ID), rec)
}

// instanceRecord is a VM's instance.json.
type instanceRecord struct {
	machine.Instance
	Pool string `json:"pool"`
	// FromID is the ID of the committed VM this one was created from, by
	// which a chain is walked: the driver's own, since a name is reused
	// once its VM is removed.
	FromID string `json:"fromID,omitempty"`
	// Upgrading is the root an unfinished upgrade gives the VM, written
	// before the upgrade touches a disk and dropped once it is done. While
	// it is set the VM does not boot or commit (Upgrade).
	Upgrading *upgradeTarget `json:"upgrading,omitempty"`
}

// oldRecord is what an instance.json written before ADR 26-11-03-606 has
// and a VM's never does: the image it was cloned from, or the base it ran
// from. Such a record is not read (ADR 26-10-23-417): its disk may
// difference over an image this driver no longer knows, so neither a VM
// created from it nor a rewrite of its record could keep that chain.
type oldRecord struct {
	ImageID string          `json:"imageID"`
	Layer   string          `json:"layer"`
	Base    json.RawMessage `json:"base"`
}

func (o oldRecord) old() bool { return o.ImageID != "" || o.Layer != "" || len(o.Base) > 0 }

// validID refuses an ID that is not a plain directory name.
func validID(id string) bool {
	return id != "" && !strings.ContainsAny(id, `/\.:`) && filepath.Base(id) == id
}

// readRecord reads a VM's record by its ID, of any pool.
func (d *Driver) readRecord(id string) (instanceRecord, error) {
	var rec instanceRecord
	if !validID(id) {
		return rec, fmt.Errorf("instance %s: %w", id, machine.ErrNotFound)
	}
	if err := fsutil.ReadJSON(d.recordFile(id), &rec); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return instanceRecord{}, fmt.Errorf("instance %s: %w", id, machine.ErrNotFound)
		}
		return instanceRecord{}, err
	}
	var old oldRecord
	if err := fsutil.ReadJSON(d.recordFile(id), &old); err != nil {
		return instanceRecord{}, err
	}
	if old.old() {
		return instanceRecord{}, fmt.Errorf("instance %s is from before ADR 26-11-03-606, and not read: %w", id, machine.ErrNotFound)
	}
	if rec.ID != id || machine.ValidatePool(rec.Pool) != nil {
		return instanceRecord{}, fmt.Errorf("instance %s: %w", id, machine.ErrNotFound)
	}
	return rec, nil
}

// readPool reads a VM of the driver's pool by its ID.
func (d *Driver) readPool(id string) (instanceRecord, error) {
	rec, err := d.readRecord(id)
	if err == nil && rec.Pool != d.pool {
		err = fmt.Errorf("instance %s: %w", id, machine.ErrNotFound)
	}
	return rec, err
}

// readInstance reads a VM of the driver's pool by its ID.
func (d *Driver) readInstance(id string) (machine.Instance, error) {
	rec, err := d.readPool(id)
	return rec.Instance, err
}

// runnable reads a VM that can boot: the pool's, and, for a hypervisor
// that Chains, one whose disk no other VM's differences over, since a boot
// writes to it. Whether a VM was committed is the caller's to know.
func (d *Driver) runnable(inst machine.Instance) (instanceRecord, error) {
	rec, err := d.readPool(inst.ID)
	if err == nil {
		err = rec.whole()
	}
	if err == nil && d.hv.Chains() {
		err = d.inUse(rec)
	}
	return rec, err
}

// chain is the committed VM fromID and, for a hypervisor that Chains, the
// committed VMs its disk differences over, base first. One that is gone is
// not found.
func (d *Driver) chain(fromID string) ([]Layer, error) {
	var chain []Layer
	for id := fromID; id != ""; {
		rec, err := d.readRecord(id)
		if err != nil {
			return nil, fmt.Errorf("the committed VM %s: %w", short(id), err)
		}
		chain = append([]Layer{{ID: id, Dir: d.machineDir(id)}}, chain...)
		if !d.hv.Chains() {
			break
		}
		id = rec.FromID
	}
	return chain, nil
}

// spec is the hypervisor's view of a VM: its chain only for a hypervisor
// that Chains, since a copy that does not is a disk of its own once
// Prepare has made it.
func (d *Driver) spec(rec instanceRecord) (InstanceSpec, error) {
	if !validID(rec.ID) {
		return InstanceSpec{}, fmt.Errorf("local: instance ID %q is not a directory name", rec.ID)
	}
	spec := InstanceSpec{
		ID: rec.ID, Dir: d.machineDir(rec.ID), GuestOS: rec.GuestOS,
		CacheDir: d.cacheDir(), Egress: rec.Egress, Data: rec.Data, Cache: rec.Cache,
		Forwards: rec.Forwards,
	}
	if d.hv.Chains() && rec.FromID != "" {
		chain, err := d.chain(rec.FromID)
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

// claim takes a name in the pool for id, or fails with ErrExists.
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

// Create copies the committed VM spec.From, or makes a fresh VM from the
// hypervisor's base.
func (d *Driver) Create(ctx context.Context, spec machine.InstanceSpec) (machine.Instance, error) {
	if err := d.authorize(ctx); err != nil {
		return machine.Instance{}, err
	}
	if err := machine.ValidateName(spec.Name); err != nil {
		return machine.Instance{}, err
	}
	if len(spec.Forwards) > 0 && !d.hv.Capabilities().Ports {
		return machine.Instance{}, fmt.Errorf("%s cannot forward a guest's connections: %w", d.hv.Name(), machine.ErrUnsupported)
	}
	for _, f := range spec.Forwards {
		if f.Peer() && f.Instance == spec.Name {
			return machine.Instance{}, fmt.Errorf("port %d forwards to the instance itself", f.Port)
		}
		if f.Peer() && !guest.DialablePort(f.InstancePort) {
			return machine.Instance{}, fmt.Errorf("port %d forwards to port %d of instance %s, and a peer is reached at its agent's port or its service's alone: %w", f.Port, f.InstancePort, f.Instance, machine.ErrUnsupported)
		}
	}
	if spec.Isolated || len(spec.Networks) > 0 {
		return machine.Instance{}, fmt.Errorf("%s: isolation: %w", d.hv.Name(), machine.ErrUnsupported)
	}
	if spec.From != nil && !d.hv.Capabilities().Commit {
		return machine.Instance{}, fmt.Errorf("%s cannot create a VM from another: %w", d.hv.Name(), machine.ErrUnsupported)
	}
	if !d.hv.Capabilities().Fences(spec.Egress) {
		return machine.Instance{}, fmt.Errorf("%s cannot fence an instance's egress to %q: %w", d.hv.Name(), spec.Egress.Allow, machine.ErrUnsupported)
	}
	if err := checkVolumes(d.hv, spec); err != nil {
		return machine.Instance{}, err
	}
	rec := instanceRecord{
		Pool: d.pool, ID: fsutil.RandomHex(6), Name: spec.Name, CPUs: spec.CPUs, Memory: spec.Memory,
		Created: time.Now().UTC(), Forwards: spec.Forwards, Egress: spec.Egress,
	}
	if spec.From == nil {
		guestOS, ok := d.hv.Base()
		if !ok {
			return machine.Instance{}, fmt.Errorf("%s has no base to make a fresh VM from; create one from a committed VM: %w", d.hv.Name(), machine.ErrUnsupported)
		}
		rec.GuestOS = guestOS
	} else {
		from, err := d.readPool(spec.From.ID)
		if err == nil && from.Name != spec.From.Name {
			err = fmt.Errorf("instance %s: %w", spec.From.Name, machine.ErrNotFound)
		}
		if err != nil {
			return machine.Instance{}, err
		}
		if shim, err := d.openShim(from.ID); err == nil && shim.status(ctx) == nil {
			return machine.Instance{}, fmt.Errorf("instance %s is running; stop and commit it to create from it", from.Name)
		}
		rec.GuestOS, rec.From, rec.FromID = from.GuestOS, from.Name, from.ID
	}
	if spec.Data != nil {
		rec.Data = &machine.DataVolume{Bytes: spec.Data.Bytes}
	}
	if err := d.claim(rec.Name, rec.ID); err != nil {
		return machine.Instance{}, err
	}
	// The VM it is created from is named in its directory before its chain
	// is read, so a removal of that VM (inUse) sees this one before its
	// record is written. A removal and a create in the same instant may
	// both pass, as two cache saves may: nothing takes a lock.
	var chain []Layer
	err := os.MkdirAll(d.instanceDir(rec.ID), 0o755)
	if err == nil && rec.FromID != "" {
		err = os.WriteFile(d.fromFile(rec.ID), []byte(rec.FromID), 0o600)
		if err == nil {
			chain, err = d.chain(rec.FromID)
		}
	}
	var hs InstanceSpec
	if err == nil {
		hs, err = d.spec(rec)
		hs.Chain = chain
	}
	if err == nil {
		err = d.prepare(ctx, &rec.Instance, &hs, spec.Cache)
	}
	if err == nil {
		// The record is the instance: until it is written, nothing lists it.
		err = d.writeRecord(rec)
	}
	if err != nil {
		if hs.Dir != "" {
			_ = d.hv.Destroy(context.Background(), hs)
		}
		_ = os.RemoveAll(d.instanceDir(rec.ID))
		_ = os.Remove(filepath.Join(d.namesDir(), rec.Name))
		return machine.Instance{}, err
	}
	return rec.Instance, nil
}

// Install lays a committed VM's disk from installation media into a VM
// directory of its own, and records it committed.
func (d *Driver) Install(ctx context.Context, spec machine.InstallSpec, name string) (machine.Instance, error) {
	if err := d.authorize(ctx); err != nil {
		return machine.Instance{}, err
	}
	if !d.hv.Capabilities().Installs(spec.GuestOS) {
		return machine.Instance{}, fmt.Errorf("%s installs no %s guest from media: %w", d.hv.Name(), spec.GuestOS, machine.ErrUnsupported)
	}
	if err := machine.ValidateName(name); err != nil {
		return machine.Instance{}, err
	}
	rec := instanceRecord{
		Pool: d.pool, ID: fsutil.RandomHex(6), Name: name, GuestOS: spec.GuestOS,
		CPUs: spec.CPUs, Memory: spec.Memory,
	}
	if err := d.claim(rec.Name, rec.ID); err != nil {
		return machine.Instance{}, err
	}
	hs, err := d.spec(rec)
	if err == nil {
		err = os.MkdirAll(hs.Dir, 0o755)
	}
	// The install's record says, while it runs, which pool's VM it
	// makes, so another process can find it (ViewInstall).
	marker := filepath.Join(d.instanceDir(rec.ID), installingFile)
	if err == nil {
		err = fsutil.WriteJSON(marker, rec)
	}
	if err == nil {
		// An install runs the guest (vz boots its installer, then the
		// installed system), so it holds an admission as a boot does.
		var release func()
		if release, err = d.admit(rec); err == nil {
			err = d.hv.Install(ctx, InstallSpec{InstallSpec: spec, CacheDir: d.cacheDir()}, Layer{ID: rec.ID, Dir: hs.Dir})
			release()
		}
	}
	if err == nil {
		err = os.Remove(marker)
	}
	if err == nil {
		rec.Created = time.Now().UTC()
		err = d.writeRecord(rec)
	}
	if err != nil {
		if hs.Dir != "" {
			_ = d.hv.Destroy(context.Background(), hs)
		}
		_ = os.RemoveAll(d.instanceDir(rec.ID))
		_ = os.Remove(filepath.Join(d.namesDir(), rec.Name))
		return machine.Instance{}, err
	}
	return rec.Instance, nil
}

// checkVolumes refuses a volume the hypervisor does not attach, and a cache
// key that is not one.
func checkVolumes(hv Hypervisor, spec machine.InstanceSpec) error {
	caps := hv.Capabilities()
	if spec.Data != nil && !caps.Attaches(machine.VolumeData) {
		return fmt.Errorf("%s: a data volume: %w", hv.Name(), machine.ErrUnsupported)
	}
	if spec.Cache == nil {
		return nil
	}
	if !caps.Attaches(machine.VolumeCache) {
		return fmt.Errorf("%s: a cache volume: %w", hv.Name(), machine.ErrUnsupported)
	}
	return machine.ValidateCacheKey(spec.Cache.Key)
}

// prepare has the hypervisor make the instance's disk and volumes, its cache
// volume cloned from its key's current generation. A generation removed
// while it was cloned (a save moved the key) is retried once with the new
// one.
func (d *Driver) prepare(ctx context.Context, inst *machine.Instance, hs *InstanceSpec, cache *machine.CacheVolume) error {
	if cache == nil {
		return d.hv.Prepare(ctx, *hs)
	}
	for attempt := 0; ; attempt++ {
		from, gen, err := d.cacheFrom(cache.Key)
		if err != nil {
			return err
		}
		inst.Cache = &machine.CacheVolume{Key: cache.Key, Bytes: cache.Bytes, Generation: gen}
		hs.Cache, hs.CacheFrom = inst.Cache, from
		err = d.hv.Prepare(ctx, *hs)
		if err == nil || gen == "" || attempt > 0 || !d.generationGone(gen) {
			return err
		}
		_ = d.hv.Destroy(ctx, *hs)
	}
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
// removed again, and a name is never taken twice. For a hypervisor that
// Chains, a VM another VM's record names as the one it was created from is
// refused, with machine.ErrInUse.
func (d *Driver) Destroy(ctx context.Context, inst machine.Instance) error {
	if err := d.authorizeInstance(ctx, inst); err != nil {
		return err
	}
	rec, err := d.readRecord(inst.ID)
	if errors.Is(err, machine.ErrNotFound) {
		// A record not written yet, or removed already: what a crash left.
		rec = instanceRecord{Instance: inst}
	} else if err != nil {
		return err
	}
	if d.hv.Chains() {
		if err := d.inUse(rec); err != nil {
			return err
		}
	}
	if shim, err := d.openShim(inst.ID); err == nil {
		_ = shim.kill(ctx)
	}
	dir := d.instanceDir(inst.ID)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return d.release(inst)
	}
	hs, err := d.spec(rec)
	if err != nil {
		// A chain whose base is gone: its disks are removed all the same.
		hs = InstanceSpec{ID: rec.ID, Dir: d.machineDir(rec.ID), GuestOS: rec.GuestOS, Data: rec.Data, Cache: rec.Cache}
	}
	if err := d.hv.Destroy(ctx, hs); err != nil {
		return err
	}
	// The disks an interrupted upgrade left beside it.
	for _, other := range []string{nextDir, oldDir} {
		if _, err := os.Stat(filepath.Join(dir, other)); err == nil {
			if err := d.hv.Destroy(ctx, d.upgradeSpec(rec, other, nil)); err != nil {
				return err
			}
		}
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

// inUse refuses a VM whose disk another VM's differences over, in any
// pool, since every pool's disks share the directory. A record that
// cannot be read is no answer, and refuses.
func (d *Driver) inUse(committed instanceRecord) error {
	entries, err := os.ReadDir(d.instancesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || entry.Name() == committed.ID {
			continue
		}
		rec, err := d.readRecord(entry.Name())
		switch {
		case errors.Is(err, machine.ErrNotFound):
			// A directory with no record is one being made, whose disk may
			// already difference over this one's, or one being removed.
			// One older than staleClaim is what a crashed Create left, as
			// a name's claim is, and holds nothing.
			from, err := os.ReadFile(d.fromFile(entry.Name()))
			if err != nil || string(from) != committed.ID {
				break
			}
			if info, err := os.Stat(d.fromFile(entry.Name())); err == nil && time.Since(info.ModTime()) < staleClaim {
				return fmt.Errorf("instance %s: a VM is being created from it in %s; try again once it is: %w", committed.Name, d.instanceDir(entry.Name()), machine.ErrInUse)
			}
		case err != nil:
			return err
		case rec.FromID == committed.ID:
			return fmt.Errorf("instance %s: %s was created from it, and its disk differences over this one's; remove that one first: %w", committed.Name, rec.Name, machine.ErrInUse)
		case rec.Upgrading != nil && rec.Upgrading.FromID == committed.ID:
			return fmt.Errorf("instance %s: %s is being upgraded to a copy of it; finish or remove that one first: %w", committed.Name, rec.Name, machine.ErrInUse)
		}
	}
	return nil
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

// boot boots the instance in this process, once admit has admitted it.
func (d *Driver) boot(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	if opts.GUI && !d.hv.Capabilities().Display {
		return nil, fmt.Errorf("%s has no display to show: %w", d.hv.Name(), machine.ErrUnsupported)
	}
	rec, err := d.runnable(inst)
	if err != nil {
		return nil, err
	}
	hs, err := d.spec(rec)
	if err != nil {
		return nil, err
	}
	title := opts.Title
	if title == "" {
		title = "disco-vm: " + inst.Name
	}
	release, err := d.admit(rec)
	if err != nil {
		return nil, err
	}
	m, err := d.hv.Boot(ctx, hs, BootOptions{CPUs: inst.CPUs, Memory: inst.Memory, GUI: opts.GUI, Title: title, Console: opts.Console})
	if err != nil {
		release()
		return nil, err
	}
	return admitted(m, release), nil
}

// Commit freezes the stopped VM in place: the hypervisor removes its volumes
// and what only a running VM needs, and its record drops them, so its disk
// is what VMs are created from. Nothing marks it committed: that is the
// caller's to record. A hypervisor that does not report
// Capabilities.Commit is refused, and the VM left as it was.
func (d *Driver) Commit(ctx context.Context, inst machine.Instance) (machine.Instance, error) {
	if err := d.authorizeInstance(ctx, inst); err != nil {
		return machine.Instance{}, err
	}
	if !d.hv.Capabilities().Commit {
		return machine.Instance{}, fmt.Errorf("%s cannot commit a VM: %w", d.hv.Name(), machine.ErrUnsupported)
	}
	rec, err := d.readPool(inst.ID)
	if err == nil {
		err = rec.whole()
	}
	if err != nil {
		return machine.Instance{}, err
	}
	if shim, err := d.openShim(inst.ID); err == nil && shim.status(ctx) == nil {
		return machine.Instance{}, fmt.Errorf("instance %s is running; stop it first", inst.Name)
	}
	hs, err := d.spec(rec)
	if err != nil {
		return machine.Instance{}, err
	}
	if err := d.hv.Commit(ctx, hs); err != nil {
		return machine.Instance{}, err
	}
	rec.Data, rec.Cache = nil, nil
	if err := d.writeRecord(rec); err != nil {
		return machine.Instance{}, err
	}
	return rec.Instance, nil
}

// Service is the hypervisor's address in the guest, and the loopback URL the
// instance's shim serves while it runs, which is its PrivateURL too where
// the guests share the host's loopback.
func (d *Driver) Service(ctx context.Context, inst machine.Instance) (machine.Service, error) {
	if err := d.authorizeInstance(ctx, inst); err != nil {
		return machine.Service{}, err
	}
	rec, err := d.runnable(inst)
	if err != nil {
		return machine.Service{}, err
	}
	hs, err := d.spec(rec)
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
