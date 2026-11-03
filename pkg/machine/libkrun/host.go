//go:build linux

package libkrun

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// otherDrivers is what a host that cannot run libkrun is told to use instead
// (ADR 26-11-03-530 §11): disco-vm never falls back to one by itself.
const otherDrivers = "or use --driver docker or --driver boxd"

// kvmPath is KVM's device. A variable so a test can point it elsewhere.
var kvmPath = "/dev/kvm"

// Check refuses a host that cannot run libkrun, naming why: not linux/amd64,
// no KVM or none this user may open, or a cached runtime that does not run.
// It does not pull.
func (h *Hypervisor) Check(ctx context.Context) error {
	if err := checkHost(); err != nil {
		return err
	}
	dir, ok, err := h.cachedRuntime()
	if err != nil || !ok {
		return err
	}
	return checkRuntime(ctx, dir)
}

// checkHost refuses a host that is not linux/amd64, or has no KVM this user
// can open read-write.
func checkHost() error {
	if runtime.GOARCH != "amd64" {
		return fmt.Errorf("libkrun: this host is linux/%s, and libkrun runs on linux/amd64 alone for now (ADR 26-11-03-530 §10); %s", runtime.GOARCH, otherDrivers)
	}
	f, err := os.OpenFile(kvmPath, os.O_RDWR|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("libkrun: this host has no %s, so it cannot run a VM: enable KVM (a VM needs nested virtualization, and a container needs %s passed in), %s", kvmPath, kvmPath, otherDrivers)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("libkrun: this user cannot open %s read-write: add it to the kvm group (`sudo usermod -aG kvm $USER`, then log in again), %s", kvmPath, otherDrivers)
	case err != nil:
		return fmt.Errorf("libkrun: open %s: %w; %s", kvmPath, err, otherDrivers)
	}
	return f.Close()
}
