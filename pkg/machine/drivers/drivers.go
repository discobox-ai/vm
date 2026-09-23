// Package drivers links every driver this OS can run into the binary. Import it
// for its side effects; the build-tagged files decide what that means per OS.
package drivers

import (
	// The fake driver is in every build: it is what CI and the neutral tests
	// boot, on every OS.
	_ "github.com/discobox-ai/vm/pkg/machine/fake"
)
