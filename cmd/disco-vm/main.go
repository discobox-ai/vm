// Command disco-vm builds OS images and runs them as VMs: Windows guests on
// Windows (Host Compute Service), macOS guests on macOS
// (Virtualization.framework), from one codebase and one command line.
//
// The same binary is also the guest agent (`disco-vm guest`) and each VM's
// supervisor (`disco-vm shim`).
package main

import (
	"os"

	"github.com/discobox-ai/vm/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
