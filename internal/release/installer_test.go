package release

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

// hostAsset is the release binary an installer picks on this machine.
func hostAsset(t *testing.T) string {
	t.Helper()
	name := "disco-vm-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if slices.Contains(Binaries, name) {
		return name
	}
	t.Skipf("no release binary for %s/%s", runtime.GOOS, runtime.GOARCH)
	return ""
}

var (
	fakeMu    sync.Mutex
	fakeBuilt = map[string][]byte{}
)

// fakeBinary is a program for this machine that answers --version as disco-vm
// tag does.
func fakeBinary(t *testing.T, tag string) []byte {
	t.Helper()
	fakeMu.Lock()
	defer fakeMu.Unlock()
	if b, ok := fakeBuilt[tag]; ok {
		return b
	}
	out := filepath.Join(t.TempDir(), "fakevm")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", out, "-ldflags", "-X main.version="+tag, "./testdata/fakevm")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fakevm: %v\n%s", err, msg)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	fakeBuilt[tag] = b
	return b
}

// fakeReleases serves releases as GitHub does, each file at
// /download/<tag>/<name>, and the newest full release's through a redirect
// from /latest/download/<name>.
type fakeReleases struct {
	t      *testing.T
	mu     sync.Mutex
	files  map[string][]byte
	latest string
	srv    *httptest.Server
}

func newFakeReleases(t *testing.T) *fakeReleases {
	t.Helper()
	f := &fakeReleases{t: t, files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if rest, ok := strings.CutPrefix(r.URL.Path, "/latest/download/"); ok && f.latest != "" {
			http.Redirect(w, r, "/download/"+f.latest+"/"+rest, http.StatusFound) //nolint:gosec // G710: a path on this test server
			return
		}
		data, ok := f.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// release publishes tag: this machine's binary a fake disco-vm of tag, and
// the others stand-ins. stamped false uploads the source installers
// unstamped, as a broken release would.
func (f *fakeReleases) release(tag string, stamped bool) {
	t := f.t
	t.Helper()
	host := hostAsset(t)
	var digests []Digest
	files := map[string][]byte{}
	for _, name := range Binaries {
		data := []byte(name + " " + tag)
		if name == host {
			data = fakeBinary(t, tag)
		}
		sum := sha256.Sum256(data)
		files[name] = data
		digests = append(digests, Digest{Name: name, SHA256: hex.EncodeToString(sum[:])})
	}
	files[ShellName], files[PowerShellName] = shellScript, powerShellScript
	if stamped {
		installers, err := Stamp(tag, digests)
		if err != nil {
			t.Fatal(err)
		}
		files[ShellName], files[PowerShellName] = installers[ShellName], installers[PowerShellName]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, data := range files {
		f.files["/download/"+tag+"/"+name] = data
	}
	if !Prerelease(tag) {
		f.latest = tag
	}
}

func (f *fakeReleases) file(tag, name string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files["/download/"+tag+"/"+name]
}

func (f *fakeReleases) replace(tag, name string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files["/download/"+tag+"/"+name] = data
}

// An installer runs one of the two scripts the way a user would.
type installer struct {
	name string
	run  func(t *testing.T, script []byte, dir string, env []string, version string) (string, error)
}

// installers are the scripts this machine runs: install.sh off Windows, and
// install.ps1 on Windows, under every PowerShell there is.
func installers(t *testing.T) []installer {
	t.Helper()
	if runtime.GOOS != "windows" {
		if _, err := exec.LookPath("sh"); err != nil {
			t.Skip("no sh")
		}
		return []installer{{name: "install.sh", run: func(t *testing.T, script []byte, dir string, env []string, version string) (string, error) {
			t.Helper()
			path := filepath.Join(t.TempDir(), "install.sh")
			if err := os.WriteFile(path, script, 0o644); err != nil {
				t.Fatal(err)
			}
			args := []string{path, "--dir", dir}
			if version != "" {
				args = append(args, "--version", version)
			}
			cmd := exec.CommandContext(t.Context(), "sh", args...)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			return string(out), err
		}}}
	}
	var out []installer
	for _, shell := range []string{"powershell", "pwsh"} {
		if _, err := exec.LookPath(shell); err != nil {
			continue
		}
		out = append(out, installer{name: "install.ps1 (" + shell + ")", run: func(t *testing.T, script []byte, dir string, env []string, version string) (string, error) {
			t.Helper()
			path := filepath.Join(t.TempDir(), "install.ps1")
			if err := os.WriteFile(path, script, 0o644); err != nil {
				t.Fatal(err)
			}
			args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path, "-InstallDir", dir}
			if version != "" {
				args = append(args, "-Version", version)
			}
			cmd := exec.CommandContext(t.Context(), shell, args...)
			// The tests never write the user's PATH.
			cmd.Env = append(env, "DISCO_VM_INSTALL_NO_MODIFY_PATH=1")
			out, err := cmd.CombinedOutput()
			return string(out), err
		}})
	}
	if len(out) == 0 {
		t.Skip("no PowerShell")
	}
	return out
}

func scriptName() string {
	if runtime.GOOS == "windows" {
		return PowerShellName
	}
	return ShellName
}

func sourceScript() []byte {
	if runtime.GOOS == "windows" {
		return powerShellScript
	}
	return shellScript
}

func installEnv(f *fakeReleases) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DISCO_VM_INSTALL_") {
			env = append(env, kv)
		}
	}
	return append(env, "DISCO_VM_INSTALL_RELEASES="+f.srv.URL)
}

