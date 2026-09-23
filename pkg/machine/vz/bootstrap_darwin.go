//go:build darwin && cgo

package vz

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Code-Hex/vz/v3"
	"golang.org/x/crypto/ssh"

	"github.com/discobox-ai/vm/pkg/machine"
)

// The agent's place in the guest, and the launchd daemon that starts it at
// boot, before anyone logs in, as root.
const (
	agentPath = "/usr/local/libexec/disco-vm"
	daemonID  = "ai.discobox.vm.guest"
)

var daemonPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + daemonID + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + agentPath + `</string>
		<string>guest</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/var/log/disco-vm-guest.log</string>
	<key>StandardErrorPath</key>
	<string>/var/log/disco-vm-guest.log</string>
</dict>
</plist>
`

// installScript runs as root in the guest. System sleep is turned off because
// a sleeping guest's agent answers nothing.
var installScript = `set -e
install -d -o root -g wheel -m 755 /usr/local/libexec
install -o root -g wheel -m 755 "$1" ` + agentPath + `
rm -f "$1"
cat > /Library/LaunchDaemons/` + daemonID + `.plist <<'PLIST'
` + daemonPlist + `PLIST
chown root:wheel /Library/LaunchDaemons/` + daemonID + `.plist
chmod 644 /Library/LaunchDaemons/` + daemonID + `.plist
pmset -a sleep 0
launchctl bootstrap system /Library/LaunchDaemons/` + daemonID + `.plist
`

// bootstrap is the install's first boot. An IPSW install leaves no way to
// write to the guest's Data volume, so macOS 27 guest provisioning creates
// the account with SSH on, and the agent goes in over SSH across the NAT.
// From then on the host speaks only to the agent, over vsock.
//
// A new guest restarts itself during its first boot, and to the framework a
// restart is a stop, so a guest that stops cleanly before the agent is in is
// booted again. Provisioning is asked for every time; a guest that has
// already applied it ignores it.
func bootstrap(ctx context.Context, b bundle, m meta, agent string, opts options, gui bool, log io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	binary, err := os.ReadFile(agent)
	if err != nil {
		return fmt.Errorf("vz: read the agent: %w", err)
	}
	address, err := b.macAddress()
	if err != nil {
		return err
	}
	for boot := 1; ; boot++ {
		if boot == 1 {
			fmt.Fprintf(log, "vz: first boot, provisioning account %s\n", opts.user)
		} else {
			fmt.Fprintln(log, "vz: the guest restarted itself during setup; booting it again")
		}
		vm, err := start(b, m.CPUs, m.Memory, nil, &vz.MacGuestProvisioningOptions{
			FullName:            opts.fullName,
			Username:            opts.user,
			Password:            opts.password,
			LogsInAutomatically: opts.autoLogin,
			EnablesRemoteLogin:  true,
		})
		if err != nil {
			return err
		}
		if err := vm.display(machine.BootOptions{GUI: gui, Title: "disco-vm install: first boot"}); err != nil {
			return err
		}
		err = installAgent(ctx, vm, address.String(), binary, opts, log)
		if errors.Is(err, errRestarted) && vm.Err() == nil && boot < 4 {
			continue
		}
		if err != nil {
			_ = vm.Kill(context.Background())
		}
		return err
	}
}

// errRestarted is a guest that stopped during setup without an error.
var errRestarted = errors.New("vz: the guest stopped during its first boot")

