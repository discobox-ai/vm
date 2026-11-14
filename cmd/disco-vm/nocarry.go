//go:build !carryagent

package main

import "io/fs"

// carried is none: a build without the carryagent tag (a plain go build, go
// install, or go test) carries no agent, and needs one configured for a
// guest of another OS than its own (ADR 26-11-14-352 §1).
func carried() fs.FS { return nil }
