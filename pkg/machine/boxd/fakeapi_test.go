package boxd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// fakeAPI is boxd's API with no cloud behind it. A machine is a directory
// standing in for its disk, and a snapshot is a copy of that directory.
// Memory is not modeled. Exec understands the command lines the driver sends:
// a request's argv runs on this host, in the machine's directory, and the
// driver's own scripts (copies, info, systemd's, sync) are done here instead.
//
// A machine's power is the API's alone: StopVm, StartVm, RebootVm. The driver
// asks nothing in the guest to power off.
type fakeAPI struct {
	boxdapi.UnimplementedBoxdApiServer
	dir string

	mu        sync.Mutex
	vms       map[string]*fakeVM
	snapshots map[string]*fakeSnapshot
	// defaultImage is GetConfig's answer: the image a machine gets when
	// CreateVm names none.
	defaultImage string

	// down makes GetVm and StopVm fail as an unreachable API does.
	down atomic.Bool
	// proxyErr, when set, is SetProxyPort's answer.
	proxyErr error
	// syncFails makes the guest's sync exit 1.
	syncFails atomic.Bool
	// egressIgnored makes SetVmEgressAllow answer and keep nothing, so
	// GetVm reports the machine unrestricted.
	egressIgnored atomic.Bool

	keys keys
	// addr and tokenURL are where the API and its token exchange are
	// served.
	addr, tokenURL string
}

type fakeVM struct {
	id, name string
	dir      string
	status   string
	vcpu     uint32
	memory   uint64
	// reboots counts RebootVm calls.
	reboots int
	// proxyPort is the port the default proxy is pinned to, zero for auto;
	// botProtection is on for a new machine, as on boxd.
	proxyPort     uint32
	botProtection bool
	// isolated is immutable, and networks set at create or by
	// SetVmNetworks.
	isolated bool
	networks []string
	// egressAtSettle is the allowlist when the driver turned off the idle
	// timers, its first call after the create.
	egressAtSettle []string
	// egress is the allowlist, empty for none. A machine starts with none,
	// a restore too: only a snapshot of a machine spec, which the driver
	// never makes, carries one.
	egress []string
}

type fakeSnapshot struct {
	id, name string
	dir      string
	vcpu     uint32
	memory   uint64
	// isolated is its machine's, which a restore inherits.
	isolated bool
}

func newFakeAPI(dir string) *fakeAPI {
	return &fakeAPI{
		dir: dir, vms: map[string]*fakeVM{}, snapshots: map[string]*fakeSnapshot{}, defaultImage: "ubuntu-24.04",
		keys: keys{byID: map[string]*fakeKey{}, jwts: map[string]string{}},
	}
}

