package hcs

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// rdpAnswers checks that an RDP server answers on a VM's console pipe: an
// X.224 connection request for standard RDP security gets a TPKT reply.
func rdpAnswers(t *testing.T, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := winio.DialPipeContext(ctx, consolePipe(id))
	if err != nil {
		t.Fatalf("open the console pipe: %v", err)
	}
	defer conn.Close()
	// TPKT(4) + X.224 CR(7) + RDP_NEG_REQ(8), requesting PROTOCOL_RDP (0).
	req := []byte{3, 0, 0, 19, 14, 0xE0, 0, 0, 0, 0, 0, 1, 0, 8, 0, 0, 0, 0, 0}
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	reply := make([]byte, 4)
	if _, err := conn.Read(reply); err != nil {
		t.Fatalf("the console did not answer: %v", err)
	}
	if reply[0] != 3 {
		t.Fatalf("the console answered % x, not a TPKT", reply)
	}
}

// Every VM serves its video console.
func TestConsole(t *testing.T) {
	base := basePayload(t)
	d := &Hypervisor{}
	ctx := context.Background()
	work := t.TempDir()
	chain := []local.Layer{{ID: "base", Dir: base}}
	boot := func(inst local.InstanceSpec) machine.Machine {
		m, err := d.Boot(ctx, inst, local.BootOptions{CPUs: 4, Memory: 4 << 30})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
		return m
	}

	cold := local.InstanceSpec{ID: "cold", Dir: filepath.Join(work, "cold"), GuestOS: machine.Windows, Chain: chain}
	if err := d.Prepare(ctx, cold); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), cold) })
	boot(cold)
	rdpAnswers(t, vmID(cold.Dir))

}
