package release

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// FormulaPath is where the formula lives in the tap, which it shares with
// discobox's: `brew install discobox-ai/tap/disco-vm`.
const FormulaPath = "Formula/disco-vm.rb"

// The formula serves each platform Homebrew runs on from the release's own
// binary, by URL and digest. It has no post-install step, so nothing rewrites
// the signed macOS binary, and its test runs the binary, which is what a
// binary for the wrong platform fails.
var formulaTemplate = template.Must(template.New("formula").Parse(`# Written by internal/cmd/disco-vm-release in {{.Repo}} for {{.Tag}}.
# Do not edit by hand: the next release overwrites it.
class DiscoVm < Formula
  desc "Run VMs, fresh or from committed ones, and commit them to make more"
  homepage "https://github.com/{{.Repo}}"
  version "{{.Version}}"
  license "Apache-2.0"

  on_macos do
    # Apple silicon only: vz's macOS guests need it.
    depends_on arch: :arm64
    on_arm do
      url "{{.URL}}/disco-vm-darwin-arm64"
      sha256 "{{index .SHA256 "disco-vm-darwin-arm64"}}"
    end
  end

  on_linux do
    on_intel do
      url "{{.URL}}/disco-vm-linux-amd64"
      sha256 "{{index .SHA256 "disco-vm-linux-amd64"}}"
    end
    on_arm do
      url "{{.URL}}/disco-vm-linux-arm64"
      sha256 "{{index .SHA256 "disco-vm-linux-arm64"}}"
    end
  end

  def install
    # A bare download is staged without its executable bit, and Homebrew's
    # cleaner leaves a file it finds not executable unrunnable. A mode change
    # keeps the macOS signature.
    binary = Dir["disco-vm-*"].first
    chmod 0755, binary
    bin.install binary => "disco-vm"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/disco-vm --version")
  end
end
`))

// Formula returns the Homebrew formula that installs release tag of repo
// from digests.
func Formula(repo, tag string, digests []Digest) ([]byte, error) {
	if err := CheckTag(tag); err != nil {
		return nil, err
	}
	if err := CheckRepo(repo); err != nil {
		return nil, err
	}
	if Prerelease(tag) {
		return nil, fmt.Errorf("%s is a prerelease, which never reaches the tap", tag)
	}
	sums := map[string]string{}
	for _, d := range digests {
		if !digestPattern.MatchString(d.SHA256) {
			return nil, fmt.Errorf("%s: %q is not a lowercase hex SHA-256", d.Name, d.SHA256)
		}
		sums[d.Name] = d.SHA256
	}
	for _, name := range []string{"disco-vm-darwin-arm64", "disco-vm-linux-amd64", "disco-vm-linux-arm64"} {
		if sums[name] == "" {
			return nil, fmt.Errorf("the formula needs %s, and %s has no digest of it", name, tag)
		}
	}
	var b bytes.Buffer
	err := formulaTemplate.Execute(&b, map[string]any{
		"Repo":    repo,
		"Tag":     tag,
		"Version": strings.TrimPrefix(tag, "v"),
		"URL":     "https://github.com/" + repo + "/releases/download/" + tag,
		"SHA256":  sums,
	})
	return b.Bytes(), err
}
