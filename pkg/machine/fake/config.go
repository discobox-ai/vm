package fake

import (
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/discobox-ai/vm/pkg/machine"
)

// AgentEnv is where the command line reads Config.Agent from.
const AgentEnv = "DISCO_VM_AGENT"

// NoCommitEnv is where the command line reads Config.NoCommit from. It
// exists to test the refusals of a driver without Capabilities.Commit from
// the command line.
const NoCommitEnv = "DISCO_VM_FAKE_NO_COMMIT"

// MaxRunningEnv is where the command line reads Config.MaxRunning from.
const MaxRunningEnv = "DISCO_VM_FAKE_MAX_RUNNING"

// Config is the fake driver's configuration (machine.Config.DriverConfig,
// ADR 26-11-11-215).
type Config struct {
	// Agent is the disco-vm binary each guest runs as `disco-vm guest`, as
	// an absolute path. Empty is the running disco-vm.
	Agent string `json:"agent,omitempty"`
	// NoCommit makes a driver without Capabilities.Commit, as
	// Hypervisor.NoCommit does.
	NoCommit bool `json:"noCommit,omitempty"`
	// MaxRunning caps running guests of the host's OS
	// (Capabilities.MaxRunning), as Hypervisor.MaxRunning does. Absent is
	// no cap.
	MaxRunning *int `json:"maxRunning,omitempty"`
}

// Definition describes the fake driver and its configuration.
func Definition() machine.Definition {
	return machine.Definition{
		Description: "Guests as disco-vm agent processes on this host, each confined to a directory: for tests and CI, on every OS.",
		Base: &machine.ConfigField{Label: "Base", Type: machine.ConfigString, Env: machine.BaseEnv, Default: DefaultBase,
			Description: "A name a fresh VM's empty root is marked with."},
		ConfigFields: []machine.ConfigField{
			{Key: "agent", Label: "Agent", Type: machine.ConfigString, Env: AgentEnv, Advanced: true, Privileged: true,
				Description: "The absolute path of the disco-vm each guest runs as its agent; empty is this binary."},
			{Key: "noCommit", Label: "No Commit", Type: machine.ConfigBoolean, Env: NoCommitEnv, Default: "false", Advanced: true,
				Description: "Make a driver that does not commit, so every VM is fresh: a test of the refusals of such a driver."},
			{Key: "maxRunning", Label: "Max Running", Type: machine.ConfigNumber, Env: MaxRunningEnv, Advanced: true,
				Description: "Cap how many guests run at once in the root, as vz caps macOS guests; absent is no cap."},
		},
		// The key is checked in the process, against the root's records,
		// and sent nowhere.
		Secrets: []machine.SecretField{{
			Key: "key", Label: "Key", Type: machine.ConfigSecret, Env: []string{KeyEnv},
			Description: "A key the fake driver issued; absent, the caller is the root directory's owner.",
		}},
	}
}

func decode(config json.RawMessage) (Config, error) {
	cfg, err := machine.DecodeConfig[Config]("fake", config)
	if err != nil {
		return cfg, err
	}
	if cfg.Agent != "" && !filepath.IsAbs(cfg.Agent) {
		return cfg, errors.New("fake: configuration: agent: not an absolute path")
	}
	if cfg.MaxRunning != nil && *cfg.MaxRunning < 0 {
		return cfg, errors.New("fake: configuration: maxRunning: not a count")
	}
	return cfg, nil
}

// Validate checks the fake driver's configuration.
func Validate(config json.RawMessage) error {
	_, err := decode(config)
	return err
}