// snapshot finds a snapshot by name or ID, as boxd does.
func (f *fakeAPI) snapshot(ref string) (*fakeSnapshot, error) {
	for _, snap := range f.snapshots {
		if snap.name == ref || snap.id == ref {
			return snap, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "no snapshot %q", ref)
}

func (f *fakeAPI) root(v *fakeVM) string { return filepath.Join(v.dir, "root") }

func (f *fakeAPI) vm(id string) (*fakeVM, error) {
	for _, v := range f.vms {
		if v.id == id || v.name == id {
			return v, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "no machine %q", id)
}

// taken reports whether a machine has the name: boxd's names are unique.
func (f *fakeAPI) taken(name string) bool {
	for _, v := range f.vms {
		if name != "" && v.name == name {
			return true
		}
	}
	return false
}

func (f *fakeAPI) newVM(name string, vcpu uint32, memory uint64) *fakeVM {
	id := "vm_" + fsutil.RandomHex(4)
	if name == "" {
		name = id
	}
	if vcpu == 0 {
		vcpu, memory = 2, 8<<30
	}
	v := &fakeVM{id: id, name: name, dir: filepath.Join(f.dir, id), status: "running", vcpu: vcpu, memory: memory, botProtection: true}
	f.vms[id] = v
	return v
}

func (f *fakeAPI) boot(v *fakeVM) { v.status = "running" }

func (f *fakeAPI) halt(v *fakeVM) { v.status = "stopped" }

// RebootVm restarts a running machine's guest, which leaves it running.
func (f *fakeAPI) RebootVm(_ context.Context, req *boxdapi.RebootVmRequest) (*boxdapi.RebootVmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	if v.status != "running" {
		return nil, status.Errorf(codes.FailedPrecondition, "%s is %s, not running", v.name, v.status)
	}
	v.reboots++
	return &boxdapi.RebootVmResponse{}, nil
}

func (f *fakeAPI) GetConfig(context.Context, *boxdapi.GetConfigRequest) (*boxdapi.GetConfigResponse, error) {
	return &boxdapi.GetConfigResponse{DefaultImage: f.defaultImage}, nil
}

func (f *fakeAPI) Whoami(context.Context, *boxdapi.WhoamiRequest) (*boxdapi.WhoamiResponse, error) {
	return &boxdapi.WhoamiResponse{UserId: "usr_fake"}, nil
}

func (f *fakeAPI) CreateVm(_ context.Context, req *boxdapi.CreateVmRequest) (*boxdapi.CreateVmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.taken(req.GetName()) {
		return nil, status.Errorf(codes.AlreadyExists, "a machine is named %q", req.GetName())
	}
	v := f.newVM(req.GetName(), req.GetConfig().GetVcpu(), req.GetConfig().GetMemoryBytes())
	v.isolated, v.networks = req.GetIsolated(), req.GetNetworks()
	if err := os.MkdirAll(f.root(v), 0o755); err != nil {
		return nil, err
	}
	return &boxdapi.CreateVmResponse{VmId: v.id, Name: v.name, Status: v.status}, nil
}

func (f *fakeAPI) CreateVmFromSnapshot(_ context.Context, req *boxdapi.CreateVmFromSnapshotRequest) (*boxdapi.CreateVmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap, err := f.snapshot(req.GetSnapshot())
	if err != nil {
		return nil, err
	}
	if f.taken(req.GetName()) {
		return nil, status.Errorf(codes.AlreadyExists, "a machine is named %q", req.GetName())
	}
	// The config is "optional overrides" of the snapshot's size, each field
	// on its own: zero keeps the snapshot's.
	vcpu, memory := snap.vcpu, snap.memory
	if c := req.GetConfig().GetVcpu(); c != 0 {
		vcpu = c
	}
	if c := req.GetConfig().GetMemoryBytes(); c != 0 {
		memory = c
	}
	v := f.newVM(req.GetName(), vcpu, memory)
	v.isolated = snap.isolated || req.GetIsolated()
	if err := fsutil.CopyTree(snap.dir, f.root(v)); err != nil {
		return nil, err
	}
	f.boot(v)
	return &boxdapi.CreateVmResponse{VmId: v.id, Name: v.name, Status: v.status}, nil
}

func (f *fakeAPI) SetVmNetworks(_ context.Context, req *boxdapi.SetVmNetworksRequest) (*boxdapi.SetVmNetworksResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	v.networks = req.GetNetworks()
	return &boxdapi.SetVmNetworksResponse{Networks: v.networks}, nil
}

// SetVmEgressAllow replaces the allowlist: empty is none, and "none" next to
// other entries is dropped.
func (f *fakeAPI) SetVmEgressAllow(_ context.Context, req *boxdapi.SetVmEgressAllowRequest) (*boxdapi.SetVmEgressAllowResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVm())
	if err != nil {
		return nil, err
	}
	entries := req.GetEntries()
	if len(entries) > 1 {
		entries = slices.DeleteFunc(slices.Clone(entries), func(e string) bool { return e == "none" })
	}
	if !f.egressIgnored.Load() {
		v.egress = entries
	}
	return &boxdapi.SetVmEgressAllowResponse{Entries: entries}, nil
}

func (f *fakeAPI) GetVm(_ context.Context, req *boxdapi.GetVmRequest) (*boxdapi.GetVmResponse, error) {
	if f.down.Load() {
		return nil, status.Error(codes.Unavailable, "fake boxd is down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	return f.info(v), nil
}

// info is a machine as GetVm and ListVms report it.
func (f *fakeAPI) info(v *fakeVM) *boxdapi.GetVmResponse {
	return &boxdapi.GetVmResponse{
		VmId: v.id, Name: v.name, Status: v.status, Vcpu: v.vcpu, MemoryBytes: v.memory,
		Isolated: v.isolated, Networks: v.networks, EgressAllow: v.egress,
	}
}

func (f *fakeAPI) ListVms(context.Context, *boxdapi.ListVmsRequest) (*boxdapi.ListVmsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &boxdapi.ListVmsResponse{}
	for _, v := range f.vms {
		out.Vms = append(out.Vms, f.info(v))
	}
	return out, nil
}

func (f *fakeAPI) DestroyVm(_ context.Context, req *boxdapi.DestroyVmRequest) (*boxdapi.DestroyVmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	f.halt(v)
	delete(f.vms, v.id)
	return &boxdapi.DestroyVmResponse{}, os.RemoveAll(v.dir)
}

func (f *fakeAPI) StartVm(_ context.Context, req *boxdapi.StartVmRequest) (*boxdapi.StartVmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	if v.status != "stopped" {
		return nil, status.Errorf(codes.FailedPrecondition, "%s is %s", v.name, v.status)
	}
	f.boot(v)
	return &boxdapi.StartVmResponse{}, nil
}

func (f *fakeAPI) StopVm(_ context.Context, req *boxdapi.StopVmRequest) (*boxdapi.StopVmResponse, error) {
	if f.down.Load() {
		return nil, status.Error(codes.Unavailable, "fake boxd is down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	f.halt(v)
	return &boxdapi.StopVmResponse{}, nil
}

func (f *fakeAPI) setStatus(id, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(id)
	if err != nil {
		return err
	}
	if v.status != from {
		return status.Errorf(codes.FailedPrecondition, "%s is %s, not %s", v.name, v.status, from)
	}
	v.status = to
	return nil
}

func (f *fakeAPI) SuspendVm(_ context.Context, req *boxdapi.SuspendVmRequest) (*boxdapi.SuspendVmResponse, error) {
	return &boxdapi.SuspendVmResponse{}, f.setStatus(req.GetVmId(), "running", "suspended")
}

func (f *fakeAPI) ResumeVm(_ context.Context, req *boxdapi.ResumeVmRequest) (*boxdapi.ResumeVmResponse, error) {
	return &boxdapi.ResumeVmResponse{}, f.setStatus(req.GetVmId(), "suspended", "running")
}

func (f *fakeAPI) SetAutoSuspendTimeout(_ context.Context, req *boxdapi.SetAutoSuspendTimeoutRequest) (*boxdapi.SetAutoSuspendTimeoutResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	v.egressAtSettle = v.egress
	return &boxdapi.SetAutoSuspendTimeoutResponse{}, nil
}

func (f *fakeAPI) SetAutoHibernateTimeout(context.Context, *boxdapi.SetAutoHibernateTimeoutRequest) (*boxdapi.SetAutoHibernateTimeoutResponse, error) {
	return &boxdapi.SetAutoHibernateTimeoutResponse{}, nil
}

func (f *fakeAPI) ResizeVm(_ context.Context, req *boxdapi.ResizeVmRequest) (*boxdapi.ResizeVmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	if v.status != "stopped" {
		return nil, status.Error(codes.FailedPrecondition, "the fake resizes stopped machines only")
	}
	v.vcpu, v.memory = req.GetVcpu(), req.GetMemoryBytes()
	return &boxdapi.ResizeVmResponse{Vcpu: v.vcpu, MemoryBytes: v.memory}, nil
}

func (f *fakeAPI) ListProxies(_ context.Context, req *boxdapi.ListProxiesRequest) (*boxdapi.ListProxiesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVm())
	if err != nil {
		return nil, err
	}
	// boxd's default proxy is not named "default", so neither is this one.
	p := &boxdapi.ProxyInfo{Name: v.name, VmName: v.name, VmId: v.id, Domain: v.name + ".boxd.test", IsDefault: true, PortMode: "auto", EffectivePort: 8000}
	if v.proxyPort != 0 {
		p.PortMode, p.EffectivePort = "locked", v.proxyPort
	}
	return &boxdapi.ListProxiesResponse{Proxies: []*boxdapi.ProxyInfo{p}}, nil
}

func (f *fakeAPI) SetProxyPort(_ context.Context, req *boxdapi.SetProxyPortRequest) (*boxdapi.SetProxyPortResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVm())
	if err != nil {
		return nil, err
	}
	if f.proxyErr != nil {
		return nil, f.proxyErr
	}
	if req.GetName() != v.name {
		return nil, status.Errorf(codes.NotFound, "%s has no proxy %q", v.name, req.GetName())
	}
	port, err := strconv.ParseUint(req.GetPort(), 10, 16)
	if err != nil || port == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "port %q", req.GetPort())
	}
	v.proxyPort = uint32(port)
	return &boxdapi.SetProxyPortResponse{}, nil
}

func (f *fakeAPI) SetBotProtection(_ context.Context, req *boxdapi.SetBotProtectionRequest) (*boxdapi.SetBotProtectionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	v.botProtection = req.GetEnabled()
	return &boxdapi.SetBotProtectionResponse{}, nil
}

func (f *fakeAPI) CreateSnapshot(_ context.Context, req *boxdapi.CreateSnapshotRequest) (*boxdapi.CreateSnapshotResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVm())
	if err != nil {
		return nil, err
	}
	if v.status != "running" {
		return nil, status.Errorf(codes.FailedPrecondition, "%s is %s, not running", v.name, v.status)
	}
	snap := &fakeSnapshot{
		id: "snap_" + fsutil.RandomHex(4), name: req.GetName(), vcpu: v.vcpu, memory: v.memory, isolated: v.isolated,
	}
	snap.dir = filepath.Join(f.dir, snap.id)
	if err := fsutil.CopyTree(f.root(v), snap.dir); err != nil {
		return nil, err
	}
	f.snapshots[snap.name] = snap
	return &boxdapi.CreateSnapshotResponse{SnapshotId: snap.id, Name: snap.name, Version: 1, Status: "pending"}, nil
}

