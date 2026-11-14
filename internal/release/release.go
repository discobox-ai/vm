// Package release is what a disco-vm release publishes beside its binaries
// (ADR 26-11-13-679): their checksums, the installers stamped with the
// release and each binary's digest, and the Homebrew formula, and the GitHub
// calls that publish them. internal/cmd/disco-vm-release is its one caller.
//
// The installers are embedded here, so the release and the tests that run
// them both handle exactly the bytes a user downloads.
package release

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Binaries are the files a release builds, one for each OS and architecture
// disco-vm ships on: static off macOS, and on macOS signed with the
// Virtualization entitlement. darwin/amd64 is not one, because vz's macOS
// guests need Apple silicon.
var Binaries = []string{
	"disco-vm-darwin-arm64",
	"disco-vm-linux-amd64",
	"disco-vm-linux-arm64",
	"disco-vm-windows-amd64.exe",
}

// The other files a release uploads beside Binaries.
const (
	ChecksumsName  = "checksums.txt"
	ShellName      = "install.sh"
	PowerShellName = "install.ps1"
)

// Assets are the names of every file a release uploads, in upload order.
func Assets() []string {
	return append(slices.Clone(Binaries), ChecksumsName, ShellName, PowerShellName)
}

//go:embed install.sh
var shellScript []byte

//go:embed install.ps1
var powerShellScript []byte

// A tag is a SemVer version with an optional -alpha.N or -rc.N (repostd §9).
var tagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|rc)\.(0|[1-9][0-9]*))?$`)

// CheckTag refuses a tag that is not a release version.
func CheckTag(tag string) error {
	if !tagPattern.MatchString(tag) {
		return fmt.Errorf("%q is not a release tag: want vX.Y.Z, vX.Y.Z-alpha.N, or vX.Y.Z-rc.N", tag)
	}
	return nil
}

// Prerelease reports whether a release tag is a prerelease, which GitHub
// marks as one and which never reaches the Homebrew tap.
func Prerelease(tag string) bool {
	return strings.Contains(tag, "-")
}

// A Digest is a file a release uploaded and its SHA-256.
type Digest struct {
	Name   string
	SHA256 string
}

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Hash returns the digest of each of Binaries in dir, and fails if any is
// missing: a release without one of its platforms is not published.
func Hash(dir string) ([]Digest, error) {
	digests := make([]Digest, 0, len(Binaries))
	for _, name := range Binaries {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		digests = append(digests, Digest{Name: name, SHA256: hex.EncodeToString(h.Sum(nil))})
	}
	return digests, nil
}

// Checksums formats digests as sha256sum writes them, so
// `sha256sum -c checksums.txt` checks a download.
func Checksums(digests []Digest) []byte {
	var b bytes.Buffer
	for _, d := range digests {
		fmt.Fprintf(&b, "%s  %s\n", d.SHA256, d.Name)
	}
	return b.Bytes()
}

// ParseChecksums reads what Checksums wrote, requiring a digest of each of
// Binaries and nothing else.
func ParseChecksums(data []byte) ([]Digest, error) {
	byName := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || !digestPattern.MatchString(sum) || !slices.Contains(Binaries, name) {
			return nil, fmt.Errorf("checksums: %q is not a line for one of the release's binaries", line)
		}
		if _, dup := byName[name]; dup {
			return nil, fmt.Errorf("checksums: %s is listed twice", name)
		}
		byName[name] = sum
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	digests := make([]Digest, 0, len(Binaries))
	for _, name := range Binaries {
		sum, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("checksums: %s is missing", name)
		}
		digests = append(digests, Digest{Name: name, SHA256: sum})
	}
	return digests, nil
}

// Stamp returns both installers, keyed by file name, stamped to install
// release from digests.
//
// Every value is checked before it is written into a script: a tag or digest
// that could close a quote in either language is an error here rather than a
// command on somebody's machine.
func Stamp(tag string, digests []Digest) (map[string][]byte, error) {
	if err := CheckTag(tag); err != nil {
		return nil, err
	}
	if len(digests) == 0 {
		return nil, errors.New("a release with no binaries has nothing to install")
	}
	for _, d := range digests {
		if !slices.Contains(Binaries, d.Name) {
			return nil, fmt.Errorf("%q is not one of the release's binaries", d.Name)
		}
		if !digestPattern.MatchString(d.SHA256) {
			return nil, fmt.Errorf("%s: %q is not a lowercase hex SHA-256", d.Name, d.SHA256)
		}
	}

	var sums strings.Builder
	sums.WriteString("checksums='\n")
	sums.Write(Checksums(digests))
	sums.WriteString("'")
	shell, err := stampLines(ShellName, shellScript, map[string]string{
		"release=":   "release='" + tag + "'",
		"checksums=": sums.String(),
	})
	if err != nil {
		return nil, err
	}

	var table strings.Builder
	table.WriteString("$checksums = @{\n")
	for _, d := range digests {
		fmt.Fprintf(&table, "    '%s' = '%s'\n", d.Name, d.SHA256)
	}
	table.WriteString("}")
	powerShell, err := stampLines(PowerShellName, powerShellScript, map[string]string{
		"$release = ''":    "$release = '" + tag + "'",
		"$checksums = @{}": table.String(),
	})
	if err != nil {
		return nil, err
	}
	return map[string][]byte{ShellName: shell, PowerShellName: powerShell}, nil
}

// stampLines replaces each line whose content, less its indentation, is a key
// of stamps, keeping that indentation on every line of the replacement. Each
// key has to match exactly one line: a script edited so that a marker moved
// or doubled would otherwise ship unstamped, or stamped twice.
func stampLines(name string, script []byte, stamps map[string]string) ([]byte, error) {
	seen := make(map[string]int, len(stamps))
	var out bytes.Buffer
	for _, line := range bytes.SplitAfter(script, []byte("\n")) {
		content := strings.TrimRight(string(line), "\r\n")
		trimmed := strings.TrimLeft(content, " \t")
		replacement, ok := stamps[trimmed]
		if !ok {
			out.Write(line)
			continue
		}
		seen[trimmed]++
		indent := content[:len(content)-len(trimmed)]
		for stamped := range strings.SplitSeq(replacement, "\n") {
			if stamped != "" {
				out.WriteString(indent)
			}
			out.WriteString(stamped)
			out.WriteString("\n")
		}
	}
	for marker := range stamps {
		if seen[marker] != 1 {
			return nil, fmt.Errorf("%s: expected one %q line to stamp, found %d", name, marker, seen[marker])
		}
	}
	return out.Bytes(), nil
}

// Write hashes the binaries in dir and writes beside them the checksums and
// both installers, stamped for tag: every file a release uploads but the
// binaries themselves.
func Write(dir, tag string) error {
	if err := CheckTag(tag); err != nil {
		return err
	}
	digests, err := Hash(dir)
	if err != nil {
		return err
	}
	installers, err := Stamp(tag, digests)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ChecksumsName), Checksums(digests), 0o600); err != nil {
		return err
	}
	for name, data := range installers {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}
