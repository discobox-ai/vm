package guest

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
)

// serviceName is the ServiceFile's name in its directory.
const serviceName = "service"

// serviceDir is the directory of a guest OS's ServiceFile.
func serviceDir(goos string) string {
	if goos == "windows" {
		return `C:\ProgramData\disco-vm`
	}
	return "/var/lib/disco-vm"
}

// ServiceFile is where a guest of an OS finds what its instance's one service
// listens on: one line, an address in Listen's form (vsock:7302,
// tcp:0.0.0.0:7302). The engine writes it at every start, before the start
// returns, and the driver picks the address. It is on disk, so it survives a
// reboot inside the guest.
func ServiceFile(goos string) string {
	if goos == "windows" {
		return serviceDir(goos) + `\` + serviceName
	}
	return serviceDir(goos) + "/" + serviceName
}

// SetService writes the ServiceFile of a guest of an OS.
func (c *Client) SetService(ctx context.Context, goos, address string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	data := []byte(address + "\n")
	if err := tw.WriteHeader(&tar.Header{Name: serviceName, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := c.CopyTo(ctx, serviceDir(goos), &buf); err != nil {
		return fmt.Errorf("guest: write %s: %w", ServiceFile(goos), err)
	}
	return nil
}

// ServiceAddress reads this guest's ServiceFile.
func ServiceAddress() (string, error) {
	data, err := os.ReadFile(ServiceFile(runtime.GOOS))
	if err != nil {
		return "", fmt.Errorf("guest: the instance's service address: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// ListenService opens the listener for this guest's one service, the address
// in its ServiceFile. What it accepts is what the instance's URL reaches.
func ListenService() (net.Listener, error) {
	address, err := ServiceAddress()
	if err != nil {
		return nil, err
	}
	return Listen(address)
}
