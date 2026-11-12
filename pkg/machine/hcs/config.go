package hcs

import (
	"encoding/json"

	"github.com/discobox-ai/vm/pkg/machine"
)

// Config is the hcs driver's configuration (machine.Config.DriverConfig),
// which has no fields yet.
type Config struct{}

// Definition describes the hcs driver, which takes no configuration, no
// secret, and no base: a Windows VM is installed from media, or made from a
// committed one, and a fresh VM is refused.
func Definition() machine.Definition {
	return machine.Definition{
		Description: "Windows VMs on Host Compute Service, installed from a retail ISO.",
	}
}

// Validate checks the hcs driver's configuration: an empty object, or none.
func Validate(config json.RawMessage) error {
	_, err := machine.DecodeConfig[Config]("hcs", config)
	return err
}
