package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// An instance's volumes are named Docker volumes (ADR 26-11-02-180):
//
//   - its data and cache volumes, `dvm-<project>--<name>--data-<random>` and
//     `--cache-<random>`, mounted at their machine.VolumePath and removed
//     with the container. The random end keeps a volume a failed Destroy
//     left from being taken by a later instance of the name.
//   - a cache generation, `dvm-<project>--cachegen--<key>--<generation>`,
//     which nothing mounts but the short-lived container that copies it.
//   - a key's pointer, `dvm-<project>--cachekey--<key>--<generation>`, an
//     empty volume made once its generation is whole. A key's current
//     generation is its newest pointer's. Labels never change, so a pointer
//     made last is how a generation is published.
//
// A cache is copied between volumes through the daemon's archive API: a tar
// stream from one container, with the owners in the guest's uids, extracted
// into another, one of them a container that is made and never started, to
// mount the volume. Bytes are a hint: the local volume driver has no quota.

const (
	// labelVolume says what a volume is: volumeData, volumeCache,
	// volumeGeneration, or volumeKey.
	labelVolume = "ai.discobox.vm.volume"
	// labelData is an instance's data volume's size, on its container;
	// empty or absent, it has none. Every volume label of a container is
	// set at Create, empty where it has no such volume, since a container
	// inherits its image's labels.
	labelData = "ai.discobox.vm.data"
	// labelCache is a cache key: on an instance's container, the key its
	// cache volume was cloned from; on a generation and a pointer, theirs.
	labelCache      = "ai.discobox.vm.cache"
	labelCacheBytes = "ai.discobox.vm.cache-bytes"
	// labelGeneration is a generation's ID, on it, on its pointer, and on
	// the container of an instance cloned from it.
	labelGeneration = "ai.discobox.vm.generation"
	// labelSaved is when a pointer's generation was saved, RFC 3339.
	labelSaved = "ai.discobox.vm.saved"

	volumeData       = "data"
	volumeCache      = "cache"
	volumeGeneration = "generation"
	volumeKey        = "key"
	// volumeCopy marks the container that copies a cache.
	volumeCopy = "copy"
)

// instanceVolume is the name of an instance's volume v.
func (d *Driver) instanceVolume(name string, v machine.Volume) string {
	return containerName(d.project, name) + "--" + string(v) + "-" + fsutil.RandomHex(3)
}

func (d *Driver) generationVolume(key, gen string) string {
	return machine.ResourcePrefix(namePrefix, d.project) + "cachegen--" + key + "--" + gen
}

func (d *Driver) keyVolume(key, gen string) string {
	return machine.ResourcePrefix(namePrefix, d.project) + "cachekey--" + key + "--" + gen
}

// volumes are what Create makes for an instance: the volumes, their mounts,
// and the labels its container carries for them.
type volumes struct {
	names  []string
	mounts []mount
	labels map[string]string
	// copier is a container that mounts the generation the cache volume is
	// cloned from, so no save removes it until the clone is done; empty for
	// an empty volume. The caller removes it.
	copier string
}

// makeVolumes makes the spec's volumes, empty, and mounts the generation its
// cache is cloned from in a copier made from image.
func (d *Driver) makeVolumes(ctx context.Context, spec machine.InstanceSpec, image string) (volumes, error) {
	vs := volumes{labels: map[string]string{}}
	make1 := func(v machine.Volume) error {
		name := d.instanceVolume(spec.Name, v)
		labels := map[string]string{labelProject: d.project, labelInstance: spec.Name, labelVolume: string(v)}
		if err := d.api.createVolume(ctx, name, labels); err != nil {
			return fmt.Errorf("docker: create volume %s: %w", name, err)
		}
		vs.names = append(vs.names, name)
		vs.mounts = append(vs.mounts, volumeMount(name, machine.VolumePath(machine.Linux, v), false))
		return nil
	}
	if spec.Data != nil {
		if err := make1(machine.VolumeData); err != nil {
			return vs, err
		}
		vs.labels[labelData] = strconv.FormatInt(spec.Data.Bytes, 10)
	}
	if spec.Cache == nil {
		return vs, nil
	}
	if err := machine.ValidateCacheKey(spec.Cache.Key); err != nil {
		return vs, err
	}
	if err := make1(machine.VolumeCache); err != nil {
		return vs, err
	}
	vs.labels[labelCache] = spec.Cache.Key
	vs.labels[labelCacheBytes] = strconv.FormatInt(spec.Cache.Bytes, 10)
	gen, copier, err := d.pin(ctx, spec.Cache.Key, image)
	if err != nil {
		return vs, err
	}
	if gen != "" {
		vs.labels[labelGeneration], vs.copier = gen, copier
	}
	return vs, nil
}

