package release

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTagsAreReleaseVersions(t *testing.T) {
	for _, tag := range []string{"v0.1.0", "v1.2.3", "v1.2.3-alpha.0", "v1.2.3-rc.12"} {
		if err := CheckTag(tag); err != nil {
			t.Errorf("CheckTag(%q): %v", tag, err)
		}
	}
	for _, tag := range []string{"", "1.2.3", "v1.2", "v01.2.3", "v1.2.3-beta.1", "v1.2.3-rc1", "v1.2.3+meta", "linux/v1", "v1.2.3'"} {
		if err := CheckTag(tag); err == nil {
			t.Errorf("CheckTag(%q) accepted it", tag)
		}
	}
	if Prerelease("v1.2.3") || !Prerelease("v1.2.3-rc.1") {
		t.Error("Prerelease: only a tag with a suffix is a prerelease")
	}
}

func digestsOf(sum string) []Digest {
	var d []Digest
	for _, name := range Binaries {
		d = append(d, Digest{Name: name, SHA256: sum})
	}
	return d
}

func TestChecksumsRoundTrip(t *testing.T) {
	digests := digestsOf(strings.Repeat("ab", 32))
	data := Checksums(digests)
	if !strings.HasPrefix(string(data), strings.Repeat("ab", 32)+"  disco-vm-darwin-arm64\n") {
		t.Errorf("not in sha256sum's format:\n%s", data)
	}
	got, err := ParseChecksums(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(digests) {
		t.Fatalf("read %d digests, want %d", len(got), len(digests))
	}
	if _, err := ParseChecksums(data[:len(data)-10]); err == nil {
		t.Error("a truncated file was read")
	}
	if _, err := ParseChecksums(append(data, data[:80]...)); err == nil {
		t.Error("a binary listed twice was read")
	}
}

func TestStampRefusesWhatCouldEscapeAQuote(t *testing.T) {
	if _, err := Stamp("v1.0.0'; rm -rf /", digestsOf(strings.Repeat("0", 64))); err == nil {
		t.Error("a tag with a quote was stamped")
	}
	if _, err := Stamp("v1.0.0", digestsOf("'")); err == nil {
		t.Error("a digest with a quote was stamped")
	}
	if _, err := Stamp("v1.0.0", []Digest{{Name: "x'y", SHA256: strings.Repeat("0", 64)}}); err == nil {
		t.Error("a name that is not a binary was stamped")
	}
}

func TestStampWritesTheReleaseIntoBothScripts(t *testing.T) {
	sum := strings.Repeat("c", 64)
	got, err := Stamp("v1.2.3", digestsOf(sum))
	if err != nil {
		t.Fatal(err)
	}
	sh := string(got[ShellName])
	if !strings.Contains(sh, "\nrelease='v1.2.3'\n") || !strings.Contains(sh, sum+"  disco-vm-linux-amd64\n") {
		t.Errorf("install.sh is not stamped:\n%s", sh)
	}
	ps := string(got[PowerShellName])
	if !strings.Contains(ps, "    $release = 'v1.2.3'\n") || !strings.Contains(ps, "        'disco-vm-windows-amd64.exe' = '"+sum+"'\n") {
		t.Errorf("install.ps1 is not stamped:\n%s", ps)
	}
	if bytes.Equal(got[ShellName], shellScript) {
		t.Error("the source copy was returned")
	}
}

// Windows PowerShell 5.1 reads a file with no byte order mark in the ANSI
// code page, so one non-ASCII byte makes install.ps1 unreadable there.
func TestScriptsAreASCII(t *testing.T) {
	for name, data := range map[string][]byte{ShellName: shellScript, PowerShellName: powerShellScript} {
		for i, b := range data {
			if b > 0x7f {
				t.Errorf("%s: byte %d is not ASCII", name, i)
				break
			}
		}
	}
}

func TestWriteNeedsEveryBinary(t *testing.T) {
	dir := t.TempDir()
	for _, name := range Binaries[1:] {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := Write(dir, "v1.0.0"); err == nil {
		t.Fatal("a release without its darwin binary was written")
	}
	if err := os.WriteFile(filepath.Join(dir, Binaries[0]), []byte(Binaries[0]), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	for _, name := range Assets() {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Error(err)
		}
	}
	sums, err := os.ReadFile(filepath.Join(dir, ChecksumsName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseChecksums(sums); err != nil {
		t.Error(err)
	}
}

func TestFormula(t *testing.T) {
	sum := strings.Repeat("d", 64)
	f, err := Formula("discobox-ai/vm", "v1.2.3", digestsOf(sum))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"class DiscoVm < Formula",
		`version "1.2.3"`,
		`url "https://github.com/discobox-ai/vm/releases/download/v1.2.3/disco-vm-darwin-arm64"`,
		`url "https://github.com/discobox-ai/vm/releases/download/v1.2.3/disco-vm-linux-arm64"`,
		`sha256 "` + sum + `"`,
		`chmod 0755, binary`,
		`bin.install binary => "disco-vm"`,
		`depends_on arch: :arm64`,
	} {
		if !strings.Contains(string(f), want) {
			t.Errorf("formula has no %s:\n%s", want, f)
		}
	}
	if strings.Contains(string(f), "post_install") {
		t.Error("the formula rewrites the signed binary after it installs it")
	}
	if _, err := Formula("discobox-ai/vm", "v1.2.3-rc.1", digestsOf(sum)); err == nil {
		t.Error("a prerelease's formula was written")
	}
	if _, err := Formula("discobox-ai/vm", "v1.2.3", digestsOf(sum)[:1]); err == nil {
		t.Error("a formula without its Linux binaries was written")
	}
	if _, err := Formula(`a"b/c`, "v1.2.3", digestsOf(sum)); err == nil {
		t.Error("a repository that could close a Ruby string was written")
	}
}
