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
	// snapshotIDRefused makes GetVm of a snapshot's ID fail with
	// InvalidArgument, not NotFound: what boxd answers is not proven.
	snapshotIDRefused atomic.Bool
	// snapshotGets counts GetSnapshot's answers that found a snapshot.
	snapshotGets atomic.Int32
	// snapshotOnDestroy makes DestroyVm find a ready snapshot of the
	// machine's name, as a Commit's taken just before the destroy is.
	snapshotOnDestroy atomic.Bool
	// proxyErr, when set, is SetProxyPort's answer.
	proxyErr error
	// botProtectionErr, when set, is SetBotProtection's answer.
	botProtectionErr error
	// deleteProxyErr, when set, is DeleteProxy's answer for a named proxy.
	deleteProxyErr error
	// proxyKept makes DeleteProxy answer and keep a named proxy.
	proxyKept atomic.Bool
	// portIgnored makes SetProxyPort answer and leave the port as it was.
	portIgnored atomic.Bool
	// namedProxy, when set, is a named proxy every new machine has, as one
	// made in the guest by boxd's in-VM CLI would be.
	namedProxy string
	// syncFails makes the guest's sync exit 1.
	syncFails atomic.Bool
	// readyHangs makes `systemctl is-system-running --wait` hang until its
	// call ends, as one sent early in a boot can.
	readyHangs atomic.Bool
	// unreachable is how many Execs to come answer Unavailable before the
	// guest answers anything: gRPC's code for a connection it cannot make,
	// which boxd itself was not seen to give (see rpc Exec in the proto).
	unreachable atomic.Int32
	// egressIgnored makes SetVmEgressAllow answer and keep nothing, so
	// GetVm reports the machine unrestricted.
	egressIgnored atomic.Bool
	// unseen makes the next lookups of a machine by ID or name answer
	// NotFound, as boxd's SetAutoSuspendTimeout did for a machine CreateVm
	// had just made (region fra, 2026-11-15). ListVms still lists it.
	unseen atomic.Int32
	// destroyErr, when set, is DestroyVm's answer, the machine kept.
	destroyErr error
	// destroyNotFound makes DestroyVm remove the machine and then answer
	// NotFound, so a caller learns it is gone only from ListVms.
	destroyNotFound atomic.Bool

	keys keys
	// addr and tokenURL are where the API and its token exchange are
	// served.
	addr, tokenURL string

	// region is the region this API serves, and regions every region boxd
	// has, as ListRegions answers them on the home endpoint.
	region  string
	regions []*boxdapi.Region
	// account, on a region's API, is the home endpoint's: it holds the
	// keys and answers for the regions, and a key call made here is refused.
	account *fakeAPI

	// The org's, on the home endpoint (org): its name, its secrets, and its
	// machine limit, which counts the machines of every region, stopped
	// ones included, as boxd's does. Every credential the fake takes is
	// fenced to the org, as boxd's API keys and machine tokens are.
	orgName  string
	secrets  []*boxdapi.VarItem
	maxVMs   atomic.Int64
	machines atomic.Int64
	// limitRefused makes GetOrgMachineLimit refuse the caller, as boxd may
	// a token minted inside a machine, which it does not document reading.
	limitRefused atomic.Bool
}

// org is the home endpoint's API, which holds the org.
func (f *fakeAPI) org() *fakeAPI {
	if f.account != nil {
		return f.account
	}
	return f
}

// admit counts a machine about to be made against the org's limit, or
// refuses it as boxd does. boxd does not document the code it refuses with,
// so the fake's is one the driver cannot mistake for anything in particular.
func (f *fakeAPI) admit() error {
	org := f.org()
	for {
		n := org.machines.Load()
		if limit := org.maxVMs.Load(); limit > 0 && n >= limit {
			return status.Errorf(codes.FailedPrecondition, "org %s has reached its machine limit (%d)", org.orgName, limit)
		}
		if org.machines.CompareAndSwap(n, n+1) {
			return nil
		}
	}
}

// GetOrgMachineLimit is the org's limit and how many machines count
// against it.
func (f *fakeAPI) GetOrgMachineLimit(context.Context, *boxdapi.GetOrgMachineLimitRequest) (*boxdapi.GetOrgMachineLimitResponse, error) {
	if err := f.accountCall(); err != nil {
		return nil, err
	}
	if f.limitRefused.Load() {
		return nil, status.Error(codes.PermissionDenied, "a token minted inside a machine cannot read the org's machine limit")
	}
	return &boxdapi.GetOrgMachineLimitResponse{
		OrgId: "org_fake", OrgName: f.orgName,
		MaxVms: uint32(f.maxVMs.Load()), VmCount: uint32(f.machines.Load()),
	}, nil
}