// pin mounts a key's current generation in a copier made from image, which
// keeps a save from removing it, and returns both; none for a key with no
// generation. A generation a save removed between reading it and mounting
// it is tried again with the key's new one.
func (d *Driver) pin(ctx context.Context, key, image string) (string, string, error) {
	for range 3 {
		cur, found, err := d.current(ctx, key)
		if err != nil || !found {
			return "", "", err
		}
		copier, err := d.copier(ctx, image, d.generationVolume(key, cur.Generation))
		if errors.Is(err, errGenerationGone) {
			continue
		}
		return cur.Generation, copier, err
	}
	return "", "", fmt.Errorf("docker: cache %s moved three times while an instance was cloned from it", key)
}

// removeVolumes is best-effort cleanup of volumes the driver made.
func (d *Driver) removeVolumes(names []string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, name := range names {
		_ = d.api.removeVolume(ctx, name)
	}
}

// volumesOf reads an instance's data and cache volumes from its container's
// labels.
func volumesOf(inst *machine.Instance, labels map[string]string) {
	if raw := labels[labelData]; raw != "" {
		n, _ := strconv.ParseInt(raw, 10, 64)
		inst.Data = &machine.DataVolume{Bytes: n}
	}
	if key := labels[labelCache]; key != "" {
		n, _ := strconv.ParseInt(labels[labelCacheBytes], 10, 64)
		inst.Cache = &machine.CacheVolume{Key: key, Bytes: n, Generation: labels[labelGeneration]}
	}
}

// checkVolumes refuses an instance whose volumes, once its container
// mounts them, are not the ones Create made: a Destroy of an earlier
// instance of the name removes them by its labels, and the daemon makes a
// missing volume a mount names, empty and unlabeled. Once mounted they are
// in use, and no Destroy removes them.
func (d *Driver) checkVolumes(ctx context.Context, names []string) error {
	for _, name := range names {
		v, err := d.api.inspectVolume(ctx, name)
		if err != nil {
			return fmt.Errorf("volume %s: %w", name, err)
		}
		if v.Labels[labelProject] != d.project {
			return fmt.Errorf("volume %s was removed while the instance was made (an earlier instance of its name destroyed at once?)", name)
		}
	}
	return nil
}

// removeOwnVolumes removes the data and cache volumes of every instance of
// the name, found by their labels, so a Destroy that failed once its
// container was gone finds them again. The labels name an instance's name,
// which every instance of it shares: one that a container still mounts (a
// later instance of the name) is in use, and is left, and one a Create
// made but has not mounted yet is found missing by its checkVolumes.
func (d *Driver) removeOwnVolumes(ctx context.Context, name string) error {
	vols, err := d.api.volumes(ctx, map[string]string{labelProject: d.project, labelInstance: name})
	if err != nil {
		return fmt.Errorf("docker: list volumes: %w", err)
	}
	for _, v := range vols {
		if kind := v.Labels[labelVolume]; kind != volumeData && kind != volumeCache {
			continue
		}
		if err := d.api.removeVolume(ctx, v.Name); err != nil && !isConflict(err) {
			return fmt.Errorf("docker: remove volume %s: %w", v.Name, err)
		}
	}
	return nil
}

// copyCache copies the cache directory of container from into container to.
// The tar stream is rooted at the directory's base name, so it is extracted
// into its parent.
func (d *Driver) copyCache(ctx context.Context, from, to string) error {
	dir := machine.VolumePath(machine.Linux, machine.VolumeCache)
	stream, err := d.api.getArchive(ctx, from, dir)
	if err != nil {
		return fmt.Errorf("docker: read the cache volume: %w", err)
	}
	defer stream.Close()
	if err := d.api.putArchive(ctx, to, path.Dir(dir), stream); err != nil {
		return fmt.Errorf("docker: write the cache volume: %w", err)
	}
	_, _ = io.Copy(io.Discard, stream)
	return nil
}

// errGenerationGone reports a generation removed before a copier mounted it.
var errGenerationGone = errors.New("docker: the cache generation is gone")

