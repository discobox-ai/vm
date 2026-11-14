package hcs

import (
	"encoding/json"
	"fmt"

	"github.com/discobox-ai/vm/pkg/machine"
)

// ViewEnv is where the command line reads Config.View from.
const ViewEnv = "DISCO_VM_HCS_VIEW"

// ViewDisplay is Config.View's one value: the agent's display stream.
const ViewDisplay = "display"

// Config is the hcs driver's configuration (machine.Config.DriverConfig).
type Config struct {
	// View set to ViewDisplay makes a window on a running instance the
	// agent's display stream even where the guest serves Remote Desktop:
	// what a Home guest gets, tried on any guest. Empty is the guest's
	// Remote Desktop where it serves one.
	View string `json:"view,omitempty"`
}

// Definition describes the hcs driver and its configuration. It takes no
// secret and no base: a Windows VM is installed from media, or made from a
// committed one, and a fresh VM is refused.
func Definition() machine.Definition {
	return machine.Definition{
		Description: "Windows VMs on Host Compute Service, installed from a retail ISO.",
		ConfigFields: []machine.ConfigField{
			{Key: "view", Label: "View", Type: machine.ConfigString, Env: ViewEnv, Advanced: true, Placeholder: ViewDisplay,
				Description: "\"display\" shows a running instance's window as the agent's display stream even where the guest serves Remote Desktop; empty is its Remote Desktop where it serves one."},
		},
	}
}

func decode(config json.RawMessage) (Config, error) {
	cfg, err := machine.DecodeConfig[Config]("hcs", config)
	if err != nil {
		return cfg, err
	}
	if cfg.View != "" && cfg.View != ViewDisplay {
		return cfg, fmt.Errorf("hcs: configuration: view: not empty or %q", ViewDisplay)
	}
	return cfg, nil
}

// Validate checks the hcs driver's configuration.
func Validate(config json.RawMessage) error {
	_, err := decode(config)
	return err
}
