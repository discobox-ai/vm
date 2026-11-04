package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// A VM's data volume and cache clone are directories on the daemon's host,
// bind-mounted at their machine.VolumePath (dirs.go), which helper
// containers make, copy, and remove. Bytes are a hint: a directory has no
// quota. A VM made before ADR 26-11-03-782 mounts named Docker volumes
// instead, `dvm-<pool>--<name>--data-<random>` and `--cache-<random>`: it
// boots with them, SaveCache reads its cache from one, Upgrade copies them
// into its directories, and Destroy removes them, found by their labels.
// Cache generations saved as named volumes then are not read.

const (
	// labelVolume says what a named volume of a VM from before ADR
	// 26-11-03-782 is (volumeData, volumeCache), and marks a helper
	// container (volumeHelper).
	labelVolume = "ai.discobox.vm.volume"
	// labelData is a VM's data volume's size, on its container; empty or
	// absent, it has none. Every volume label of a container is set at
	// Create, empty where it has no such volume, since a container inherits
	// its image's labels.
	labelData = "ai.discobox.vm.data"
	// labelCache is the key a VM's cache volume was cloned from, on its
	// container.
	labelCache      = "ai.discobox.vm.cache"
	labelCacheBytes = "ai.discobox.vm.cache-bytes"
	// labelGeneration is the generation a VM's cache volume was cloned from.
	labelGeneration = "ai.discobox.vm.generation"

	volumeData   = "data"
	volumeCache  = "cache"
	volumeHelper = "helper"

	// helperRepo names the helper image: the agent alone, at /disco-vm,
	// tagged with the start of its SHA-256.
	helperRepo  = "disco-vm-helper"
	helperAgent = "/disco-vm"
)

// layout is the driver's directories under its root, for its pool.
func (d *Driver) layout() layout { return layout{root: d.Root, pool: d.pool} }

// dirMounts bind-mounts a VM's directories at their paths.
func (d *Driver) dirMounts(name string, data, cache bool) []mount {
	var out []mount
	for v, has := range map[machine.Volume]bool{machine.VolumeData: data, machine.VolumeCache: cache} {
		if has {
			out = append(out, bindMount(d.layout().vm(v, name), machine.VolumePath(machine.Linux, v)))
		}
	}
	return out
}

// hasDirs reports whether a container mounts a VM's directories.
func hasDirs(c containerInfo) bool {
	for _, m := range c.Mounts {
		if m.Type == "bind" && (m.Destination == machine.VolumePath(machine.Linux, machine.VolumeData) ||
			m.Destination == machine.VolumePath(machine.Linux, machine.VolumeCache)) {
			return true
		}
	}
	return false
}

// oldVolume is the named volume a container from before ADR 26-11-03-782
// mounts for v, or "".
func oldVolume(c containerInfo, v machine.Volume) string {
	for _, m := range c.Mounts {
		if m.Type == "volume" && m.Destination == machine.VolumePath(machine.Linux, v) {
			return m.Name
		}
	}
	return ""
}

// volumes are what Create makes for a VM: its mounts, the labels its
// container carries for them, and the cache clone staged for it.
type volumes struct {
	data, cache bool
	mounts      []mount
	labels      map[string]string
	staging     string
}

// prepareVolumes makes the spec's directories, as they are if they exist,
// and stages a clone of its key's current generation. Neither is the VM's
// until installVolumes, once its container holds the name.
func (d *Driver) prepareVolumes(ctx context.Context, spec machine.InstanceSpec) (volumes, error) {
	vs := volumes{data: spec.Data != nil, cache: spec.Cache != nil, labels: map[string]string{}}
	if !vs.data && !vs.cache {
		return vs, nil
	}
	req := dirsRequest{Op: opPrepare, Name: spec.Name, Data: vs.data}
	if vs.data {
		vs.labels[labelData] = strconv.FormatInt(spec.Data.Bytes, 10)
	}
	if vs.cache {
		if err := machine.ValidateCacheKey(spec.Cache.Key); err != nil {
			return vs, err
		}
		req.Key = spec.Cache.Key
		vs.labels[labelCache] = spec.Cache.Key
		vs.labels[labelCacheBytes] = strconv.FormatInt(spec.Cache.Bytes, 10)
	}
	res, err := d.runDirs(ctx, req, "")
	if err != nil {
		return vs, err
	}
	vs.staging = res.Staging
	if res.Generation != "" {
		vs.labels[labelGeneration] = res.Generation
	}
	vs.mounts = d.dirMounts(spec.Name, vs.data, vs.cache)
	return vs, nil
}

