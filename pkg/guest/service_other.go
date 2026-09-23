//go:build !windows

package guest

// RunAgent runs serve. A launchd daemon or a systemd unit is an ordinary
// process, so there is nothing to report to; stop is for the Windows service
// control manager.
func RunAgent(serve func() error, stop func()) error { return serve() }
