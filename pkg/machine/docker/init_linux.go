package docker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// stageEnv tells Init which side of the user namespace it runs on.
const stageEnv = "DISCO_VM_DOCKER_STAGE"

// sigPoweroff is systemd's SIGRTMIN+4, "power off", in glibc's numbering
// (SIGRTMIN is 34 there, since glibc keeps 32 and 33).
const sigPoweroff = syscall.Signal(34 + 4)

// Init is `disco-vm docker-init`, the container's PID 1. It runs as the
// container's root, which is the host's root with Docker's default
// capabilities, and gives the guest a user namespace of its own, which Docker
// does not offer per container:
//
//  1. It delegates the container's cgroup: Init moves itself into a leaf
//     (init), and hands a sibling (guest) to the guest's root.
//  2. It starts the guest in new user, mount, pid, cgroup, uts, and ipc
//     namespaces, with guest uids 0-65535 mapped to UIDBase and up on the
//     host. The guest's side mounts what systemd needs and execs it.
//  3. It records the guest's pid as a link in relayDir, through which
//     `disco-vm pipe` in a docker exec reaches the agent's socket.
//  4. It waits. A guest that powers off ends the container; one that reboots
//     is started again. SIGTERM (docker stop) asks systemd to power off.
func Init() error {
	if os.Getenv(stageEnv) == "guest" {
		return guestStage()
	}
	if err := delegateCgroup(); err != nil {
		return fmt.Errorf("docker-init: delegate the cgroup: %w", err)
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	for {
		pid, err := startGuest()
		if err != nil {
			return fmt.Errorf("docker-init: start the guest: %w", err)
		}
		if err := linkRelay(pid); err != nil {
			return fmt.Errorf("docker-init: %w", err)
		}
		status := reap(pid, signals)
		switch {
		case status.Exited() && status.ExitStatus() == 0, status.Signaled() && status.Signal() == syscall.SIGINT:
			// systemd in a container exits for a power-off or a halt; a
			// pid namespace's init that calls reboot(2) dies of SIGINT.
			return nil
		case status.Signaled() && status.Signal() == syscall.SIGHUP:
			fmt.Fprintln(os.Stderr, "docker-init: the guest rebooted")
		default:
			return fmt.Errorf("docker-init: the guest's init ended: %s", describe(status))
		}
	}
}

// delegateCgroup moves this process out of the container's cgroup into a leaf,
// enables every controller below it, and makes the guest cgroup the guest
// root's own, so systemd can manage its subtree. Docker mounts the container's
// cgroup writable only with --security-opt writable-cgroups=true.
func delegateCgroup() error {
	root := "/sys/fs/cgroup"
	controllers, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("cgroup v2 is required: %w", err)
	}
	for _, dir := range []string{"init", "guest"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	if err := joinCgroup("init"); err != nil {
		return err
	}
	for c := range strings.FieldsSeq(string(controllers)) {
		// One at a time: a controller the kernel will not delegate here
		// should not keep the others from the guest.
		_ = os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("+"+c), 0)
	}
	guest := filepath.Join(root, "guest")
	for _, name := range []string{"", "cgroup.procs", "cgroup.threads", "cgroup.subtree_control"} {
		if err := os.Lchown(filepath.Join(guest, name), UIDBase, UIDBase); err != nil {
			return err
		}
	}
	return nil
}