func (f *fakeAPI) GetSnapshot(_ context.Context, req *boxdapi.GetSnapshotRequest) (*boxdapi.GetSnapshotResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap, err := f.snapshot(req.GetName())
	if err != nil {
		return nil, err
	}
	return &boxdapi.GetSnapshotResponse{Snapshot: &boxdapi.SnapshotInfo{
		SnapshotId: snap.id, Name: snap.name, Version: 1, Status: "ready", Vcpu: snap.vcpu, MemoryBytes: snap.memory,
	}}, nil
}

func (f *fakeAPI) ListSnapshots(context.Context, *boxdapi.ListSnapshotsRequest) (*boxdapi.ListSnapshotsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &boxdapi.ListSnapshotsResponse{}
	for _, snap := range f.snapshots {
		out.Snapshots = append(out.Snapshots, &boxdapi.SnapshotInfo{
			SnapshotId: snap.id, Name: snap.name, Version: 1, Status: "ready", Vcpu: snap.vcpu, MemoryBytes: snap.memory,
		})
	}
	return out, nil
}

func (f *fakeAPI) DeleteSnapshot(_ context.Context, req *boxdapi.DeleteSnapshotRequest) (*boxdapi.DeleteSnapshotResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap, err := f.snapshot(req.GetName())
	if err != nil {
		return nil, err
	}
	delete(f.snapshots, snap.name)
	return &boxdapi.DeleteSnapshotResponse{}, os.RemoveAll(snap.dir)
}

