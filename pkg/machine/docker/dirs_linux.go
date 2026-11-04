package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// A key's current generation is named in currentFile, replaced by a rename,
// and lockFile is what a helper locks the key by.
const (
	currentFile = "current.json"
	lockFile    = ".lock"
)

// keys is the directory of the pool's cache keys.
func (l layout) keys() string { return path.Join(l.root, "cache", "pools", l.pool, "keys") }

func (l layout) key(key string) string { return path.Join(l.keys(), key) }

// Dirs is `disco-vm docker-volumes REQUEST`, run as the host's root in a
// helper container that bind-mounts the driver's root at helperRoot (and, to
// carry a VM from before ADR 26-11-03-782 over, one of its named volumes at
// helperOld). REQUEST is a dirsRequest as JSON; the answer is a dirsResult as
// JSON on stdout. A guarded save whose key has moved exits exitCacheMoved.
func Dirs(request string) error {
	var req dirsRequest
	if err := json.Unmarshal([]byte(request), &req); err != nil {
		return fmt.Errorf("docker-volumes: %w", err)
	}
	res, err := req.run(helperRoot)
	if err != nil {
		if errors.Is(err, machine.ErrCacheMoved) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(exitCacheMoved)
		}
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(res)
}

func (r dirsRequest) run(root string) (dirsResult, error) {
	if err := machine.ValidatePool(r.Pool); err != nil {
		return dirsResult{}, err
	}
	if r.Name != "" {
		if err := machine.ValidateName(r.Name); err != nil {
			return dirsResult{}, err
		}
	}
	if r.Key != "" {
		if err := machine.ValidateCacheKey(r.Key); err != nil {
			return dirsResult{}, err
		}
	}
	if r.Staging != "" && (filepath.Base(r.Staging) != r.Staging || !strings.HasPrefix(r.Staging, ".") || !strings.Contains(r.Staging, "--")) {
		return dirsResult{}, fmt.Errorf("docker-volumes: %q is not a staging directory", r.Staging)
	}
	l := layout{root: root, pool: r.Pool}
	needName := map[string]bool{opPrepare: true, opInstall: true, opRemove: true, opSave: true, opMigrate: true}
	if needName[r.Op] && r.Name == "" {
		return dirsResult{}, fmt.Errorf("docker-volumes: %s needs a VM's name", r.Op)
	}
	switch r.Op {
	case opPrepare:
		return r.prepare(l)
	case opInstall:
		return dirsResult{}, r.install(l)
	case opUnstage:
		if r.Staging == "" {
			return dirsResult{}, nil
		}
		return dirsResult{}, os.RemoveAll(filepath.Join(l.vms(machine.VolumeCache), r.Staging))
	case opRemove:
		return dirsResult{}, r.remove(l)
	case opSave:
		return r.save(l)
	case opCaches:
		caches, err := l.caches()
		return dirsResult{Caches: caches}, err
	case opDeleteCache:
		return dirsResult{}, r.deleteCache(l)
	case opMigrate:
		return dirsResult{}, r.migrate(l)
	default:
		return dirsResult{}, fmt.Errorf("docker-volumes: no operation %q", r.Op)
	}
}

// prepare makes the VM's directories, as they are if they exist, and clones
// its key's current generation into a staging directory of its own, under a
// shared lock on the key, so no save prunes the generation meanwhile.
func (r dirsRequest) prepare(l layout) (dirsResult, error) {
	var res dirsResult
	if r.Data {
		if err := os.MkdirAll(l.vm(machine.VolumeData, r.Name), 0o755); err != nil {
			return res, err
		}
	}
	if r.Key == "" {
		return res, nil
	}
	if err := os.MkdirAll(l.vm(machine.VolumeCache, r.Name), 0o755); err != nil {
		return res, err
	}
	res.Staging = "." + r.Name + "--" + fsutil.RandomHex(4)
	staging := filepath.Join(l.vms(machine.VolumeCache), res.Staging)
	unlock, err := l.lock(r.Key, unix.LOCK_SH)
	if err != nil {
		return res, err
	}
	defer unlock()
	cur, found, err := l.current(r.Key)
	if err == nil && found {
		res.Generation = cur.Generation
		err = copyTree(filepath.Join(l.key(r.Key), cur.Generation), staging)
	} else if err == nil {
		err = os.Mkdir(staging, 0o755)
	}
	if err != nil {
		_ = os.RemoveAll(staging)
		return dirsResult{}, err
	}
	return res, nil
}