func joinCgroup(dir string) error {
	return os.WriteFile(filepath.Join("/sys/fs/cgroup", dir, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0)
}

// startGuest starts this binary again in the guest's namespaces. A new cgroup
// namespace is rooted at the cgroup its creator is in, so Init steps into the
// guest cgroup to start it and back out after.
func startGuest() (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(exe, os.Args[1:]...) //nolint:gosec,noctx // G204: this binary again; noctx: the guest lives as long as the container, and reap waits for it
	cmd.Env = append(os.Environ(), stageEnv+"=guest")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	ids := []syscall.SysProcIDMap{{ContainerID: 0, HostID: UIDBase, Size: UIDCount}}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID |
			syscall.CLONE_NEWCGROUP | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC,
		// The guest's root, explicitly: a child left with Init's uid is
		// nobody in the new namespace, and execs with no capabilities.
		Credential:                 &syscall.Credential{Uid: 0, Gid: 0},
		UidMappings:                ids,
		GidMappings:                ids,
		GidMappingsEnableSetgroups: true,
	}
	if err := joinCgroup("guest"); err != nil {
		return 0, err
	}
	err = cmd.Start()
	if back := joinCgroup("init"); err == nil {
		err = back
	}
	if err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

// linkRelay points relayDir/guest at the guest's root directory, so a relay in
// a docker exec reaches the agent's socket inside the guest's mount namespace.
func linkRelay(pid int) error {
	if err := os.MkdirAll(relayDir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(relayDir, "guest.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(fmt.Sprintf("/proc/%d/root", pid), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(relayDir, "guest"))
}

// reap waits for the guest's init, reaping any other orphan PID 1 inherits,
// and asks the guest to power off when the container is told to stop.
func reap(pid int, signals <-chan os.Signal) syscall.WaitStatus {
	exited := make(chan syscall.WaitStatus, 1)
	go func() {
		for {
			var status syscall.WaitStatus
			got, err := syscall.Wait4(-1, &status, 0, nil)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil || got == pid {
				exited <- status
				return
			}
		}
	}()
	for {
		select {
		case status := <-exited:
			return status
		case <-signals:
			_ = syscall.Kill(pid, sigPoweroff)
		}
	}
}

func describe(status syscall.WaitStatus) string {
	if status.Signaled() {
		return "killed by " + status.Signal().String()
	}
	return fmt.Sprintf("exit status %d", status.ExitStatus())
}

// guestStage runs as root of the guest's user namespace, and PID 1 of its pid
// namespace. It mounts what this namespace's systemd needs over what Docker
// mounted for the container, and execs systemd.
func guestStage() error {
	type mnt struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
		optional               bool
	}
	const nosuid, nodev, noexec = unix.MS_NOSUID, unix.MS_NODEV, unix.MS_NOEXEC
	mounts := []mnt{
		// Nothing the guest mounts propagates back to the container.
		{"", "/", "", unix.MS_REC | unix.MS_PRIVATE, "", false},
		// This pid namespace's processes. The kernel allows it only because
		// Docker's /proc is not masked (systempaths=unconfined).
		{"proc", "/proc", "proc", nosuid | nodev | noexec, "", false},
		// Over Docker's /run, which hides relayDir from the guest.
		{"tmpfs", "/run", "tmpfs", nosuid | nodev, "mode=0755", false},
		// Terminals of the guest's own, with uids it owns.
		{"devpts", "/dev/pts", "devpts", nosuid | noexec, "newinstance,ptmxmode=0666,mode=0620,gid=5", false},
		{"/dev/pts/ptmx", "/dev/ptmx", "", unix.MS_BIND, "", false},
		{"mqueue", "/dev/mqueue", "mqueue", nosuid | nodev | noexec, "", true},
	}
	for _, m := range mounts {
		if err := unix.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil && !m.optional {
			return fmt.Errorf("docker-init: mount %s on %s: %w", m.fstype+m.source, m.target, err)
		}
	}
	// The guest's cgroup, as this cgroup namespace sees it, over Docker's
	// view of the same filesystem.
	if err := mountCgroup("/sys/fs/cgroup"); err != nil {
		return fmt.Errorf("docker-init: mount cgroup2 on /sys/fs/cgroup: %w", err)
	}
	for _, init := range []string{"/lib/systemd/systemd", "/usr/lib/systemd/systemd", "/sbin/init"} {
		if _, err := os.Stat(init); err == nil {
			// systemd reads $container to know it is one, and leaves the
			// hardware alone.
			return unix.Exec(init, []string{init}, []string{"container=disco-vm"})
		}
	}
	return errors.New("docker-init: the image has no init (install systemd)")
}

// mountCgroup mounts cgroup2 with the new mount API. mount(2) refuses to put
// a filesystem on a mount point whose top mount is the same filesystem, which
// Docker's cgroup mount is; move_mount(2) does not.
func mountCgroup(target string) error {
	fs, err := unix.Fsopen("cgroup2", unix.FSOPEN_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(fs)
	if err := unix.FsconfigCreate(fs); err != nil {
		return err
	}
	mnt, err := unix.Fsmount(fs, unix.FSMOUNT_CLOEXEC, unix.MOUNT_ATTR_NOSUID|unix.MOUNT_ATTR_NODEV|unix.MOUNT_ATTR_NOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(mnt)
	return unix.MoveMount(mnt, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH)
}

// Shift is `disco-vm docker-init --shift`, which Install runs once, as the
// install container's root, on the base image: it moves every file's owner
// into the guest's range, from uid N to UIDBase+N, so that in the guest's user
// namespace the files belong to the uids they always did. Layers built on the
// base are written by the guest, so they are in the range already.
//
// chown clears setuid and setgid bits and file capabilities, so they are put
// back. A hard link is shifted once, and an id already in the range is left
// as it is, so Shift can be run twice. An id outside 0-65535 becomes nobody.
func Shift() error {
	var root unix.Stat_t
	if err := unix.Lstat("/", &root); err != nil {
		return err
	}
	seen := map[uint64]bool{}
	return filepath.WalkDir("/", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return err
		}
		if st.Dev != root.Dev {
			// Another filesystem: /proc, /sys, /dev, and the files Docker
			// mounts into every container (/etc/hosts and the like).
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && st.Nlink > 1 {
			if seen[st.Ino] {
				return nil
			}
			seen[st.Ino] = true
		}
		regular := st.Mode&unix.S_IFMT == unix.S_IFREG
		var caps []byte
		if regular {
			if n, err := unix.Lgetxattr(path, "security.capability", nil); err == nil && n > 0 {
				caps = make([]byte, n)
				if n, err = unix.Lgetxattr(path, "security.capability", caps); err == nil {
					caps = caps[:n]
				} else {
					caps = nil
				}
			}
		}
		if err := unix.Lchown(path, shiftID(st.Uid), shiftID(st.Gid)); err != nil {
			return fmt.Errorf("shift %s: %w", path, err)
		}
		if regular && st.Mode&(unix.S_ISUID|unix.S_ISGID) != 0 {
			if err := unix.Chmod(path, st.Mode&0o7777); err != nil {
				return fmt.Errorf("shift %s: %w", path, err)
			}
		}
		if caps != nil {
			if err := unix.Lsetxattr(path, "security.capability", caps, 0); err != nil {
				return fmt.Errorf("shift %s: restore file capabilities: %w", path, err)
			}
		}
		return nil
	})
}

func shiftID(id uint32) int {
	switch {
	case id < UIDCount:
		return int(UIDBase + id)
	case id >= UIDBase && id < UIDBase+UIDCount:
		return int(id)
	default:
		return int(UIDBase + 65534)
	}
}
