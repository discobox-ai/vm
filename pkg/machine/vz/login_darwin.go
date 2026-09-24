//go:build darwin && cgo

package vz

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// loginScript runs as root in the guest over SSH: it creates an account ($1,
// uid $2 or the guest's pick when empty, full name $3, password $4), makes it
// an administrator with passwordless sudo, logs it in at boot (kcpassword $5,
// base64), and hides the install's account ($6) from the login window.
//
// It has to go over SSH: macOS lets only a session with Full Disk Access write
// a user record, and it ignores -UID or refuses outright from the agent.
var loginScript = `set -e
name=$1 uid=$2 full=$3 pw=$4 kc=$5 admin=$6
if id -u "$name" >/dev/null 2>&1; then
	echo "an account named $name already exists in this image" >&2
	exit 1
fi
if [ -n "$uid" ]; then
	owner=$(dscl . -search /Users UniqueID "$uid" | awk 'NR==1 {print $1}')
	if [ -n "$owner" ]; then
		echo "uid $uid is already $owner's in this image; its base was installed before the install account moved off 501, so install it again" >&2
		exit 1
	fi
	sysadminctl -addUser "$name" -fullName "$full" -UID "$uid" -password "$pw" -admin
	if [ "$(id -u "$name")" != "$uid" ]; then
		echo "macOS gave $name uid $(id -u "$name"), not $uid" >&2
		exit 1
	fi
else
	sysadminctl -addUser "$name" -fullName "$full" -password "$pw" -admin
fi
echo "$kc" | base64 -d > /etc/kcpassword
chown root:wheel /etc/kcpassword
chmod 600 /etc/kcpassword
defaults write /Library/Preferences/com.apple.loginwindow autoLoginUser "$name"
echo "$name ALL=(ALL) NOPASSWD: ALL" > "/etc/sudoers.d/disco-vm-$name"
chmod 440 "/etc/sudoers.d/disco-vm-$name"
visudo -cf "/etc/sudoers.d/disco-vm-$name" >/dev/null
dscl . -create "/Users/$admin" IsHidden 1 || true
`

// loginVersion is how a stage's user is set up. It goes up when that changes,
// so that a stage made before is made again: 3 clears Setup Assistant's
// first-login setup after quitting it, and checks with a reboot.
const loginVersion = 3

// loginBase is the bundle a stage with a user clones its templates from: the
// image with the user created, logged in once, and set to log in at boot, shut
// down in order. It is made once per stage and kept, so topping the stage up
// does not make it again.
func loginBase(ctx context.Context, parent bundle, warmDir string, u *machine.User, log io.Writer) (bundle, error) {
	base := bundle(filepath.Join(warmDir, "base"))
	if m, err := base.readMeta(); err == nil && m.Login != nil && m.Login.Name == u.Name && m.Login.UID == u.UID && m.Login.Version == loginVersion {
		return base, nil
	}
	// A new base means new templates: the old ones were cloned from the old.
	_ = os.RemoveAll(string(base))
	_ = os.RemoveAll(filepath.Join(warmDir, templatesName))
	m, err := parent.readMeta()
	if err != nil {
		return "", err
	}
	if m.Password == "" {
		return "", errors.New("vz: this image records no password for its install account, which creating a user needs")
	}
	started := time.Now()
	tmp := bundle(filepath.Join(warmDir, "tmp-"+fsutil.RandomHex(6)))
	if err := makeLogin(ctx, parent, tmp, m, u); err != nil {
		_ = os.RemoveAll(string(tmp))
		return "", err
	}
	if err := os.Rename(string(tmp), string(base)); err != nil {
		_ = os.RemoveAll(string(tmp))
		return "", err
	}
	fmt.Fprintf(log, "vz: created %s and logged it in, in %s\n", u.Name, time.Since(started).Round(time.Second))
	return base, nil
}

func makeLogin(ctx context.Context, parent, dst bundle, m meta, u *machine.User) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if err := cloneLayer(parent, dst); err != nil {
		return err
	}
	if err := dst.newIdentifier(); err != nil {
		return err
	}
	cpus, memory := size(m, 0, 0)
	vm, err := start(dst, cpus, memory, nil, nil)
	if err != nil {
		return err
	}
	defer func() { _ = vm.Kill(context.Background()) }()
	address, err := dst.macAddress()
	if err != nil {
		return err
	}
	ip, err := waitLease(ctx, address.String(), vm.Done())
	if err != nil {
		return err
	}
	client, err := dialSSH(ctx, ip, m.User, m.Password, vm.Done())
	if err != nil {
		return err
	}
	defer client.Close()
	password := fsutil.RandomHex(12)
	uid := ""
	if u.UID != 0 {
		uid = strconv.Itoa(u.UID)
	}
	full := u.FullName
	if full == "" {
		full = u.Name
	}
	kc := base64.StdEncoding.EncodeToString(kcpassword(password))
	if out, err := sudo(client, m.Password, loginScript, u.Name, uid, full, password, kc, m.User); err != nil {
		return fmt.Errorf("vz: create %s in the guest: %w\n%s", u.Name, err, out)
	}

	if err := powerOff(ctx, vm); err != nil {
		return err
	}

	// The user's first login marks it for Setup Assistant's first-login setup
	// (MiniBuddyLaunch in its loginwindow preferences), and every template
	// would resume into Setup Assistant. So log it in, quit Setup Assistant
	// (which sets the mark again while it runs), clear the mark, and shut down;
	// then boot once more to see a plain desktop. Clearing it at creation does
	// not stick: the login sets it.
	for attempt := 1; ; attempt++ {
		vm, err = start(dst, cpus, memory, nil, nil)
		if err != nil {
			return err
		}
		setup, err := loggedInSetup(ctx, vm, u.Name)
		if err == nil && !setup && attempt > 1 {
			return finishLogin(ctx, vm, dst, m, u, password)
		}
		if err == nil {
			err = clearSetup(ctx, vm, u.Name)
		}
		if err == nil {
			err = powerOff(ctx, vm)
		}
		if err != nil {
			_ = vm.Kill(context.Background())
			return err
		}
		if attempt == 3 {
			return fmt.Errorf("vz: %s still logs in to Setup Assistant", u.Name)
		}
	}
}

