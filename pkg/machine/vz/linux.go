package vz

import (
	"fmt"

	"github.com/discobox-ai/vm/pkg/machine/linuximage"
)

// DefaultLinuxBase is the Linux guest image a fresh VM starts from when the
// driver is configured with no base (machine.Config.Base).
const DefaultLinuxBase = linuximage.DefaultBase

// AgentEnv names the disco-vm built for linux on this host's architecture,
// which every boot of a Linux guest hands the guest. A darwin disco-vm
// carries no linux one: `go tool task build` builds it beside the darwin
// one, and a release ships both.
const AgentEnv = "DISCO_VM_AGENT"

// The guest image's artifacts vz boots, and the names a bundle keeps them
// under but the root's, which it keeps as disk.img.
const (
	kernelName = linuximage.Kernel
	initrdName = linuximage.Initrd
	rootName   = linuximage.Root
)

// parseLinuxBase is vz's base: the guest image machine.Config.Base names,
// with Debian's kernel.
func parseLinuxBase(base string) (linuximage.Image, error) {
	img, err := linuximage.Parse(base, kernelName, initrdName, rootName)
	if err != nil {
		return img, fmt.Errorf("vz: %w", err)
	}
	return img, nil
}

// checkAgent refuses an agent that is not a linux executable for the guest's
// architecture, before a VM is started that could never answer.
func checkAgent(agent, arch string) error {
	if agent == "" {
		return fmt.Errorf("vz: a Linux guest needs the disco-vm built for linux/%s as its agent: set %s to it (`go tool task build` builds it in build/linux-%s)", arch, AgentEnv, arch)
	}
	if err := linuximage.CheckAgent(agent, arch); err != nil {
		return fmt.Errorf("vz: %w (%s)", err, AgentEnv)
	}
	return nil
}
