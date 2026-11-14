// Command disco-vm builds OS images and runs them as VMs: Windows guests on
// Windows (Host Compute Service), macOS guests on macOS
// (Virtualization.framework), from one codebase and one command line.
//
// The same binary is also the guest agent (`disco-vm guest`) and each VM's
// supervisor (`disco-vm shim`). Built with the carryagent tag for a host
// that is not Linux, it carries the linux disco-vm of its own build, which
// it hands a Linux guest as its agent.
package main

import (
	"os"
	"runtime"

	"github.com/discobox-ai/vm/internal/cli"
	"github.com/discobox-ai/vm/pkg/machine"
)

// The main goroutine keeps the main thread, which a driver's native window
// needs (machine.Main).
func init() { runtime.LockOSThread() }

func main() {
	os.Exit(machine.Main(func() int { return cli.Main(os.Args[1:], carried()) }))
}