// ListVars lists the org's secrets as boxd does: names, scopes, and hosts,
// never a value. A fenced credential's empty org resolves to its own, and
// any other is refused (boxd's credential fence).
func (f *fakeAPI) ListVars(_ context.Context, req *boxdapi.ListVarsRequest) (*boxdapi.VarListReply, error) {
	if err := f.accountCall(); err != nil {
		return nil, err
	}
	if req.GetOrg() != "" && req.GetOrg() != f.orgName {
		return nil, status.Errorf(codes.PermissionDenied, "this credential is fenced to org %s, not %q", f.orgName, req.GetOrg())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &boxdapi.VarListReply{}
	for _, v := range f.secrets {
		if req.GetKind() == "" || req.GetKind() == v.GetKind() {
			out.Vars = append(out.Vars, v)
		}
	}
	return out, nil
}

// setSecret adds a secret to the org, in a scope.
func (f *fakeAPI) setSecret(name, scope string, domains ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets = append(f.secrets, &boxdapi.VarItem{Name: name, Kind: "secret", Scope: scope, Domains: domains})
}

// ListRegions is the home endpoint's list, with the region this one serves.
func (f *fakeAPI) ListRegions(context.Context, *boxdapi.ListRegionsRequest) (*boxdapi.ListRegionsResponse, error) {
	home := f
	if f.account != nil {
		home = f.account
	}
	home.mu.Lock()
	defer home.mu.Unlock()
	return &boxdapi.ListRegionsResponse{Regions: home.regions, ThisRegion: f.region}, nil
}

// accountCall refuses an account call made to a region's endpoint, which
// the driver must make at the home endpoint.
func (f *fakeAPI) accountCall() error {
	if f.account != nil {
		return status.Errorf(codes.Unimplemented, "the %s endpoint answers no account calls: call boxd.sh:9443", f.region)
	}
	return nil
}

type fakeVM struct {
	id, name string
	// imageRef is the image CreateVm made it from.
	imageRef string
	dir      string
	status   string
	vcpu     uint32
	memory   uint64
	// reboots counts RebootVm calls.
	reboots int
	// proxyPort is the port the default proxy is locked to, when
	// proxyLocked, which port 0 can be; botProtection is on for a new
	// machine, as on boxd.
	proxyPort     uint32
	proxyLocked   bool
	botProtection bool
	// named are the machine's proxies other than its default one.
	named []string
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
	// status is boxd's, "ready" when empty.
	status string
}

// state is the snapshot's status as boxd answers it.
func (s *fakeSnapshot) state() string {
	if s.status == "" {
		return "ready"
	}
	return s.status
}

func newFakeAPI(dir string) *fakeAPI {
	f := &fakeAPI{
		dir: dir, vms: map[string]*fakeVM{}, snapshots: map[string]*fakeSnapshot{}, defaultImage: "ubuntu-24.04",
		keys:    keys{byID: map[string]*fakeKey{}, jwts: map[string]string{}},
		orgName: "fake-org",
	}
	// boxd's default limit for an org.
	f.maxVMs.Store(50)
	return f
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
	if f.unseen.Add(-1) >= 0 {
		return nil, status.Errorf(codes.NotFound, "vm not found: %s", id)
	}
	f.unseen.Store(0)
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
	if f.namedProxy != "" {
		v.named = []string{f.namedProxy}
	}
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
	if err := f.accountCall(); err != nil {
		return nil, err
	}
	return &boxdapi.WhoamiResponse{UserId: "usr_fake"}, nil
}

func (f *fakeAPI) CreateVm(_ context.Context, req *boxdapi.CreateVmRequest) (*boxdapi.CreateVmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.taken(req.GetName()) {
		return nil, status.Errorf(codes.AlreadyExists, "a machine is named %q", req.GetName())
	}
	if err := f.admit(); err != nil {
		return nil, err
	}
	v := f.newVM(req.GetName(), req.GetConfig().GetVcpu(), req.GetConfig().GetMemoryBytes())
	v.isolated, v.networks = req.GetIsolated(), req.GetNetworks()
	v.imageRef = req.GetImageRef()
	if v.imageRef == "" {
		v.imageRef = f.defaultImage
	}
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
	if err := f.admit(); err != nil {
		return nil, err
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
	if err != nil && f.snapshotIDRefused.Load() {
		for _, snap := range f.snapshots {
			if snap.id == req.GetVmId() {
				return nil, status.Errorf(codes.InvalidArgument, "%q is a snapshot", snap.id)
			}
		}
	}
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
	if f.destroyErr != nil {
		return nil, f.destroyErr
	}
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	f.halt(v)
	// boxd finishes a pending snapshot of a machine it destroys: one taken
	// of a machine then destroyed was ready a moment later (2026-11-15).
	if snap := f.snapshots[v.name]; snap != nil && snap.state() == "pending" {
		snap.status = ""
	}
	if f.snapshotOnDestroy.Load() {
		f.snapshots[v.name] = &fakeSnapshot{id: "snap_" + fsutil.RandomHex(4), name: v.name, dir: filepath.Join(f.dir, "snap_"+v.id)}
	}
	delete(f.vms, v.id)
	f.org().machines.Add(-1)
	if err := os.RemoveAll(v.dir); err != nil || !f.destroyNotFound.Load() {
		return &boxdapi.DestroyVmResponse{}, err
	}
	return nil, status.Errorf(codes.NotFound, "vm not found: %s", v.id)
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
		// boxd's API says `vm` filters the listing, not what it answers for
		// an ID that is no machine: the fake lists nothing, the answer a
		// caller can least tell from a machine with no proxies.
		return &boxdapi.ListProxiesResponse{}, nil
	}
	// boxd lists a machine's default proxy with an empty name, and always
	// lists one.
	p := &boxdapi.ProxyInfo{VmName: v.name, VmId: v.id, Domain: v.name + ".boxd.test", IsDefault: true, PortMode: "auto", EffectivePort: 8000}
	if v.proxyLocked {
		p.PortMode, p.EffectivePort = "locked", v.proxyPort
	}
	list := []*boxdapi.ProxyInfo{p}
	for _, n := range v.named {
		list = append(list, &boxdapi.ProxyInfo{Name: n, VmName: v.name, VmId: v.id, Domain: n + ".boxd.test", PortMode: "auto", EffectivePort: 8000})
	}
	return &boxdapi.ListProxiesResponse{Proxies: list}, nil
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
	// An empty name is the default proxy, which the fake alone sets a
	// port on.
	if req.GetName() != "" {
		return nil, status.Errorf(codes.NotFound, "%s has no proxy %q", v.name, req.GetName())
	}
	// boxd locks a proxy to port 0, and takes no port past 65535.
	port, err := strconv.ParseUint(req.GetPort(), 10, 16)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid port: %s", req.GetPort())
	}
	if !f.portIgnored.Load() {
		v.proxyPort, v.proxyLocked = uint32(port), true
	}
	return &boxdapi.SetProxyPortResponse{}, nil
}

