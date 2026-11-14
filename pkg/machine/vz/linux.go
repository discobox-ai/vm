package vz

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/agent"
	"github.com/discobox-ai/vm/pkg/machine/linuximage"
)

// DefaultLinuxBase is the Linux guest image a fresh VM starts from when the
// driver is configured with no base (machine.Config.Base).
const DefaultLinuxBase = linuximage.DefaultBase

// AgentEnv is where the command line reads Config.Agent from.
const AgentEnv = "DISCO_VM_AGENT"

// Config is the vz driver's configuration (machine.Config.DriverConfig).
type Config struct {
	// Agent is the disco-vm built for linux on this host's architecture,
	// which every boot of a Linux guest hands the guest. A darwin disco-vm
	// carries no linux one: `go tool task build` builds it beside the
	// darwin one, and a release ships both.
	Agent string `json:"agent,omitempty"`
	// MacOSAgent is the disco-vm built for darwin on this Mac's
	// architecture, which every boot of a macOS guest attaches on a
	// read-only drive as its agent (ADR 26-11-13-318 §3). The command line
	// sets it to itself; an embedder names one.
	MacOSAgent string `json:"macosAgent,omitempty"`
}

// Definition describes the vz driver and its configuration.
func Definition() machine.Definition {
	return machine.Definition{
		Description: "macOS VMs installed from an IPSW, and Linux VMs from a guest image, on Virtualization.framework.",
		Base: &machine.ConfigField{Label: "Base", Type: machine.ConfigString, Env: machine.BaseEnv, Default: DefaultLinuxBase, Privileged: true,
			Description: "The Linux guest image a fresh VM is made from: a registry reference, or an absolute directory on this Mac whose kernel and root are booted."},
		ConfigFields: []machine.ConfigField{
			{Key: "agent", Label: "Agent", Type: machine.ConfigString, Env: AgentEnv, Placeholder: "/path/to/build/linux-arm64/disco-vm", Privileged: true,
				Description: "The absolute path of the disco-vm built for linux on this Mac's architecture, which every boot of a Linux guest hands the guest as its agent. A Linux guest needs it."},
			{Key: "macosAgent", Label: "macOS agent", Type: machine.ConfigString, Env: MacOSAgentEnv, Placeholder: "/path/to/build/disco-vm", Privileged: true, DefaultExecutable: true,
				Description: "The absolute path of the disco-vm built for darwin on this Mac's architecture, which every boot of a macOS guest, and its install, attaches on a read-only drive as its agent. A macOS guest needs it; the command line is one, and sets it to itself."},
		},
	}
}

// Validate checks the vz driver's configuration.
func Validate(config json.RawMessage) error {
	_, err := decode(config)
	return err
}

func decode(config json.RawMessage) (Config, error) {
	cfg, err := machine.DecodeConfig[Config]("vz", config)
	if err == nil && cfg.Agent != "" && !filepath.IsAbs(cfg.Agent) {
		err = errors.New("vz: configuration: agent: not an absolute path")
	}
	if err == nil && cfg.MacOSAgent != "" && !filepath.IsAbs(cfg.MacOSAgent) {
		err = errors.New("vz: configuration: macosAgent: not an absolute path")
	}
	return cfg, err
}

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
func checkAgent(linuxAgent, arch string) error {
	if linuxAgent == "" {
		return fmt.Errorf("vz: a Linux guest needs the disco-vm built for linux/%s as its agent: configure agent ($%s) as it (`go tool task build` builds it in build/linux-%s)", arch, AgentEnv, arch)
	}
	if err := agent.Check(linuxAgent, "linux", arch); err != nil {
		return fmt.Errorf("vz: %w (%s)", err, AgentEnv)
	}
	return nil
}
