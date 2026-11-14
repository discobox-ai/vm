package vz

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/discobox-ai/vm/pkg/machine/agent"
)

// A macOS guest's agent comes on a read-only drive the host attaches at
// every boot (ADR 26-11-13-318), and its base holds only a launch daemon
// whose program is /bin/sh running stub (stubScript): it copies the agent
// off the drive into a root-only directory and runs it.
const (
	// daemonID is the launch daemon's label, which starts the stub at boot,
	// before anyone logs in, as root, and again whenever it exits.
	daemonID = "ai.discobox.vm.guest"
	// driveLabel is the drive's volume label, which macOS mounts it under
	// at /Volumes/<label>.
	driveLabel = "DISCO_VM_AGENT"
	// driveAgent is the agent's name on the drive, and driveDigest the
	// file holding its SHA-256, in hex.
	driveAgent  = "disco-vm"
	driveDigest = "disco-vm.sha256"
	// agentRunDir is where the stub keeps the copy it runs: under /var/run,
	// which macOS empties at boot, readable by root alone.
	agentRunDir = "/var/run/disco-vm"
)

// stubWait is how many fifths of a second the stub waits for the drive to
// be mounted before it gives up and launchd starts it again.
const stubWait = 300

// stubScript is the launch daemon's program, for the drive mounted at
// mountPoint, its whole-disk devices matching devices (a shell glob), and
// the copy kept in runDir. It waits for Disk Arbitration to mount the drive,
// asks it to mount it read-only (diskutil) where it has not, copies the
// agent off it into a file of its own, checks it against the drive's
// digest, unmounts the drive, and runs the copy as the agent. It holds no
// state between boots, and a failure exits for launchd to start it again.
func stubScript(mountPoint, devices, runDir string) string {
	return `set -u
label=` + driveLabel + `
mnt=` + mountPoint + `
dir=` + runDir + `
# mounted: the drive is mounted where it is read, read-only, as ISO 9660.
mounted() { mount | grep -F " on $mnt (cd9660, " | grep -qF "read-only"; }
# device: the whole disk whose volume is the drive's.
device() {
	for d in ` + devices + `; do
		case "${d##*/}" in *s[0-9]*) continue ;; esac
		diskutil info "$d" 2>/dev/null | grep -Eq "^ *Volume Name: +$label\$" && { echo "$d"; return 0; }
	done
	return 1
}
digest() {
	if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1"; else openssl dgst -sha256 -r "$1"; fi | cut -d ' ' -f 1
}
i=0
until mounted; do
	i=$((i + 1))
	if [ "$i" -gt ` + strconv.Itoa(stubWait) + ` ]; then
		echo "disco-vm: no drive $label is mounted at $mnt; the host attaches it at every boot" >&2
		exit 1
	fi
	if [ $((i % 5)) -eq 0 ] && d=$(device); then
		diskutil mount readOnly "$d" >/dev/null 2>&1 || true
	fi
	sleep 0.2
done
umask 077
mkdir -p "$dir" && chmod 700 "$dir" || exit 1
rm -f "$dir/disco-vm.new"
if ! cp "$mnt/` + driveAgent + `" "$dir/disco-vm.new"; then
	echo "disco-vm: could not copy the agent off $mnt" >&2
	exit 1
fi
want=$(cat "$mnt/` + driveDigest + `")
got=$(digest "$dir/disco-vm.new")
if [ -z "$want" ] || [ "$got" != "$want" ]; then
	echo "disco-vm: the agent copied off $mnt has SHA-256 $got, and the drive says $want" >&2
	rm -f "$dir/disco-vm.new"
	exit 1
fi
chmod 755 "$dir/disco-vm.new" && mv -f "$dir/disco-vm.new" "$dir/disco-vm" || exit 1
diskutil unmount "$mnt" >/dev/null 2>&1 || umount "$mnt" 2>/dev/null || true
exec "$dir/disco-vm" guest
`
}