// installVolumes empties the VM's data directory and puts its staged clone
// in place, which only the Create whose container holds the name does.
func (d *Driver) installVolumes(ctx context.Context, spec machine.InstanceSpec, vs volumes) error {
	if !vs.data && !vs.cache {
		return nil
	}
	req := dirsRequest{Op: opInstall, Name: spec.Name, Data: vs.data, Staging: vs.staging}
	if vs.cache {
		req.Key = spec.Cache.Key
	}
	_, err := d.runDirs(ctx, req, "")
	return err
}

// unstage is best-effort cleanup of a clone Create staged; none is nothing.
func (d *Driver) unstage(staging string) {
	if staging == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = d.runDirs(ctx, dirsRequest{Op: opUnstage, Staging: staging}, "")
}

// helper is the helper image, imported from the agent if the daemon does not
// have it yet. Helpers of other agents are removed, best-effort.
func (d *Driver) helper(ctx context.Context) (string, error) {
	if d.Agent == "" {
		return "", fmt.Errorf("docker: the driver keeps volumes and caches with the disco-vm agent built for linux: set %s to one", AgentEnv)
	}
	data, err := os.ReadFile(d.Agent)
	if err != nil {
		return "", fmt.Errorf("docker: agent: %w", err)
	}
	sum := sha256.Sum256(data)
	tag := hex.EncodeToString(sum[:])[:16]
	ref := helperRepo + ":" + tag
	if _, err := d.api.inspectImage(ctx, ref); err == nil {
		return ref, nil
	} else if !isNotFound(err) {
		return "", err
	}
	in, err := d.api.info(ctx)
	if err != nil {
		return "", err
	}
	if err := checkArch(d.Agent, in.Architecture); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(helperAgent, "/"), Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(data))}); err != nil {
		return "", err
	}
	if _, err := tw.Write(data); err != nil {
		return "", err
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := d.api.importImage(ctx, helperRepo, tag, &buf); err != nil {
		return "", err
	}
	// Another version's helper is left: another disco-vm on the daemon (an
	// embedder's engine beside a command line) may be using it.
	return ref, nil
}

// runDirs takes one step in a helper container that bind-mounts the root,
// and old, a named volume, at helperOld where it is not empty.
func (d *Driver) runDirs(ctx context.Context, req dirsRequest, old string) (dirsResult, error) {
	if !path.IsAbs(d.Root) {
		return dirsResult{}, fmt.Errorf("docker: the driver's root %q (%s) is not an absolute path", d.Root, RootEnv)
	}
	req.Pool = d.pool
	image, err := d.helper(ctx)
	if err != nil {
		return dirsResult{}, err
	}
	arg, err := json.Marshal(req)
	if err != nil {
		return dirsResult{}, err
	}
	hc := hostConfig{Binds: []string{d.Root + ":" + helperRoot}, NetworkMode: "none"}
	if old != "" {
		hc.Mounts = []mount{volumeMount(old, helperOld, true)}
	}
	name := machine.ResourcePrefix(namePrefix, d.pool) + "helper--" + fsutil.RandomHex(4)
	id, err := d.api.createContainer(ctx, name, containerConfig{
		Image:      image,
		Entrypoint: []string{helperAgent, "docker-volumes", string(arg)},
		Cmd:        []string{},
		User:       "0",
		Labels:     map[string]string{labelPool: d.pool, labelVolume: volumeHelper},
		HostConfig: hc,
	})
	if err != nil {
		return dirsResult{}, fmt.Errorf("docker: create %s: %w", name, err)
	}
	defer d.removeContainer(id)
	if err := d.api.start(ctx, id); err != nil {
		return dirsResult{}, fmt.Errorf("docker: start %s: %w", name, err)
	}
	code, err := d.api.wait(ctx, id)
	if err != nil {
		return dirsResult{}, err
	}
	stdout, stderr, err := d.api.output(ctx, id)
	if err != nil {
		return dirsResult{}, err
	}
	switch code {
	case 0:
		var res dirsResult
		if err := json.Unmarshal([]byte(stdout), &res); err != nil {
			return dirsResult{}, fmt.Errorf("docker: %s answered %q: %w", req.Op, stdout, err)
		}
		return res, nil
	case exitCacheMoved:
		return dirsResult{}, fmt.Errorf("%s: %w", strings.TrimSpace(stderr), machine.ErrCacheMoved)
	default:
		return dirsResult{}, fmt.Errorf("docker: %s under %s exited %d: %s", req.Op, d.Root, code, strings.TrimSpace(stderr))
	}
}

