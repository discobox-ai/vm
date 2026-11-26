package fake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// Size reads the size the fake driver's instance last booted at, as a
// hypervisor holds a guest to it: a test hook, the conformance suite's
// Config.Size, since a fake guest is a host process with no size of its
// own.
func Size(ctx context.Context, d machine.Driver, inst machine.Instance) (int, uint64, error) {
	ld, ok := d.(*local.Driver)
	if !ok {
		return 0, 0, fmt.Errorf("fake: size: %T is not a fake driver: %w", d, machine.ErrUnsupported)
	}
	if _, ok := ld.Hypervisor().(*Hypervisor); !ok {
		return 0, 0, fmt.Errorf("fake: size: the %s driver is not the fake's: %w", ld.Name(), machine.ErrUnsupported)
	}
	hs, err := ld.Spec(ctx, inst)
	if err != nil {
		return 0, 0, err
	}
	b, err := os.ReadFile(filepath.Join(hs.Dir, sizeName))
	if err != nil {
		return 0, 0, fmt.Errorf("fake: size of %s: %w", inst.Name, err)
	}
	var cpus int
	var memory uint64
	if _, err := fmt.Sscanf(string(b), "cpus=%d memory=%d", &cpus, &memory); err != nil {
		return 0, 0, fmt.Errorf("fake: size of %s: %w", inst.Name, err)
	}
	return cpus, memory, nil
}

func recordSize(dir string, cpus int, memory uint64) error {
	return os.WriteFile(filepath.Join(dir, sizeName), fmt.Appendf(nil, "cpus=%d memory=%d\n", cpus, memory), 0o644) //nolint:gosec // G306: a size, no secrets
}
