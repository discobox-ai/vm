// Command fakevm stands in for a released disco-vm in the installer tests:
// it answers --version as disco-vm does, with the version it was linked with.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("disco-vm version %s\n", version)
		return
	}
	os.Exit(2)
}