// loggedInSetup waits for the user's session and reports whether Setup
// Assistant came up in it, which it does within seconds of the desktop.
func loggedInSetup(ctx context.Context, vm *vmMachine, name string) (bool, error) {
	agent := vm.agent()
	defer agent.Close()
	if err := agent.WaitReady(ctx, vm.Done()); err != nil {
		return false, err
	}
	if err := waitLoggedIn(ctx, agent, name); err != nil {
		return false, err
	}
	check := guest.ExecRequest{Argv: []string{"pgrep", "-qx", "Setup Assistant"}}
	for i := 0; i < 5; i++ {
		if code, err := agent.Run(ctx, check, nil, io.Discard, io.Discard); err == nil && code == 0 {
			return true, nil
		}
		time.Sleep(2 * time.Second)
	}
	return false, nil
}

// clearSetup quits Setup Assistant and clears the user's mark for it.
func clearSetup(ctx context.Context, vm *vmMachine, name string) error {
	agent := vm.agent()
	defer agent.Close()
	clear := guest.ExecRequest{Argv: []string{"/bin/sh", "-c", `pkill -x "Setup Assistant" || true
sleep 2
lw="/Users/$1/Library/Preferences/com.apple.loginwindow"
defaults delete "$lw" MiniBuddyLaunch 2>/dev/null || true
defaults delete "$lw" MiniBuddyLaunchCount 2>/dev/null || true
chown "$1:staff" "$lw.plist"`, "sh", name}}
	var out strings.Builder
	if code, err := agent.Run(ctx, clear, nil, &out, &out); err != nil || code != 0 {
		return fmt.Errorf("vz: clear %s's first-login setup: %v (exit %d): %s", name, err, code, out.String())
	}
	return nil
}

// finishLogin shuts the verified base down and records its user.
func finishLogin(ctx context.Context, vm *vmMachine, dst bundle, m meta, u *machine.User, password string) error {
	if err := powerOff(ctx, vm); err != nil {
		_ = vm.Kill(context.Background())
		return err
	}
	// The base carries no running machine's identity; each state gets its own.
	_ = os.Remove(dst.path(macName))
	m.Login = &login{Name: u.Name, UID: u.UID, Password: password, Version: loginVersion}
	return dst.writeMeta(m)
}

// powerOff shuts a guest down through its agent and waits for it to stop.
func powerOff(ctx context.Context, vm *vmMachine) error {
	agent := vm.agent()
	defer agent.Close()
	if err := agent.WaitReady(ctx, vm.Done()); err != nil {
		return err
	}
	if err := agent.Shutdown(ctx, false); err != nil {
		return fmt.Errorf("vz: ask the guest to shut down: %w", err)
	}
	select {
	case <-vm.Done():
		return vm.Err()
	case <-ctx.Done():
		return fmt.Errorf("vz: the guest did not power off: %w", ctx.Err())
	}
}

// waitLoggedIn waits until the user owns the console and has a desktop
// (Finder), so a state saved after it resumes into that user's session.
func waitLoggedIn(ctx context.Context, agent *guest.Client, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	check := guest.ExecRequest{Argv: []string{"/bin/sh", "-c",
		`[ "$(stat -f %Su /dev/console)" = "$1" ] && pgrep -qxu "$1" Finder`, "sh", name}}
	for {
		if code, err := agent.Run(ctx, check, nil, io.Discard, io.Discard); err == nil && code == 0 {
			// The desktop is up; give it a moment to finish drawing.
			select {
			case <-time.After(5 * time.Second):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("vz: %s was not logged in: %w", name, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// kcpassword is /etc/kcpassword's content for a password: loginwindow reads
// the password for automatic login from it. It is the password XORed with a
// fixed key and padded with zeros to a multiple of 12 bytes, with at least one.
func kcpassword(password string) []byte {
	key := []byte{0x7d, 0x89, 0x52, 0x23, 0xd2, 0xbc, 0xdd, 0xea, 0xa3, 0xb9, 0x1f}
	out := make([]byte, (len(password)/12+1)*12)
	copy(out, password)
	for i := range out {
		out[i] ^= key[i%len(key)]
	}
	return out
}
