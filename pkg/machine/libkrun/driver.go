//go:build linux

package libkrun

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/linuximage"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

func init() {
	// The base is checked where a fresh VM is made from it (Prepare), so a
	// moved guest-image directory stops nothing but that.
	machine.Register("libkrun", machine.Registration{
		New: func(cfg machine.Config) (machine.Driver, error) {
			conf, err := decode(cfg.DriverConfig)
			if err != nil {
				return nil, err
			}
			h := newHypervisor(cfg, conf)
			d, err := local.New(cfg, h)
			if err != nil {
				return nil, err
			}
			h.cacheDir = d.CacheDir()
			return d, nil
		},
		Definition: Definition(),
		Validate:   Validate,
	})
}

// Hypervisor runs Linux guests on libkrun, under a local.Driver: fresh from
// the Linux guest image (machine.Config.Base), or a copy of a committed VM.
type Hypervisor struct {
	// base is the Linux guest image a fresh VM starts from, as
	// linuximage.Parse reads it; empty is linuximage.DefaultBase.
	base string
	// runtimeDir is a runtime directory used as it is (Config.Runtime), or
	// empty for DefaultRuntime, pulled into the cache.
	runtimeDir string
	// agent is the disco-vm a boot hands the guest (Config.Agent), or empty
	// for the running one.
	agent string
	// transport calls a registry for the base and the runtime
	// (machine.Config.HTTPTransport): through an embedder's dialer, where
	// one is set.
	transport http.RoundTripper
	// cacheDir is the driver's cache (cache/libkrun), where Check looks for
	// a pulled runtime. Prepare and Boot are given it in their InstanceSpec.
	cacheDir string
}

// newHypervisor is the Hypervisor cfg and its decoded driver configuration
// describe, its cache directory left for local.New's driver to give it.
func newHypervisor(cfg machine.Config, conf Config) *Hypervisor {
	return &Hypervisor{base: cfg.Base, runtimeDir: conf.Runtime, agent: conf.Agent, transport: cfg.HTTPTransport()}
}

var _ local.Hypervisor = (*Hypervisor)(nil)

func (*Hypervisor) Name() string { return "libkrun" }

// Capabilities: Linux guests from the base, with no install, no cap on how
// many run, commits and copies of them, virtiofs shares, the agent handed in
// at every boot, the agent's and the service's guest ports and forwards out,
// and no network device as the empty fence. No keys, no isolation, and no
// volumes yet, so no upgrade.
func (*Hypervisor) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS:           []machine.OS{machine.Linux},
		Commit:            true,
		SharedDirectories: true,
		Ports:             true,
		AcrossPools:       true,
		Agent:             true,
		Keys:              machine.KeyCapabilities{Mode: machine.KeysNone},
		Egress:            []machine.EgressFence{machine.EgressNone},
	}
}

// Chains is false: a VM's files are its own copy of the committed VM's.
func (*Hypervisor) Chains() bool { return false }

// Base is Linux: a fresh VM is a Linux guest from the configured guest image.
func (*Hypervisor) Base() (machine.OS, bool) { return machine.Linux, true }

// SharesLoopback is false: a guest reaches neither another guest nor the
// host's loopback.
func (*Hypervisor) SharesLoopback() bool { return false }

// Install is unsupported: a libkrun VM starts from the base.
func (*Hypervisor) Install(context.Context, local.InstallSpec, local.Layer) error {
	return fmt.Errorf("libkrun: install: %w", machine.ErrUnsupported)
}

// Commit freezes the stopped VM's bundle in place: its kernel, initrd, disk,
// and krun.json stay, and everything else goes, its identity among them,
// since every copy gets its own.
func (*Hypervisor) Commit(_ context.Context, inst local.InstanceSpec) error {
	return bundle(inst.Dir).freeze()
}

// MoveVolumes is unsupported: libkrun attaches no volume yet, so a VM has no
// upgrade (ADR 26-11-03-782).
func (*Hypervisor) MoveVolumes(context.Context, local.InstanceSpec, local.InstanceSpec) error {
	return fmt.Errorf("libkrun: moving volumes: %w", machine.ErrUnsupported)
}

// SaveCache is unsupported: libkrun attaches no volume yet (ADR
// 26-11-03-530, Deferred).
func (*Hypervisor) SaveCache(context.Context, local.InstanceSpec, local.Layer) error {
	return fmt.Errorf("libkrun: a cache volume: %w", machine.ErrUnsupported)
}

func (*Hypervisor) Destroy(_ context.Context, inst local.InstanceSpec) error {
	return os.RemoveAll(inst.Dir)
}

// Service is guest.ServicePort over vsock; the instance's shim serves it on
// the host's loopback.
func (*Hypervisor) Service(local.InstanceSpec) (string, uint32, error) {
	return fmt.Sprintf("vsock:%d", guest.ServicePort), guest.ServicePort, nil
}

// parseBase is libkrun's base: the guest image machine.Config.Base names,
// with the kernel it carries for libkrun.
func parseBase(base string) (linuximage.Image, error) {
	img, err := linuximage.Parse(base, linuximage.KrunKernel, linuximage.Initrd, linuximage.Root)
	if err != nil {
		return img, fmt.Errorf("libkrun: %w", err)
	}
	return img, nil
}