// volumesOf reads a VM's data and cache volumes from its container's labels.
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

// removeOldVolumes removes the named data and cache volumes of every VM of
// the name made before ADR 26-11-03-782, found by their labels, so a
// Destroy that failed once its container was gone finds them again. One a
// container still mounts is in use, and is left.
func (d *Driver) removeOldVolumes(ctx context.Context, name string) error {
	vols, err := d.api.volumes(ctx, map[string]string{labelPool: d.pool, labelInstance: name})
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

// Caches lists each of the pool's keys with its current generation.
func (d *Driver) Caches(ctx context.Context) ([]machine.Cache, error) {
	res, err := d.runDirs(ctx, dirsRequest{Op: opCaches}, "")
	return res.Caches, err
}

// SaveCache copies the stopped VM's cache directory (or the named volume of
// a VM from before ADR 26-11-03-782) into a new generation on the daemon's
// host, then, under the key's lock, checks a guarded save, publishes the
// generation by a rename, and removes the key's others.
func (d *Driver) SaveCache(ctx context.Context, inst machine.Instance, save machine.CacheSave) (machine.Cache, error) {
	if err := refuseCommitted(inst); err != nil {
		return machine.Cache{}, err
	}
	if err := machine.ValidateCacheKey(save.Key); err != nil {
		return machine.Cache{}, err
	}
	c, err := d.resolve(ctx, inst)
	if err != nil {
		return machine.Cache{}, fmt.Errorf("docker: container %s: %w", containerName(d.pool, inst.Name), err)
	}
	if c.State.Running {
		return machine.Cache{}, fmt.Errorf("instance %s is running; stop it first", inst.Name)
	}
	cur := d.instance(c)
	if cur.Cache == nil {
		return machine.Cache{}, fmt.Errorf("instance %s has no cache volume", inst.Name)
	}
	old := oldVolume(c, machine.VolumeCache)
	res, err := d.runDirs(ctx, dirsRequest{
		Op: opSave, Name: inst.Name, Key: save.Key, Generation: fsutil.RandomHex(8),
		Guarded: save.Guarded, IfGeneration: save.IfGeneration, Bytes: cur.Cache.Bytes, FromOld: old != "",
	}, old)
	if err != nil {
		return machine.Cache{}, err
	}
	return res.Saved, nil
}

// DeleteCache drops the key's pointer, then its generations. A VM's clone is
// its own, and is kept.
func (d *Driver) DeleteCache(ctx context.Context, key string) error {
	if err := machine.ValidateCacheKey(key); err != nil {
		return err
	}
	_, err := d.runDirs(ctx, dirsRequest{Op: opDeleteCache, Key: key}, "")
	return err
}

// errNoContainer is a VM whose container the daemon does not have.
var errNoContainer = &apiError{Status: 404, Message: "no such container"}