// daemonPlist is the launch daemon a macOS base carries, its stub inline,
// so the base holds no executable of disco-vm's and launchd's program is
// Apple's own /bin/sh.
func daemonPlist() string {
	stub := plistEscape(stubScript("/Volumes/"+driveLabel, "/dev/disk[0-9]*", agentRunDir))
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + daemonID + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>-c</string>
		<string>` + stub + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/var/log/` + daemonID + `.log</string>
	<key>StandardErrorPath</key>
	<string>/var/log/` + daemonID + `.log</string>
</dict>
</plist>
`
}

// plistEscape escapes text for a plist's <string>: &, < and > alone. Newlines
// and tabs stay literal, since launchd's plist reader ends a string at a
// character reference such as encoding/xml's &#xA;, so a stub escaped that
// way runs as its first line alone.
func plistEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// MacOSAgentEnv is where the command line reads Config.MacOSAgent from,
// when it does not set it to its own executable.
const MacOSAgentEnv = "DISCO_VM_MACOS_AGENT"

// checkMacOSAgent refuses an agent that is not a Mach-O executable for this
// Mac's architecture, before a macOS guest is booted or installed with a
// drive it could never run.
func checkMacOSAgent(macosAgent, arch string) error {
	if macosAgent == "" {
		return fmt.Errorf("vz: a macOS guest needs the disco-vm built for darwin/%s as its agent: configure macosAgent ($%s) as it", arch, MacOSAgentEnv)
	}
	if err := agent.Check(macosAgent, "darwin", arch); err != nil {
		return fmt.Errorf("vz: the macOS agent (macosAgent, $%s): %w", MacOSAgentEnv, err)
	}
	return nil
}

// agentDrive is the agent's drive in the cache, <cacheDir>/agent/<the
// agent's SHA-256>.iso, built the first time an agent is booted and used as
// it is after that (ADR 26-11-13-318 §2). Beside it, <name>.sha256 holds the
// image's own SHA-256, and an image that does not match it is built again.
// The image is the same bytes for the same agent, so two boots that build
// it at once write the same file.
func agentDrive(cacheDir, agent string) (string, error) {
	data, err := os.ReadFile(agent)
	if err != nil {
		return "", fmt.Errorf("vz: the macOS agent: %w", err)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	dir := filepath.Join(cacheDir, "agent")
	iso := filepath.Join(dir, digest+".iso")
	if err := checkDrive(iso); err == nil {
		return iso, nil
	} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errDriveDigest) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("vz: the agent's drive: %w", err)
	}
	var image bytes.Buffer
	if err := writeISO(&image, driveLabel, []isoFile{
		{Name: driveAgent, Mode: 0o755, Data: data},
		{Name: driveDigest, Mode: 0o644, Data: []byte(digest + "\n")},
	}); err != nil {
		return "", fmt.Errorf("vz: the agent's drive: %w", err)
	}
	imageSum := sha256.Sum256(image.Bytes())
	if err := writeFileAtomic(iso, image.Bytes()); err != nil {
		return "", err
	}
	if err := writeFileAtomic(iso+".sha256", []byte(hex.EncodeToString(imageSum[:])+"\n")); err != nil {
		return "", err
	}
	return iso, nil
}

// errDriveDigest is a cached drive whose content is not the digest recorded
// beside it.
var errDriveDigest = errors.New("vz: the agent's drive does not match its recorded digest")

// checkDrive checks a cached drive against the digest recorded beside it.
func checkDrive(iso string) error {
	want, err := os.ReadFile(iso + ".sha256")
	if err != nil {
		return err
	}
	f, err := os.Open(iso)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("vz: the agent's drive: %w", err)
	}
	if hex.EncodeToString(h.Sum(nil)) != strings.TrimSpace(string(want)) {
		return errDriveDigest
	}
	return nil
}

// writeFileAtomic writes data to a new file beside path, readable by its
// owner alone, and renames it into place.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("vz: the agent's drive: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("vz: the agent's drive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("vz: the agent's drive: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("vz: the agent's drive: %w", err)
	}
	return nil
}