// install empties the VM's data directory and puts its staged cache clone in
// place. It runs once the VM's container holds the name, so what a create
// that lost its container left is never a later VM's.
func (r dirsRequest) install(l layout) error {
	if r.Data {
		if err := freshDir(l.vm(machine.VolumeData, r.Name)); err != nil {
			return err
		}
	}
	if r.Key == "" {
		return nil
	}
	dir := l.vm(machine.VolumeCache, r.Name)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if r.Staging == "" {
		return os.MkdirAll(dir, 0o755)
	}
	return os.Rename(filepath.Join(l.vms(machine.VolumeCache), r.Staging), dir)
}

// remove removes the VM's directories and any staging directory of its name.
func (r dirsRequest) remove(l layout) error {
	for _, v := range []machine.Volume{machine.VolumeData, machine.VolumeCache} {
		if err := os.RemoveAll(l.vm(v, r.Name)); err != nil {
			return err
		}
	}
	staged, err := filepath.Glob(filepath.Join(l.vms(machine.VolumeCache), "."+r.Name+"--*"))
	if err != nil {
		return err
	}
	for _, dir := range staged {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}

// save copies the VM's cache directory (or, for a VM from before ADR
// 26-11-03-782, its named volume at helperOld) into a hidden directory of
// the key, then, under an exclusive lock on the key, checks the guard,
// renames the copy into place as the new generation, points the key at it,
// and removes the key's other generations. A guard is checked before the
// copy too, so a refused save copies nothing.
func (r dirsRequest) save(l layout) (dirsResult, error) {
	if r.Generation == "" || filepath.Base(r.Generation) != r.Generation || strings.HasPrefix(r.Generation, ".") {
		return dirsResult{}, fmt.Errorf("docker-volumes: %q is not a generation", r.Generation)
	}
	src := l.vm(machine.VolumeCache, r.Name)
	if r.FromOld {
		src = helperOld
	}
	dir := l.key(r.Key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dirsResult{}, err
	}
	if err := r.guard(l); err != nil {
		return dirsResult{}, err
	}
	tmp := filepath.Join(dir, savePrefix+r.Generation)
	// The copy is held by a lock of its own while this save lives, so a
	// prune tells it from what a save that died left (sweepSaves).
	held, err := os.OpenFile(tmp+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return dirsResult{}, err
	}
	defer func() {
		_ = os.Remove(tmp + ".lock")
		_ = held.Close()
	}()
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil {
		return dirsResult{}, err
	}
	if err := copyTree(src, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return dirsResult{}, err
	}
	unlock, err := l.lock(r.Key, unix.LOCK_EX)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return dirsResult{}, err
	}
	defer unlock()
	if err := r.guard(l); err != nil {
		_ = os.RemoveAll(tmp)
		return dirsResult{}, err
	}
	saved := machine.Cache{Key: r.Key, Generation: r.Generation, Bytes: r.Bytes, Saved: time.Now().UTC()}
	if err := os.Rename(tmp, filepath.Join(dir, r.Generation)); err != nil {
		_ = os.RemoveAll(tmp)
		return dirsResult{}, err
	}
	if err := fsutil.WriteJSON(filepath.Join(dir, currentFile), saved); err != nil {
		return dirsResult{}, err
	}
	if err := l.prune(r.Key, r.Generation); err != nil {
		return dirsResult{}, err
	}
	return dirsResult{Saved: saved}, nil
}

// guard refuses a guarded save whose key does not hold the generation it
// expects.
func (r dirsRequest) guard(l layout) error {
	if !r.Guarded {
		return nil
	}
	cur, found, err := l.current(r.Key)
	switch {
	case err != nil:
		return err
	case cur.Generation == r.IfGeneration:
		return nil
	case !found:
		return fmt.Errorf("cache %s holds no generation, not %s: %w", r.Key, r.IfGeneration, machine.ErrCacheMoved)
	default:
		return fmt.Errorf("cache %s holds generation %s: %w", r.Key, cur.Generation, machine.ErrCacheMoved)
	}
}