func (f *fakeAPI) Exec(stream grpc.BidiStreamingServer[boxdapi.ExecChunk, boxdapi.ExecChunk]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	f.mu.Lock()
	v, err := f.vm(first.GetVmId())
	var running bool
	if err == nil {
		running = v.status == "running"
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if !running {
		return status.Errorf(codes.FailedPrecondition, "%s is not running", v.name)
	}
	// A command's stdout and stderr are copied from goroutines of their own,
	// and a gRPC stream takes one Send at a time.
	var sends sync.Mutex
	stdout := execWriter{stream: stream, mu: &sends}
	stderr := execWriter{stream: stream, mu: &sends, stderr: true}
	exit := func(code int32, msg string) error {
		if msg != "" {
			if _, err := stderr.Write([]byte(msg)); err != nil {
				return err
			}
		}
		sends.Lock()
		defer sends.Unlock()
		return stream.Send(&boxdapi.ExecChunk{ExitCode: code})
	}

	command := first.GetCommand()
	req, err := parseCommand(command)
	if err != nil {
		return exit(127, fmt.Sprintf("fake boxd: %v", err))
	}
	stdin, feed := io.Pipe()
	defer stdin.Close()
	go func() {
		for {
			chunk, err := stream.Recv()
			if err != nil {
				// The client half-closed (EOF) or went away.
				_ = feed.Close()
				return
			}
			if chunk.GetStdin() {
				if _, err := feed.Write(chunk.GetData()); err != nil {
					return
				}
			}
		}
	}()

	root := f.root(v)
	arg := func() string { return guest.Resolve(root, req.Argv[len(req.Argv)-1]) }
	switch {
	case slices.Equal(req.Argv, readyCommand.Argv):
		_, _ = stdout.Write([]byte("running\n"))
		return exit(0, "")
	case slices.Equal(req.Argv, []string{"sync"}):
		// The host's disks are not the guest's to flush.
		if f.syncFails.Load() {
			return exit(1, "sync: I/O error")
		}
		return exit(0, "")
	case isScript(req, infoScript):
		arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
		_, _ = stdout.Write([]byte(arch + "\n" + v.name + "\n"))
		return exit(0, "")
	case isScript(req, copyInScript):
		if err := guest.Unpack(stdin, arg()); err != nil {
			return exit(2, err.Error())
		}
		return exit(0, "")
	case isScript(req, copyOutScript):
		path := arg()
		if _, err := os.Lstat(path); err != nil {
			return exit(1, err.Error())
		}
		_, _ = stdout.Write([]byte{copyOutReady})
		if err := guest.Pack(stdout, path, false); err != nil {
			return exit(2, err.Error())
		}
		return exit(0, "")
	}
	cmd := exec.CommandContext(stream.Context(), req.Argv[0], req.Argv[1:]...) //nolint:gosec // G204: the test's own commands
	cmd.Dir = guest.Resolve(root, req.Dir)
	cmd.Env = append(append(os.Environ(), guest.RootEnv+"="+root), req.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err = cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit(int32(exitErr.ExitCode()), "")
	}
	if err != nil {
		return exit(127, err.Error())
	}
	return exit(0, "")
}

// isScript reports whether req runs the driver's script text.
func isScript(req machine.ExecRequest, text string) bool {
	return len(req.Argv) >= 4 && req.Argv[0] == "sh" && req.Argv[1] == "-c" && req.Argv[2] == text
}

// parseCommand reads back a line command made, which is all the driver sends
// through sudo.
func parseCommand(line string) (machine.ExecRequest, error) {
	words, err := shellWords(line)
	if err != nil {
		return machine.ExecRequest{}, err
	}
	if len(words) < 11 || !slices.Equal(words[:4], []string{"sudo", "-n", "-H", "-u"}) || !slices.Equal(words[5:8], []string{"--", "env", "-C"}) || words[9] != "--" {
		return machine.ExecRequest{}, fmt.Errorf("unknown command %q", line)
	}
	req := machine.ExecRequest{User: words[4], Dir: words[8]}
	rest := words[10:]
	// As env reads them: a word with "=" is a variable, until the first
	// that is not.
	for len(rest) > 0 && strings.Contains(rest[0], "=") {
		req.Env, rest = append(req.Env, rest[0]), rest[1:]
	}
	if len(rest) == 0 {
		return machine.ExecRequest{}, fmt.Errorf("no argv in %q", line)
	}
	req.Argv = rest
	return req, nil
}

// shellWords splits a line the way sh does for words that are single-quoted
// or backslash-escaped, which is how quote writes them.
func shellWords(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord := false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == ' ':
			if inWord {
				words, inWord = append(words, word.String()), false
				word.Reset()
			}
		case c == '\'':
			end := strings.IndexByte(line[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated quote in %q", line)
			}
			word.WriteString(line[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case c == '\\' && i+1 < len(line):
			word.WriteByte(line[i+1])
			i++
			inWord = true
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, nil
}

type execWriter struct {
	stream grpc.BidiStreamingServer[boxdapi.ExecChunk, boxdapi.ExecChunk]
	mu     *sync.Mutex
	stderr bool
}

func (w execWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.stream.Send(&boxdapi.ExecChunk{Data: p, IsStderr: w.stderr}); err != nil {
		return 0, err
	}
	return len(p), nil
}
