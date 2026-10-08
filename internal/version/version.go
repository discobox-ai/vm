// Package version is disco-vm's version: set by the linker on a release
// build, and otherwise the VCS revision the binary was built from.
package version

import "runtime/debug"

// Version is set at link time with
// -ldflags '-X github.com/discobox-ai/vm/internal/version.Version=vX.Y.Z'.
var Version = ""

// String returns Version, or the VCS revision when it was not set.
func String() string {
	if Version != "" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return rev + dirty
}
