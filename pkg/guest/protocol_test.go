package guest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// infoOnly is a guest that answers Info alone.
type infoOnly struct {
	machine.Guest
	info machine.GuestInfo
}

func (g infoOnly) Info(context.Context) (machine.GuestInfo, error) { return g.info, nil }

func TestCheckProtocol(t *testing.T) {
	ctx := context.Background()
	if err := guest.CheckProtocol(ctx, infoOnly{info: machine.GuestInfo{Protocol: guest.Protocol}}); err != nil {
		t.Fatalf("an agent of this protocol: %v", err)
	}
	for _, protocol := range []int{0, guest.Protocol + 1} {
		err := guest.CheckProtocol(ctx, infoOnly{info: machine.GuestInfo{Version: "old", Protocol: protocol}})
		if err == nil || !strings.Contains(err.Error(), "speaks protocol") {
			t.Errorf("an agent of protocol %d: %v, want it refused", protocol, err)
		}
	}
}
