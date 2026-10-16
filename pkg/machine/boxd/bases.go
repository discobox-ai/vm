package boxd

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// Bases lists boxd's default image, then every ready snapshot in the caller's
// context that disco-vm did not make, oldest first. A name that starts with
// dvm- is disco-vm's, of this project or another, and is never a base.
func (d *Driver) Bases(ctx context.Context) ([]machine.Base, error) {
	api, err := d.api.API()
	if err != nil {
		return nil, err
	}
	config, err := api.GetConfig(ctx, &boxdapi.GetConfigRequest{})
	if err != nil {
		return nil, fmt.Errorf("boxd: read the default image: %w", err)
	}
	listed, err := api.ListSnapshots(ctx, &boxdapi.ListSnapshotsRequest{})
	if err != nil {
		return nil, fmt.Errorf("boxd: list snapshots: %w", err)
	}
	var out []machine.Base
	// boxd reports no version of an image, so a build on the default image
	// is cached while its name stays the same.
	if image := config.GetDefaultImage(); image != "" {
		out = append(out, machine.Base{
			Name: image, ID: image, GuestOS: machine.Linux,
			Options: map[string]string{"image": image},
		})
	}
	var snapshots []machine.Base
	for _, snap := range listed.GetSnapshots() {
		if strings.HasPrefix(snap.GetName(), namePrefix) || snap.GetStatus() != "ready" {
			continue
		}
		snapshots = append(snapshots, machine.Base{
			Name: snap.GetName(), ID: snap.GetSnapshotId(),
			Version: strconv.FormatUint(snap.GetVersion(), 10), GuestOS: machine.Linux,
			// By ID: a name can be deleted and given to another snapshot.
			Options: map[string]string{"snapshot": snap.GetSnapshotId()},
			Created: unixTime(snap.GetCreatedAt(), time.Millisecond),
		})
	}
	slices.SortFunc(snapshots, func(a, b machine.Base) int { return a.Created.Compare(b.Created) })
	return append(out, snapshots...), nil
}