// deleteCache drops the key's pointer, then its generations, under an
// exclusive lock. The key's directory and lock stay, so a process waiting on
// the lock never holds one nobody else sees.
func (r dirsRequest) deleteCache(l layout) error {
	if _, err := os.Stat(l.key(r.Key)); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	unlock, err := l.lock(r.Key, unix.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(filepath.Join(l.key(r.Key), currentFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return l.prune(r.Key, "")
}

// migrate copies a named volume of a VM from before ADR 26-11-03-782,
// mounted at helperOld, into the VM's directory for it, which it replaces.
func (r dirsRequest) migrate(l layout) error {
	v := machine.Volume(r.Volume)
	if v != machine.VolumeData && v != machine.VolumeCache {
		return fmt.Errorf("docker-volumes: no volume %q", r.Volume)
	}
	dir := l.vm(v, r.Name)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return copyTree(helperOld, dir)
}

// lock takes a flock on the key's lock file, which every helper on the
// daemon's host shares, and returns its release.
func (l layout) lock(key string, how int) (func(), error) {
	if err := os.MkdirAll(l.key(key), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(l.key(key), lockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock cache %s: %w", key, err)
	}
	return func() { _ = f.Close() }, nil
}

// current is the generation the key points at, if any, and whole: a pointer
// is written only once its generation has been renamed into place.
func (l layout) current(key string) (machine.Cache, bool, error) {
	var cur machine.Cache
	err := fsutil.ReadJSON(filepath.Join(l.key(key), currentFile), &cur)
	if errors.Is(err, fs.ErrNotExist) {
		return machine.Cache{}, false, nil
	}
	if err != nil {
		return machine.Cache{}, false, err
	}
	if _, err := os.Stat(filepath.Join(l.key(key), cur.Generation)); errors.Is(err, fs.ErrNotExist) {
		return machine.Cache{}, false, nil
	} else if err != nil {
		return machine.Cache{}, false, err
	}
	return cur, true, nil
}

// savePrefix starts the name of a save's copy until it is renamed into place
// as a generation, and of the lock that copy is held by.
const savePrefix = ".save-"

// prune removes every generation of the key but kept, and the copies of
// saves that died (sweepSaves). Other hidden entries (the lock, a live
// save's copy) are left.
func (l layout) prune(key, kept string) error {
	if err := l.sweepSaves(key); err != nil {
		return err
	}
	entries, err := os.ReadDir(l.key(key))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || e.Name() == currentFile || e.Name() == kept {
			continue
		}
		if err := os.RemoveAll(filepath.Join(l.key(key), e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// sweepSaves removes the copies of saves that died before they published,
// which no save holds the lock of: a copy whose lock this takes, or that
// has no lock at all, since a save takes its lock before it copies.
func (l layout) sweepSaves(key string) error {
	dir := l.key(key)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, savePrefix) || strings.HasSuffix(name, ".lock") {
			continue
		}
		copied := filepath.Join(dir, name)
		f, err := os.OpenFile(copied+".lock", os.O_RDWR, 0)
		if err == nil {
			if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
				_ = f.Close()
				continue // a save still copies into it
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		err = os.RemoveAll(copied)
		_ = os.Remove(copied + ".lock")
		if f != nil {
			_ = f.Close()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// caches lists the pool's keys that point at a generation.
func (l layout) caches() ([]machine.Cache, error) {
	entries, err := os.ReadDir(l.keys())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []machine.Cache
	for _, e := range entries {
		if !e.IsDir() || machine.ValidateCacheKey(e.Name()) != nil {
			continue
		}
		cur, found, err := l.current(e.Name())
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, cur)
		}
	}
	slices.SortFunc(out, func(a, b machine.Cache) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

func freshDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

// copyTree copies the tree src to dst, which must not exist, as it is:
// owners (the guest's shifted uids), modes, times, symlinks, hard links,
// and special files.
func copyTree(src, dst string) error {
	links := map[uint64]string{}
	var dirs []string
	err := filepath.WalkDir(src, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return err
		}
		mode := st.Mode & unix.S_IFMT
		if mode != unix.S_IFDIR && st.Nlink > 1 {
			if first, ok := links[st.Ino]; ok {
				return os.Link(first, target)
			}
			links[st.Ino] = target
		}
		switch mode {
		case unix.S_IFDIR:
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, path)
		case unix.S_IFLNK:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil { //nolint:gosec // G122: target is under the helper's own new directory, which nothing else writes
				return err
			}
		case unix.S_IFREG:
			if err := copyFile(path, target); err != nil {
				return err
			}
		default:
			if err := unix.Mknod(target, st.Mode, int(st.Rdev)); err != nil {
				return err
			}
		}
		if mode == unix.S_IFDIR {
			return nil
		}
		return copyMeta(target, &st)
	})
	if err != nil {
		return err
	}
	// Directories last, deepest first, so their times survive their
	// contents being written.
	for _, path := range slices.Backward(dirs) {
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return err
		}
		if err := copyMeta(filepath.Join(dst, rel), &st); err != nil {
			return err
		}
	}
	return nil
}

// copyMeta gives target st's owner, then its mode (a chown clears setuid),
// then its times.
func copyMeta(target string, st *unix.Stat_t) error {
	if err := os.Lchown(target, int(st.Uid), int(st.Gid)); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		if err := unix.Chmod(target, st.Mode&0o7777); err != nil {
			return err
		}
	}
	times := []unix.Timespec{st.Atim, st.Mtim}
	return unix.UtimesNanoAt(unix.AT_FDCWD, target, times, unix.AT_SYMLINK_NOFOLLOW)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