// installed returns what the disco-vm installed in dir says it is.
func installed(t *testing.T, dir string) string {
	t.Helper()
	name := "disco-vm"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out, err := exec.CommandContext(t.Context(), filepath.Join(dir, name), "--version").Output() //nolint:gosec // G204: the binary this test installed
	if err != nil {
		t.Fatalf("the installed disco-vm does not run: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestAStampedInstallerInstallsItsOwnRelease(t *testing.T) {
	f := newFakeReleases(t)
	f.release("v1.2.2", true)
	f.release("v1.2.3", true)
	for _, in := range installers(t) {
		t.Run(in.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bin")
			if out, err := in.run(t, f.file("v1.2.2", scriptName()), dir, installEnv(f), ""); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if got := installed(t, dir); got != "disco-vm version v1.2.2" {
				t.Errorf("installed %q, want v1.2.2", got)
			}
		})
	}
}

func TestAnInstallerRefusesABinaryItsReleaseDidNotUpload(t *testing.T) {
	f := newFakeReleases(t)
	f.release("v1.2.3", true)
	f.replace("v1.2.3", hostAsset(t), fakeBinary(t, "v6.6.6"))
	for _, in := range installers(t) {
		t.Run(in.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bin")
			out, err := in.run(t, f.file("v1.2.3", scriptName()), dir, installEnv(f), "")
			if err == nil || !strings.Contains(out, "is not the file v1.2.3 was released with") {
				t.Fatalf("installed a binary of another digest: %v\n%s", err, out)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("left %d files in %s", len(entries), dir)
			}
		})
	}
}

func TestAnInstallerHandsAnotherVersionToThatReleasesInstaller(t *testing.T) {
	f := newFakeReleases(t)
	f.release("v1.2.3", true)
	f.release("v1.3.0-rc.1", true)
	for _, in := range installers(t) {
		t.Run(in.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bin")
			// Without its v, as a user may type it.
			if out, err := in.run(t, f.file("v1.2.3", scriptName()), dir, installEnv(f), "1.3.0-rc.1"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if got := installed(t, dir); got != "disco-vm version v1.3.0-rc.1" {
				t.Errorf("installed %q, want v1.3.0-rc.1", got)
			}
		})
	}
}

func TestTheSourceInstallerInstallsTheNewestFullRelease(t *testing.T) {
	f := newFakeReleases(t)
	f.release("v1.2.3", true)
	f.release("v1.3.0-rc.1", true)
	for _, in := range installers(t) {
		t.Run(in.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bin")
			if out, err := in.run(t, sourceScript(), dir, installEnv(f), ""); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if got := installed(t, dir); got != "disco-vm version v1.2.3" {
				t.Errorf("installed %q, want the newest full release, v1.2.3", got)
			}
		})
	}
}

func TestAnInstallerStopsAtASecondHandOver(t *testing.T) {
	f := newFakeReleases(t)
	f.release("v1.2.3", false)
	for _, in := range installers(t) {
		t.Run(in.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bin")
			out, err := in.run(t, sourceScript(), dir, installEnv(f), "v1.2.3")
			if err == nil || !strings.Contains(out, "the installer v1.2.3 uploaded is stamped for") {
				t.Fatalf("followed an unstamped release's installer: %v\n%s", err, out)
			}
		})
	}
}

func TestAnInstallerRefusesAVersionThatIsNotOne(t *testing.T) {
	f := newFakeReleases(t)
	f.release("v1.2.3", true)
	for _, in := range installers(t) {
		t.Run(in.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bin")
			out, err := in.run(t, f.file("v1.2.3", scriptName()), dir, installEnv(f), "v1.2.3/../x")
			if err == nil || !strings.Contains(out, "is not a release version") {
				t.Fatalf("took a version that is not one: %v\n%s", err, out)
			}
		})
	}
}