// copier makes a container, from image and never started, that mounts the
// generation volume at the cache volume's path, which holds the volume in
// use. The daemon makes a volume a mount names when it is missing, empty and
// unlabeled, so one that is not a generation once mounted was removed in
// between: the copier and that volume are removed, and the error is
// errGenerationGone.
func (d *Driver) copier(ctx context.Context, image, volume string) (string, error) {
	name := machine.ResourcePrefix(namePrefix, d.project) + "cachecopy--" + fsutil.RandomHex(6)
	id, err := d.api.createContainer(ctx, name, containerConfig{
		Image:  image,
		Cmd:    []string{},
		Labels: map[string]string{labelProject: d.project, labelVolume: volumeCopy},
		HostConfig: hostConfig{
			Mounts: []mount{volumeMount(volume, machine.VolumePath(machine.Linux, machine.VolumeCache), false)},
		},
	})
	if err != nil {
		return "", fmt.Errorf("docker: create %s: %w", name, err)
	}
	v, err := d.api.inspectVolume(ctx, volume)
	if err == nil && v.Labels[labelVolume] == volumeGeneration {
		return id, nil
	}
	d.removeContainer(id)
	if err != nil {
		return "", fmt.Errorf("docker: volume %s: %w", volume, err)
	}
	if v.Labels[labelVolume] == "" {
		d.removeVolumes([]string{volume})
	}
	return "", errGenerationGone
}

// pointer is a key's pointer volume, as the daemon lists it.
type pointer struct {
	machine.Cache
	name string
}

// pointers reads a key's pointers whose generation is there, newest first,
// and every volume of the key: pointers and generations.
func (d *Driver) pointers(ctx context.Context, key string) ([]pointer, []volumeInfo, error) {
	vols, err := d.api.volumes(ctx, map[string]string{labelProject: d.project, labelCache: key})
	if err != nil {
		return nil, nil, fmt.Errorf("docker: list cache %s: %w", key, err)
	}
	return d.pointersIn(vols), vols, nil
}

// pointersIn are the pointers in vols whose generation is in vols too,
// newest first.
func (d *Driver) pointersIn(vols []volumeInfo) []pointer {
	gens := map[string]bool{}
	for _, v := range vols {
		if v.Labels[labelVolume] == volumeGeneration {
			gens[v.Labels[labelCache]+"/"+v.Labels[labelGeneration]] = true
		}
	}
	var out []pointer
	for _, v := range vols {
		key, gen := v.Labels[labelCache], v.Labels[labelGeneration]
		if v.Labels[labelVolume] != volumeKey || !gens[key+"/"+gen] {
			continue
		}
		p := pointer{name: v.Name}
		p.Key, p.Generation = key, gen
		p.Bytes, _ = strconv.ParseInt(v.Labels[labelCacheBytes], 10, 64)
		p.Saved, _ = time.Parse(time.RFC3339Nano, v.Labels[labelSaved])
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b pointer) int {
		if c := b.Saved.Compare(a.Saved); c != 0 {
			return c
		}
		return strings.Compare(b.name, a.name)
	})
	return out
}

