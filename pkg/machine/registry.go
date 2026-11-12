package machine

import (
	"fmt"
	"maps"
	"slices"
	"sync"
)

// Factory constructs a driver for a pool. Construction must be cheap and
// must not touch the hypervisor or its service: `disco-vm info` constructs
// every driver to list it. It is given a configuration Normalize returned.
type Factory func(Config) (Driver, error)

// Registration is a driver: how to construct one, and the configuration and
// secrets it takes.
type Registration struct {
	// New constructs the driver.
	New Factory
	// Definition describes the driver, every field of its configuration
	// (Config.DriverConfig), and its secrets, where the command line finds
	// them (KeyFromEnv) and where the driver sends them.
	Definition Definition
	// Validate checks a configuration the definition's fields allow.
	Validate Validator
}

var (
	registryMu sync.Mutex
	registry   = map[string]Registration{}
)

// Register makes a driver available by name. Drivers register themselves from
// init in build-tagged files, so a binary contains exactly the drivers its OS
// can run; import pkg/machine/drivers to get all of them. It panics on a
// registration missing its factory or validator, or whose definition is
// malformed: those are the driver's bugs.
func Register(name string, r Registration) {
	if r.New == nil || r.Validate == nil {
		panic("machine: driver " + name + " registered without a factory or a validator")
	}
	if err := r.Definition.check(); err != nil {
		panic("machine: driver " + name + ": " + err.Error())
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("machine: driver registered twice: " + name)
	}
	registry[name] = r
}

// Names lists the registered drivers.
func Names() []string {
	registryMu.Lock()
	defer registryMu.Unlock()
	return slices.Sorted(maps.Keys(registry))
}

// DefaultName is this OS's hypervisor: hcs on Windows, vz on macOS, and the
// fake driver anywhere else.
func DefaultName() string { return defaultDriver }

// DefinitionOf is the named driver's definition: its description and its
// configuration's fields.
func DefinitionOf(name string) (Definition, error) {
	r, err := lookup(name)
	return r.Definition, err
}

func lookup(name string) (Registration, error) {
	registryMu.Lock()
	r, ok := registry[name]
	registryMu.Unlock()
	if !ok {
		return Registration{}, fmt.Errorf("machine: no driver %q in this build (have %v)", name, Names())
	}
	return r, nil
}

// New constructs the named driver, or the default one for an empty name, for
// cfg's pool, which it checks, with cfg.DriverConfig, which it validates
// and normalizes (Normalize): the driver is given its current fields alone.
func New(name string, cfg Config) (Driver, error) {
	if name == "" {
		name = defaultDriver
	}
	if cfg.Pool == "" {
		cfg.Pool = DefaultPool
	}
	if err := ValidatePool(cfg.Pool); err != nil {
		return nil, err
	}
	r, err := lookup(name)
	if err != nil {
		return nil, err
	}
	if cfg.DriverConfig, err = Normalize(name, cfg.DriverConfig); err != nil {
		return nil, err
	}
	return r.New(cfg)
}
