package machine

import "sync"

// MainLoop runs fn, which is the whole program, and returns its exit code. It
// is called on the main thread and may keep that thread for itself while fn
// runs on another goroutine.
type MainLoop func(fn func() int) int

var (
	mainMu   sync.Mutex
	mainLoop MainLoop
)

// SetMainLoop installs the loop Main runs the program under. A driver calls it
// from init; the last one wins, and a binary links one windowing driver.
func SetMainLoop(loop MainLoop) {
	mainMu.Lock()
	defer mainMu.Unlock()
	mainLoop = loop
}

// Main runs the program. Call it from main, with the main goroutine locked to
// the main thread (runtime.LockOSThread in an init of package main).
func Main(fn func() int) int {
	mainMu.Lock()
	loop := mainLoop
	mainMu.Unlock()
	if loop == nil {
		return fn()
	}
	return loop(fn)
}
