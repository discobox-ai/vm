package docker

import "testing"

func TestShiftID(t *testing.T) {
	for in, want := range map[uint32]int{
		0:               UIDBase,
		1000:            UIDBase + 1000,
		65535:           UIDBase + 65535,
		UIDBase + 5:     UIDBase + 5, // already shifted
		70000:           UIDBase + 65534,
		UIDBase + 70000: UIDBase + 65534,
	} {
		if got := shiftID(in); got != want {
			t.Errorf("shiftID(%d) = %d, want %d", in, got, want)
		}
	}
}