// Caches lists each of the project's keys with its newest pointer's
// generation.
func (d *Driver) Caches(ctx context.Context) ([]machine.Cache, error) {
	vols, err := d.api.volumes(ctx, map[string]string{labelProject: d.project})
	if err != nil {
		return nil, fmt.Errorf("docker: list volumes: %w", err)
	}
	var out []machine.Cache
	seen := map[string]bool{}
	for _, p := range d.pointersIn(vols) {
		if !seen[p.Key] {
			seen[p.Key] = true
			out = append(out, p.Cache)
		}
	}
	slices.SortFunc(out, func(a, b machine.Cache) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

// current is a key's current generation: its newest pointer's.
func (d *Driver) current(ctx context.Context, key string) (machine.Cache, bool, error) {
	ps, _, err := d.pointers(ctx, key)
	if err != nil || len(ps) == 0 {
		return machine.Cache{}, false, err
	}
	return ps[0].Cache, true, nil
}

// guard refuses a guarded save whose key does not hold the generation it
// expects.
func (d *Driver) guard(ctx context.Context, save machine.CacheSave) error {
	if !save.Guarded {
		return nil
	}
	cur, found, err := d.current(ctx, save.Key)
	if err != nil {
		return err
	}
	if cur.Generation == save.IfGeneration {
		return nil
	}
	if !found {
		return fmt.Errorf("cache %s holds no generation, not %s: %w", save.Key, save.IfGeneration, machine.ErrCacheMoved)
	}
	return fmt.Errorf("cache %s holds generation %s: %w", save.Key, cur.Generation, machine.ErrCacheMoved)
}

// SaveCache copies the stopped instance's cache volume into a new
// generation, then publishes it with a pointer, checking a guarded save
// before it copies and again just before it publishes, and then removes the
// key's older pointers and generations, but one a copy still mounts. Two
// saves racing in the same moment may both pass the check: the one begun
// last wins (prune).
func (d *Driver) SaveCache(ctx context.Context, inst machine.Instance, save machine.CacheSave) (machine.Cache, error) {
	if err := refuseCommitted(inst); err != nil {
		return machine.Cache{}, err
	}
	if err := machine.ValidateCacheKey(save.Key); err != nil {
		return machine.Cache{}, err
	}
	ref := containerName(d.project, inst.Name)
	c, err := d.api.inspectContainer(ctx, inst.ID)
	if err != nil {
		return machine.Cache{}, fmt.Errorf("docker: container %s: %w", ref, err)
	}
	if c.State.Running {
		return machine.Cache{}, fmt.Errorf("instance %s is running; stop it first", inst.Name)
	}
	cur := d.instance(c)
	if cur.Cache == nil {
		return machine.Cache{}, fmt.Errorf("instance %s has no cache volume", inst.Name)
	}
	if err := d.guard(ctx, save); err != nil {
		return machine.Cache{}, err
	}
	saved := machine.Cache{Key: save.Key, Generation: fsutil.RandomHex(8), Bytes: cur.Cache.Bytes, Saved: time.Now().UTC()}
	labels := map[string]string{
		labelProject: d.project, labelCache: saved.Key, labelGeneration: saved.Generation,
		labelCacheBytes: strconv.FormatInt(saved.Bytes, 10), labelSaved: saved.Saved.Format(time.RFC3339Nano),
	}
	gen := d.generationVolume(saved.Key, saved.Generation)
	labels[labelVolume] = volumeGeneration
	if err := d.api.createVolume(ctx, gen, labels); err != nil {
		return machine.Cache{}, fmt.Errorf("docker: create volume %s: %w", gen, err)
	}
	copier, err := d.copier(ctx, c.Image, gen)
	if errors.Is(err, errGenerationGone) {
		err = fmt.Errorf("cache %s: a later save removed this one's generation: %w", save.Key, machine.ErrCacheMoved)
	}
	if err == nil {
		err = d.copyCache(ctx, inst.ID, copier)
		d.removeContainer(copier)
	}
	if err == nil {
		err = d.guard(ctx, save)
	}
	if err == nil {
		labels[labelVolume] = volumeKey
		if err = d.api.createVolume(ctx, d.keyVolume(saved.Key, saved.Generation), labels); err != nil {
			err = fmt.Errorf("docker: publish generation %s of cache %s: %w", saved.Generation, saved.Key, err)
		}
	}
	if err != nil {
		d.removeVolumes([]string{gen})
		return machine.Cache{}, err
	}
	d.prune(ctx, saved)
	return saved, nil
}

// prune removes a key's pointers and generations saved before kept,
// best-effort: a generation a copy still mounts is in use, and a later save
// removes it. One saved after it, by a save racing this one, is kept, and is
// the key's current generation: of two saves at once, the one begun last
// wins, whichever publishes first.
func (d *Driver) prune(ctx context.Context, kept machine.Cache) {
	_, vols, err := d.pointers(ctx, kept.Key)
	if err != nil {
		return
	}
	// Pointers first, so a generation is never the current one while it is
	// removed.
	slices.SortStableFunc(vols, func(a, b volumeInfo) int {
		return strings.Compare(b.Labels[labelVolume], a.Labels[labelVolume])
	})
	for _, v := range vols {
		saved, err := time.Parse(time.RFC3339Nano, v.Labels[labelSaved])
		if err == nil && saved.Before(kept.Saved) && v.Labels[labelGeneration] != kept.Generation {
			_ = d.api.removeVolume(ctx, v.Name)
		}
	}
}

// DeleteCache removes every pointer of the key, then every generation of it.
// A generation a copy still mounts is left, and a key with one left lists
// nothing, since no pointer names it.
func (d *Driver) DeleteCache(ctx context.Context, key string) error {
	if err := machine.ValidateCacheKey(key); err != nil {
		return err
	}
	_, vols, err := d.pointers(ctx, key)
	if err != nil {
		return err
	}
	for _, kind := range []string{volumeKey, volumeGeneration} {
		for _, v := range vols {
			if v.Labels[labelVolume] != kind {
				continue
			}
			// A generation a copy still mounts is left for a later save.
			if err := d.api.removeVolume(ctx, v.Name); err != nil && (kind != volumeGeneration || !isConflict(err)) {
				return fmt.Errorf("docker: remove volume %s: %w", v.Name, err)
			}
		}
	}
	return nil
}
