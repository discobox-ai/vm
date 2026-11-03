//go:build linux

package libkrun

import (
	"archive/tar"
	"bytes"
	"context"
	"debug/elf"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/linuximage"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// The launcher's configuration names everything a boot needs, in the order
// launch.c reads it, and refuses a value it could not carry.
func TestRender(t *testing.T) {
	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	l := launch{
		cpus: 2, memory: 2 << 30, kernel: "/k", initrd: "/i", cmdline: "console=hvc0 root=/dev/vda",
		console: "/c", disk: "/d with space",
		in:    map[uint32]string{7302: "/s2", 7300: "/s0"},
		out:   map[uint32]string{7402: "/o"},
		netFD: 3, mac: mac,
		shares: []machine.Share{{Tag: "src", HostPath: "/home/me/src", ReadOnly: true}},
	}
	got, err := l.render()
	if err != nil {
		t.Fatal(err)
	}
	want := `krun-launch 1
cpus 2
memory 2048
kernel /k
initrd /i
cmdline console=hvc0 root=/dev/vda
console /c
disk vda rw /d with space
vsock-in 7300 /s0
vsock-in 7302 /s2
vsock-out 7402 /o
net 3 02:00:00:00:00:01
share src ro /home/me/src
`
	if string(got) != want {
		t.Fatalf("render:\n%s\nwant:\n%s", got, want)
	}
	l.netFD = 0
	l.shares = []machine.Share{{Tag: "a tag", HostPath: "/x"}}
	if _, err := l.render(); err == nil {
		t.Fatal("a share tag with a space was rendered")
	}
	l.shares = nil
	l.kernel = "/k\n"
	if _, err := l.render(); err == nil {
		t.Fatal("a path with a newline was rendered")
	}
}

// A copy where the filesystem has no reflinks keeps the source's holes.
func TestCopyFileSparse(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("head"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("tail"), 256<<20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	in, _ := os.ReadFile(src)
	out, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(in, out) {
		t.Fatalf("the copy differs: %v", err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(dst, &st); err != nil {
		t.Fatal(err)
	}
	if used := st.Blocks * 512; used > 16<<20 {
		t.Fatalf("the copy of a 256 MiB file holding 8 bytes uses %d bytes", used)
	}
	if err := copyFile(src, dst); err == nil {
		t.Fatal("a copy over an existing file")
	}
}

// Check names why a host cannot run libkrun.
func TestCheckHost(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		if err := checkHost(); err == nil || !strings.Contains(err.Error(), "linux/amd64 alone") {
			t.Fatalf("an %s host: %v", runtime.GOARCH, err)
		}
		return
	}
	old := kvmPath
	t.Cleanup(func() { kvmPath = old })
	kvmPath = filepath.Join(t.TempDir(), "kvm")
	if err := checkHost(); err == nil || !strings.Contains(err.Error(), "has no "+kvmPath) || !strings.Contains(err.Error(), "--driver docker") {
		t.Fatalf("no KVM: %v", err)
	}
	if err := os.WriteFile(kvmPath, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if err := checkHost(); err == nil || !strings.Contains(err.Error(), "kvm group") {
			t.Fatalf("a KVM this user cannot open: %v", err)
		}
	}
	if err := os.Chmod(kvmPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkHost(); err != nil {
		t.Fatalf("a KVM this user opens: %v", err)
	}
}

// fakeRuntime is a runtime whose loader is a script that runs its fourth
// argument on (after --inhibit-cache --library-path LIB), and whose launcher answers --version as launch.c does and
// otherwise does what launch says.
func fakeRuntime(t *testing.T, config, launch string) map[string]string {
	t.Helper()
	return map[string]string{ //nolint:gosec // G101: shell scripts, no credentials
		filepath.Join(libDir, loaders[runtime.GOARCH]): "#!/bin/sh\nshift 3\nexec \"$@\"\n",
		launcherName:                          "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'krun-launch 1'; exit 0; fi\n" + launch + "\n",
		passtName:                             "#!/bin/sh\nexit 0\n",
		filepath.Join(libDir, "libkrun.so.1"): "not a library",
		manifestName:                          `{"version":"test","libkrun":"1.19.7","passt":"test","config":"` + config + `"}`,
	}
}

func writeRuntime(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for file, data := range files {
		path := filepath.Join(dir, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o755); err != nil { //nolint:gosec // G306: the runtime's executables
			t.Fatal(err)
		}
	}
	return dir
}

// A runtime runs its launcher's --version through its own loader, and one
// that does not run, or reads another configuration version, is refused
// naming why.
func TestCheckRuntime(t *testing.T) {
	ctx := context.Background()
	if err := checkRuntime(ctx, writeRuntime(t, fakeRuntime(t, launchVersion, ""))); err != nil {
		t.Fatal(err)
	}
	if err := checkRuntime(ctx, writeRuntime(t, fakeRuntime(t, "2", ""))); err == nil || !strings.Contains(err.Error(), `version "2", and this disco-vm writes "1"`) {
		t.Fatalf("a runtime for another configuration version: %v", err)
	}
	broken := fakeRuntime(t, launchVersion, "")
	broken[launcherName] = "#!/bin/sh\necho 'error while loading shared libraries: libkrun.so.1' >&2\nexit 127\n"
	if err := checkRuntime(ctx, writeRuntime(t, broken)); err == nil || !strings.Contains(err.Error(), "error while loading shared libraries") {
		t.Fatalf("a runtime whose launcher does not load: %v", err)
	}
	partial := fakeRuntime(t, launchVersion, "")
	delete(partial, passtName)
	if err := checkRuntime(ctx, writeRuntime(t, partial)); err == nil || !strings.Contains(err.Error(), "has no bin/passt") {
		t.Fatalf("a runtime with no passt: %v", err)
	}
}

// A runtime whose launcher needs a library lib/ lacks is refused, naming
// it, before the loader can take the host's.
func TestRuntimeLacksALibrary(t *testing.T) {
	dynamic := "/bin/true"
	f, err := elf.Open(dynamic)
	if err != nil {
		t.Skipf("no ELF %s: %v", dynamic, err)
	}
	needed, err := f.ImportedLibraries()
	_ = f.Close()
	if err != nil || len(needed) == 0 {
		t.Skipf("%s needs no library", dynamic)
	}
	dir := writeRuntime(t, fakeRuntime(t, launchVersion, ""))
	data, err := os.ReadFile(dynamic)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, launcherName), data, 0o755); err != nil { //nolint:gosec // G306: an executable
		t.Fatal(err)
	}
	if err := completeRuntime(dir); err == nil || !strings.Contains(err.Error(), "has no lib/"+needed[0]+", which krun-launch needs") {
		t.Fatalf("a launcher needing %s, which lib/ lacks: %v", needed[0], err)
	}
}

// pushRuntime pushes a runtime image of files, executable, for linux/arch.
func pushRuntime(t *testing.T, files map[string]string, arch string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for file, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: file, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	config, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS, config.Architecture = "linux", arch
	if img, err = mutate.ConfigFile(img, config); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	ref := strings.TrimPrefix(server.URL, "http://") + "/krun:v1"
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	return ref
}

// A runtime is pulled once per digest into the cache, executable, and only
// once its launcher runs; one for another platform, or whose launcher does
// not run, is refused and never cached.
func TestPullRuntime(t *testing.T) {
	ctx := context.Background()
	cache := t.TempDir()
	ref := pushRuntime(t, fakeRuntime(t, launchVersion, ""), runtime.GOARCH)
	dir, err := pullRuntime(ctx, cache, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, filepath.Join(cache, "runtime")+string(filepath.Separator)) {
		t.Fatalf("pulled into %s", dir)
	}
	if got, ok := cachedRuntimeOf(cache, ref); !ok || got != dir {
		t.Fatalf("the cache does not find the pull: %s, %v", got, ok)
	}
	if again, err := pullRuntime(ctx, cache, ref); err != nil || again != dir {
		t.Fatalf("a second pull: %s, %v", again, err)
	}
	other := map[string]string{"amd64": "arm64", "arm64": "amd64"}[runtime.GOARCH]
	if _, err := pullRuntime(ctx, t.TempDir(), pushRuntime(t, fakeRuntime(t, launchVersion, ""), other)); err == nil || !strings.Contains(err.Error(), "is for linux/"+other) {
		t.Fatalf("a runtime for linux/%s: %v", other, err)
	}
	broken := fakeRuntime(t, launchVersion, "")
	broken[launcherName] = "#!/bin/sh\nexit 1\n"
	empty := t.TempDir()
	if _, err := pullRuntime(ctx, empty, pushRuntime(t, broken, runtime.GOARCH)); err == nil || !strings.Contains(err.Error(), "does not run") {
		t.Fatalf("a runtime whose launcher does not run: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(empty, "runtime")); len(entries) != 0 {
		t.Fatalf("a runtime that does not run was cached: %v", entries)
	}
}

// guestDir is a guest image's artifacts, libkrun's kernel among them.
func guestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, artifact := range []string{linuximage.KrunKernel, linuximage.Initrd, linuximage.Root} {
		path := filepath.Join(dir, artifact)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(artifact), 0o644); err != nil { //nolint:gosec // G306: test data
			t.Fatal(err)
		}
	}
	return dir
}

// usableKVM points Check at a file this user can open, for tests that boot
// no VM.
func usableKVM(t *testing.T) {
	t.Helper()
	if runtime.GOARCH != "amd64" {
		t.Skip("libkrun runs on linux/amd64 alone")
	}
	old := kvmPath
	t.Cleanup(func() { kvmPath = old })
	kvmPath = filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(kvmPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Prepare makes a fresh bundle from the base, its disk grown sparse and an
// identity of its own; a copy of a committed one has another identity; and
// a commit keeps only what a copy needs. A launcher that exits before the
// agent answers is refused with its standard error, the boot's sockets gone.
func TestPrepareCommitBoot(t *testing.T) {
	usableKVM(t)
	ctx := context.Background()
	rt := writeRuntime(t, fakeRuntime(t, launchVersion, "echo 'krun-launch: krun_start_enter: Operation not permitted' >&2; exit 1"))
	h := &Hypervisor{base: guestDir(t), runtimeDir: rt}
	root := t.TempDir()
	fresh := local.InstanceSpec{ID: "a", Dir: filepath.Join(root, "a"), GuestOS: machine.Linux, CacheDir: filepath.Join(root, "cache")}
	agentForward := fresh
	agentForward.Forwards = []machine.Forward{{Port: guest.AgentPort, Socket: "/s"}}
	if err := h.Prepare(ctx, agentForward); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("a forward of the agent's port: %v, want ErrUnsupported", err)
	}
	if err := h.Prepare(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	b := bundle(fresh.Dir)
	if err := b.prepared(); err != nil {
		t.Fatal(err)
	}
	if kernel, err := os.ReadFile(b.path(kernelName)); err != nil || string(kernel) != linuximage.KrunKernel {
		t.Fatalf("the bundle's kernel is %q, %v; want the base's libkrun kernel", kernel, err)
	}
	if info, err := os.Stat(b.path(diskName)); err != nil || info.Size() != diskCeiling {
		t.Fatalf("the disk is %v, %v; want it grown to %d", info.Size(), err, int64(diskCeiling))
	}
	if err := h.Commit(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.path(machineIDName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a committed VM keeps its machine ID: %v", err)
	}
	copied := local.InstanceSpec{ID: "b", Dir: filepath.Join(root, "b"), GuestOS: machine.Linux, CacheDir: fresh.CacheDir, Chain: []local.Layer{{ID: "a", Dir: fresh.Dir}}}
	if err := h.Prepare(ctx, copied); err != nil {
		t.Fatal(err)
	}
	if err := bundle(copied.Dir).prepared(); err != nil {
		t.Fatal(err)
	}

	copied.Egress = &machine.Egress{}
	copied.Forwards = []machine.Forward{{Port: 7402, Socket: "/nowhere"}}
	var console bytes.Buffer
	_, err := h.Boot(ctx, copied, local.BootOptions{Console: &console})
	if err == nil || !strings.Contains(err.Error(), "krun_start_enter: Operation not permitted") {
		t.Fatalf("a launcher that fails: %v, want its standard error", err)
	}
	conf, err := os.ReadFile(bundle(copied.Dir).path(launchName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"vsock-in 7300 ", "vsock-in 7302 ", "vsock-out 7402 ", "disco-vm.nic=0", "reboot=k"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("launch.conf lacks %q:\n%s", want, conf)
		}
	}
	if strings.Contains(string(conf), "\nnet ") {
		t.Errorf("a fenced VM has a network device:\n%s", conf)
	}
	for _, sock := range []string{"vsock-in-7300.sock", "vsock-out-7402.sock"} {
		if _, err := os.Stat(filepath.Join(copied.Dir, sock)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s outlived a failed boot: %v", sock, err)
		}
	}
	if _, _, port, err := func() (string, string, uint32, error) {
		listen, port, err := h.Service(copied)
		return listen, "", port, err
	}(); err != nil || port != guest.ServicePort {
		t.Fatalf("service port %d, %v", port, err)
	}
}
