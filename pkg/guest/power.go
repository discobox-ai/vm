package guest

import (
	"fmt"
	"os/exec"
	"runtime"
)

// powerOff shuts the guest down in order, the way its OS expects to be asked.
// On macOS this is the only orderly path: Virtualization.framework's own stop
// request arrives as a power-button press, which macOS answers by sleeping.
func powerOff(reboot bool) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "windows":
		name, args = "shutdown.exe", []string{"/s", "/t", "0", "/f"}
		if reboot {
			args[0] = "/r"
		}
	case "darwin":
		name, args = "/sbin/shutdown", []string{"-h", "now"}
		if reboot {
			args[0] = "-r"
		}
	default:
		name, args = "/usr/bin/systemctl", []string{"poweroff"}
		if reboot {
			args[0] = "reboot"
		}
	}
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // Fixed lifecycle command.
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, out)
	}
	return nil
}