// installAgent puts the agent into a booting guest, then shuts the guest down
// through it.
func installAgent(ctx context.Context, vm *vmMachine, mac string, binary []byte, opts options, log io.Writer) error {
	ip, err := waitLease(ctx, mac, vm.Done())
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "vz: guest is %s; installing the agent over SSH\n", ip)
	client, err := dialSSH(ctx, ip, opts.user, opts.password, vm.Done())
	if err != nil {
		return err
	}
	defer client.Close()
	const upload = "/tmp/disco-vm.upload"
	if _, err := runSSH(client, "cat > "+upload, bytes.NewReader(binary)); err != nil {
		return fmt.Errorf("vz: copy the agent into the guest: %w", err)
	}
	// sudo reads the password from stdin; the script is an argument.
	cmd := "sudo -S -p '' /bin/sh -c " + shellQuote(installScript) + " install " + upload
	if out, err := runSSH(client, cmd, strings.NewReader(opts.password+"\n")); err != nil {
		return fmt.Errorf("vz: install the agent in the guest: %w\n%s", err, out)
	}

	guest := vm.agent()
	defer guest.Close()
	if err := guest.WaitReady(ctx, vm.Done()); err != nil {
		return fmt.Errorf("vz: the agent was installed but does not answer on vsock: %w", err)
	}
	info, err := guest.Info(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "vz: agent %s answers from %s; shutting down\n", info.Version, info.Hostname)
	if err := guest.Shutdown(ctx, false); err != nil {
		return fmt.Errorf("vz: ask the guest to shut down: %w", err)
	}
	select {
	case <-vm.Done():
		return vm.Err()
	case <-time.After(5 * time.Minute):
		return errors.New("vz: the guest did not power off after the install")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitLease finds the guest's address in the NAT's DHCP leases, by its MAC.
func waitLease(ctx context.Context, mac string, stopped <-chan struct{}) (string, error) {
	want, err := net.ParseMAC(mac)
	if err != nil {
		return "", err
	}
	for {
		if ip := leaseFor(dhcpLeases, want); ip != "" {
			return ip, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("vz: the guest took no DHCP lease for %s: %w", mac, ctx.Err())
		case <-stopped:
			return "", errRestarted
		case <-time.After(2 * time.Second):
		}
	}
}

// dhcpLeases is where macOS's NAT (bootpd) records its leases.
const dhcpLeases = "/var/db/dhcpd_leases"

// leaseFor reads the most recent lease for a MAC. bootpd writes MAC octets
// without leading zeros ("b2:7:43:..."), so they are compared as numbers.
func leaseFor(file string, mac net.HardwareAddr) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	var ip, best string
	var hw net.HardwareAddr
	var lease, bestLease uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "{":
			ip, hw, lease = "", nil, 0
		case line == "}":
			if bytes.Equal(hw, mac) && ip != "" && lease >= bestLease {
				best, bestLease = ip, lease
			}
		case strings.HasPrefix(line, "ip_address="):
			ip = strings.TrimPrefix(line, "ip_address=")
		case strings.HasPrefix(line, "hw_address="):
			_, addr, _ := strings.Cut(strings.TrimPrefix(line, "hw_address="), ",")
			hw = parseLooseMAC(addr)
		case strings.HasPrefix(line, "lease="):
			lease, _ = strconv.ParseUint(strings.TrimPrefix(line, "lease="), 0, 64)
		}
	}
	return best
}

func parseLooseMAC(s string) net.HardwareAddr {
	parts := strings.Split(s, ":")
	out := make(net.HardwareAddr, 0, len(parts))
	for _, part := range parts {
		b, err := strconv.ParseUint(part, 16, 8)
		if err != nil {
			return nil
		}
		out = append(out, byte(b))
	}
	return out
}

// dialSSH retries until provisioning has created the account and sshd lets it
// in. Host keys are not checked: this is a guest the install just created, on
// the host's own NAT, reached before anything else could be.
func dialSSH(ctx context.Context, ip, user, password string, stopped <-chan struct{}) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = password
				}
				return answers, nil
			}),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // See above.
		Timeout:         10 * time.Second,
	}
	var last error
	for {
		client, err := ssh.Dial("tcp", net.JoinHostPort(ip, "22"), config)
		if err == nil {
			return client, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("vz: SSH to the guest as %s: %w (last: %v)", user, ctx.Err(), last)
		case <-stopped:
			return nil, errRestarted
		case <-time.After(3 * time.Second):
		}
	}
}

func runSSH(client *ssh.Client, cmd string, stdin io.Reader) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	session.Stdin = stdin
	out, err := session.CombinedOutput(cmd)
	return string(out), err
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
