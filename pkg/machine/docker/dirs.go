package docker

import (
	"path"

	"github.com/discobox-ai/vm/pkg/machine"
)

// A VM's volumes and a pool's cache generations are directories on the
// daemon's host (ADR 26-11-03-782 §6), under the driver's root (Driver.Root,
// DefaultRoot), laid out as discobox's own docker driver lays out its tree:
//
//	data/pools/<pool>/vms/<name>              a VM's data volume
//	cache/pools/<pool>/vms/<name>             a VM's cache clone
//	cache/pools/<pool>/vms/.<name>--<random>  a clone being staged
//	cache/pools/<pool>/keys/<key>/<generation> a key's generation
//	cache/pools/<pool>/keys/<key>/current.json the key's pointer
//	cache/pools/<pool>/keys/<key>/.lock        the key's flock
//
// The daemon may be remote, so the driver never touches them itself: a
// helper container of the agent alone (helperRepo), bind-mounting the root,
// runs `disco-vm docker-volumes` (Dirs) for each step.

const (
	// DefaultRoot is where the driver keeps its directories on the daemon's
	// host when neither Driver.Root nor RootEnv names another.
	DefaultRoot = "/var/lib/discobox"
	// RootEnv overrides DefaultRoot.
	RootEnv = "DISCO_VM_DOCKER_ROOT"

	// helperRoot is where a helper container mounts the driver's root, and
	// helperOld a named volume of a VM from before ADR 26-11-03-782.
	helperRoot = "/discobox"
	helperOld  = "/old"
	// exitCacheMoved is a helper's exit status for a guarded save whose key
	// has moved.
	exitCacheMoved = 3

	opPrepare     = "prepare"
	opInstall     = "install"
	opUnstage     = "unstage"
	opRemove      = "remove"
	opSave        = "save"
	opCaches      = "caches"
	opDeleteCache = "delete-cache"
	opMigrate     = "migrate"
)

// dirsRequest is one step a helper takes, as JSON on its command line.
type dirsRequest struct {
	Op   string `json:"op"`
	Pool string `json:"pool"`
	// Name is the VM's.
	Name string `json:"name,omitempty"`
	// Data is that the VM has a data volume; Key, a cache key, that it has a
	// cache volume, or the key a save or a delete names.
	Data bool   `json:"data,omitempty"`
	Key  string `json:"key,omitempty"`
	// Staging is the clone prepare staged, which install puts in place and
	// unstage removes.
	Staging string `json:"staging,omitempty"`
	// Generation, Guarded, IfGeneration, and Bytes are a save's; FromOld
	// saves the named volume mounted at helperOld.
	Generation   string `json:"generation,omitempty"`
	Guarded      bool   `json:"guarded,omitempty"`
	IfGeneration string `json:"ifGeneration,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
	FromOld      bool   `json:"fromOld,omitempty"`
	// Volume is the volume migrate copies helperOld into.
	Volume string `json:"volume,omitempty"`
}

// dirsResult is a helper's answer, as JSON on its stdout.
type dirsResult struct {
	Generation string          `json:"generation,omitempty"`
	Staging    string          `json:"staging,omitempty"`
	Caches     []machine.Cache `json:"caches,omitempty"`
	Saved      machine.Cache   `json:"saved,omitzero"`
}

// layout is where the driver's directories are under its root. Its paths
// are the daemon's host's, which is linux.
type layout struct {
	root, pool string
}

// vms is the directory of the pool's VMs' directories for volume v.
func (l layout) vms(v machine.Volume) string {
	return path.Join(l.root, string(v), "pools", l.pool, "vms")
}

func (l layout) vm(v machine.Volume, name string) string { return path.Join(l.vms(v), name) }
