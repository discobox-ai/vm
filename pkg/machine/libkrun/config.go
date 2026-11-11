//go:build linux

package libkrun

import (
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/linuximage"
)

// RuntimeEnv is where the command line reads Config.Runtime from.
const RuntimeEnv = "DISCO_VM_KRUN_RUNTIME"

// AgentEnv is where the command line reads Config.Agent from.
const AgentEnv = "DISCO_VM_AGENT"

// Config is the libkrun driver's configuration (machine.Config.DriverConfig).
type Config struct {
	// Runtime is a runtime directory to use as it is, in place of
	// DefaultRuntime: one `go tool task build:krun-runtime` built from the
	// checkout. It must be absolute.
	Runtime string `json:"runtime,omitempty"`
	// Agent is the disco-vm a boot hands the guest as its agent, in place
	// of the running binary: a test's, whose own binary is not disco-vm.
	Agent string `json:"agent,omitempty"`
}

// Definition describes the libkrun driver and its configuration.
func Definition() machine.Definition {
	return machine.Definition{
		Description: "Linux VMs as libkrun microVMs on KVM, from a Linux guest image.",
		Base: machine.ConfigField{Label: "Base", Type: machine.ConfigString, Env: machine.BaseEnv, Default: linuximage.DefaultBase, Privileged: true,
			Description: "The Linux guest image a fresh VM is made from: a registry reference, or an absolute directory on this host whose kernel and root are booted."},
		ConfigFields: []machine.ConfigField{
			{Key: "runtime", Label: "Runtime", Type: machine.ConfigString, Env: RuntimeEnv, Advanced: true, Privileged: true,
				Description: "An absolute runtime directory (the launcher, passt, and libkrun) to run as it is; empty is " + DefaultRuntime + ", pulled into the cache."},
			{Key: "agent", Label: "Agent", Type: machine.ConfigString, Env: AgentEnv, Advanced: true, Privileged: true,
				Description: "The absolute path of the static linux disco-vm a boot hands the guest as its agent; empty is this binary."},
		},
	}
}

func decode(config json.RawMessage) (Config, error) {
	cfg, err := machine.DecodeConfig[Config]("libkrun", config)
	if err != nil {
		return cfg, err
	}
	if cfg.Runtime != "" && !filepath.IsAbs(cfg.Runtime) {
		return cfg, errors.New("libkrun: configuration: runtime: not an absolute directory")
	}
	if cfg.Agent != "" && !filepath.IsAbs(cfg.Agent) {
		return cfg, errors.New("libkrun: configuration: agent: not an absolute path")
	}
	return cfg, nil
}

// Validate checks the libkrun driver's configuration.
func Validate(config json.RawMessage) error {
	_, err := decode(config)
	return err
}
