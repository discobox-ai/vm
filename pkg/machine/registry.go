package machine

import (
	"fmt"
	"maps"
	"slices"
	"sync"
)

// Factory constructs a driver for a project. Construction must be cheap and
// must not touch the hypervisor or its service: `disco-vm info` constructs
// every driver to list it.
type Factory func(Config) (Driver, error)

var (
	registryMu sync.Mutex
	registry   = map[string]Factory{}
)

// Register makes a driver available by name. Drivers register themselves from
// init in build-tagged files, so a binary contains exactly the drivers its OS
// can run; import pkg/machine/drivers to get all of them.
func Register(name string, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("machine: driver registered twice: " + name)
	}
	registry[name] = factory
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

// New constructs the named driver, or the default one for an empty name, for
// cfg's project, which it checks.
func New(name string, cfg Config) (Driver, error) {
	if name == "" {
		name = defaultDriver
	}
	if cfg.Project == "" {
		cfg.Project = DefaultProject
	}
	if err := ValidateProject(cfg.Project); err != nil {
		return nil, err
	}
	registryMu.Lock()
	factory, ok := registry[name]
	registryMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("machine: no driver %q in this build (have %v)", name, Names())
	}
	return factory(cfg)
}
