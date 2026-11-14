//go:build carryagent

package main

import (
	"embed"
	"io/fs"
)

// carriedFiles is the linux agent of this build (ADR 26-11-14-352 §1),
// which the release and `go tool task build` put in carried/
// (agent.Carry, by `disco-vm-release carry`) before they build a disco-vm
// for a host that is not Linux with the carryagent tag. The directory is not
// in git.
//
//go:embed carried
var carriedFiles embed.FS

// carried is the agents this disco-vm carries, by platform.
func carried() fs.FS {
	sub, err := fs.Sub(carriedFiles, "carried")
	if err != nil {
		panic(err)
	}
	return sub
}