func (f *fakeAPI) DeleteProxy(_ context.Context, req *boxdapi.DeleteProxyRequest) (*boxdapi.DeleteProxyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVm())
	if err != nil {
		return nil, err
	}
	// As boxd answers: the default proxy, "@", is never removed, and its
	// empty name is no proxy.
	if req.GetName() == "@" {
		return nil, status.Error(codes.FailedPrecondition, "cannot delete the default proxy")
	}
	i := slices.Index(v.named, req.GetName())
	if i < 0 {
		return nil, status.Errorf(codes.NotFound, "proxy '%s' not found for VM '%s'", req.GetName(), v.name)
	}
	if f.deleteProxyErr != nil {
		return nil, f.deleteProxyErr
	}
	if !f.proxyKept.Load() {
		v.named = slices.Delete(v.named, i, i+1)
	}
	return &boxdapi.DeleteProxyResponse{}, nil
}

func (f *fakeAPI) SetBotProtection(_ context.Context, req *boxdapi.SetBotProtectionRequest) (*boxdapi.SetBotProtectionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	if f.botProtectionErr != nil {
		return nil, f.botProtectionErr
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
	f.snapshotGets.Add(1)
	return &boxdapi.GetSnapshotResponse{Snapshot: &boxdapi.SnapshotInfo{
		SnapshotId: snap.id, Name: snap.name, Version: 1, Status: snap.state(), Vcpu: snap.vcpu, MemoryBytes: snap.memory,
	}}, nil
}

func (f *fakeAPI) ListSnapshots(context.Context, *boxdapi.ListSnapshotsRequest) (*boxdapi.ListSnapshotsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &boxdapi.ListSnapshotsResponse{}
	for _, snap := range f.snapshots {
		out.Snapshots = append(out.Snapshots, &boxdapi.SnapshotInfo{
			SnapshotId: snap.id, Name: snap.name, Version: 1, Status: snap.state(), Vcpu: snap.vcpu, MemoryBytes: snap.memory,
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
	if f.unreachable.Add(-1) >= 0 {
		return status.Errorf(codes.Unavailable, "%s does not answer yet", v.name)
	}
	f.unreachable.Store(0)
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
		if f.readyHangs.Load() {
			<-stream.Context().Done()
			return stream.Context().Err()
		}
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
