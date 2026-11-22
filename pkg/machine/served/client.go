package served

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/vm/internal/netutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Config is what a client of a served driver is made with.
type Config struct {
	// Dial opens a new stream to the served handler, one per call: the
	// channel the embedder serves it on. Required.
	Dial func(ctx context.Context) (net.Conn, error)
	// Version is the client's engine version, sent on every call. Required.
	Version string
	// Prefix is the path the embedder mounts the handler under
	// (http.StripPrefix), with no trailing slash; empty is the root.
	Prefix string
}

// Driver is a machine.Driver whose every call is a served driver's. Its
// name, capabilities, and allow-list are the server's, read when it is made,
// and its capabilities again after each Check, since a driver may learn a
// cap from its provider there (Capabilities.MaxMachines); every VM is read
// from the server on every call.
type Driver struct {
	cfg  Config
	http *http.Client
	info driverInfo
	// caps is info.Capabilities, as last read.
	caps atomic.Pointer[machine.Capabilities]
}

var _ machine.Driver = (*Driver)(nil)

// New connects to a served driver and reads what it serves. A server of
// another version refuses it with a *VersionError, and one that does not
// allow OpCapabilities with ErrRefused.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	if cfg.Dial == nil || cfg.Version == "" {
		return nil, errors.New("served: a client needs a dialer and its version")
	}
	cfg.Prefix = strings.TrimSuffix(cfg.Prefix, "/")
	d := &Driver{cfg: cfg, http: &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return cfg.Dial(ctx) },
		DisableKeepAlives: true,
	}}}
	if err := d.call(ctx, http.MethodGet, "/v1/driver", nil, &d.info); err != nil {
		return nil, err
	}
	d.caps.Store(&d.info.Capabilities)
	return d, nil
}

// Name is the served driver's.
func (d *Driver) Name() string { return d.info.Name }

// Capabilities are the served driver's, narrowed to what the server
// allows.
func (d *Driver) Capabilities() machine.Capabilities { return *d.caps.Load() }

// Check is the served driver's, and then reads its capabilities again.
func (d *Driver) Check(ctx context.Context) error {
	if err := d.call(ctx, http.MethodPost, "/v1/check", nil, nil); err != nil {
		return err
	}
	var info driverInfo
	if err := d.call(ctx, http.MethodGet, "/v1/driver", nil, &info); err != nil {
		return err
	}
	d.caps.Store(&info.Capabilities)
	return nil
}

// Install is never served.
func (d *Driver) Install(context.Context, machine.InstallSpec, string) (machine.Instance, error) {
	return machine.Instance{}, fmt.Errorf("served: install is never served: %w", machine.ErrUnsupported)
}

// Create makes a VM on the server, which finds its From by name in its
// pool. A cache volume is refused: caches are not served.
func (d *Driver) Create(ctx context.Context, spec machine.InstanceSpec) (machine.Instance, error) {
	if spec.Cache != nil {
		return machine.Instance{}, fmt.Errorf("served: caches are never served: %w", machine.ErrUnsupported)
	}
	reach := d.Capabilities().URLs
	if err := spec.URLs.Check(reach); err != nil {
		return machine.Instance{}, fmt.Errorf("served: %w", err)
	}
	req := createRequest{
		Name: spec.Name, CPUs: spec.CPUs, Memory: spec.Memory, Forwards: spec.Forwards,
		Networks: spec.Networks, Egress: spec.Egress, Data: spec.Data,
		// A driver whose every machine is bare needs no request of it.
		Bare: spec.Bare && d.Capabilities().Bare != machine.BareAlways,
	}
	if asked := asked(spec.URLs, reach); len(asked) > 0 {
		switch {
		case slices.Contains(d.info.Fields, FieldURLs):
			req.URLs = &spec.URLs
		case strict(spec.URLs, asked):
			// The embedder lets no create ask, so the driver's default is
			// what the instance gets, and a strict request of it is one
			// the client cannot meet, as a narrowed capability is.
			return machine.Instance{}, fmt.Errorf("served: the embedder lets no create ask for a %s URL: %w", joinScopes(asked), machine.ErrUnsupported)
		}
		// Not strict: the driver's default, which the caller takes.
	}
	if spec.From != nil {
		req.From, req.FromPool = spec.From.Name, spec.From.Pool
	}
	var inst machine.Instance
	return inst, d.call(ctx, http.MethodPost, "/v1/vms", req, &inst)
}

// Instance is the pool's VM of a name.
func (d *Driver) Instance(ctx context.Context, name string) (machine.Instance, error) {
	var inst machine.Instance
	return inst, d.call(ctx, http.MethodGet, vmPath(name, ""), nil, &inst)
}

// Instances lists the pool's VMs.
func (d *Driver) Instances(ctx context.Context) ([]machine.Instance, error) {
	var all []machine.Instance
	return all, d.call(ctx, http.MethodGet, "/v1/vms", nil, &all)
}

// Destroy removes a VM; one already gone is not an error.
func (d *Driver) Destroy(ctx context.Context, inst machine.Instance) error {
	err := d.call(ctx, http.MethodDelete, vmPath(inst.Name, ""), nil, nil)
	if errors.Is(err, machine.ErrNotFound) {
		return nil
	}
	return err
}

// Boot is Start: the machine is the server's, and outlives this process,
// as a boxd machine does. A paused VM is ErrPaused: Start resumes it. A
// state the server could not read is that error, never a Start, which
// would resume a paused VM a boot must leave paused.
func (d *Driver) Boot(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	state, err := d.State(ctx, inst)
	if err != nil {
		return nil, err
	}
	if state == machine.StatePaused {
		return nil, fmt.Errorf("instance %s: %w", inst.Name, machine.ErrPaused)
	}
	return d.Start(ctx, inst, opts)
}

// Start starts the VM on the server, and returns it attached. A window is
// never served.
func (d *Driver) Start(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	if opts.GUI {
		return nil, fmt.Errorf("served: a window is never served: %w", machine.ErrUnsupported)
	}
	if err := d.call(ctx, http.MethodPost, vmPath(inst.Name, "/start"), nil, nil); err != nil {
		return nil, err
	}
	return d.machine(inst.Name), nil
}

// State is the server's Driver.State. One this client does not know is an
// error, not a guess.
func (d *Driver) State(ctx context.Context, inst machine.Instance) (machine.State, error) {
	var resp stateResponse
	if err := d.call(ctx, http.MethodGet, vmPath(inst.Name, "/state"), nil, &resp); err != nil {
		return "", err
	}
	switch resp.State {
	case machine.StateStopped, machine.StateRunning, machine.StatePaused:
		return resp.State, nil
	default:
		return "", fmt.Errorf("served: instance %s is in a state this client does not know: %q", inst.Name, resp.State)
	}
}

// Attach returns the running VM's machine, ErrNotRunning, or ErrPaused.
func (d *Driver) Attach(ctx context.Context, inst machine.Instance) (machine.Machine, error) {
	state, err := d.State(ctx, inst)
	switch {
	case err != nil:
		return nil, err
	case state == machine.StatePaused:
		return nil, fmt.Errorf("instance %s: %w", inst.Name, machine.ErrPaused)
	case state == machine.StateStopped:
		return nil, fmt.Errorf("instance %s: %w", inst.Name, machine.ErrNotRunning)
	}
	return d.machine(inst.Name), nil
}

// running is whether the VM has not stopped: a paused one is held, and has
// not.
func (d *Driver) running(ctx context.Context, inst machine.Instance) (bool, error) {
	state, err := d.State(ctx, inst)
	return state != machine.StateStopped, err
}

// Commit commits the stopped VM on the server.
func (d *Driver) Commit(ctx context.Context, inst machine.Instance) (machine.Instance, error) {
	var committed machine.Instance
	return committed, d.call(ctx, http.MethodPost, vmPath(inst.Name, "/commit"), nil, &committed)
}

// Upgrade gives the stopped VM a new root on the server, from the committed
// VM from names, which the server finds in its pool.
func (d *Driver) Upgrade(ctx context.Context, inst machine.Instance, from *machine.Instance) (machine.Instance, error) {
	var req upgradeRequest
	if from != nil {
		req.From, req.FromPool = from.Name, from.Pool
	}
	var upgraded machine.Instance
	return upgraded, d.call(ctx, http.MethodPost, vmPath(inst.Name, "/upgrade"), req, &upgraded)
}

// Listen is the served driver's, which OpStart allows, since a start
// writes it to the guest.
func (d *Driver) Listen(ctx context.Context, inst machine.Instance) (string, error) {
	var resp listenResponse
	return resp.Listen, d.call(ctx, http.MethodGet, vmPath(inst.Name, "/listen"), nil, &resp)
}

// Service is the served driver's, unnarrowed (ADR 26-11-14-610), with the
// credential it minted, if any (ADR 26-11-17-311 §6): its URLs are where the
// server's host and the instance's peers reach the service, and a client of
// another host reaches it by Dial. An embedder that must withhold an
// ingress URL forces it off in Options.Create.
func (d *Driver) Service(ctx context.Context, inst machine.Instance) (machine.Service, error) {
	var service machine.Service
	return service, d.call(ctx, http.MethodGet, vmPath(inst.Name, "/service"), nil, &service)
}

// Caches lists none: caches are never served.
func (d *Driver) Caches(context.Context) ([]machine.Cache, error) { return nil, nil }

// SaveCache is never served.
func (d *Driver) SaveCache(context.Context, machine.Instance, machine.CacheSave) (machine.Cache, error) {
	return machine.Cache{}, fmt.Errorf("served: caches are never served: %w", machine.ErrUnsupported)
}

// DeleteCache is never served.
func (d *Driver) DeleteCache(context.Context, string) error {
	return fmt.Errorf("served: caches are never served: %w", machine.ErrUnsupported)
}

// IssueKey is never served: the channel is the client's credential.
func (d *Driver) IssueKey(context.Context, machine.IssueRequest) (machine.Key, error) {
	return machine.Key{}, fmt.Errorf("served: %w", machine.ErrNoKey)
}

// IssuedKeys is never served.
func (d *Driver) IssuedKeys(context.Context, string) ([]machine.Key, error) {
	return nil, fmt.Errorf("served: %w", machine.ErrNoKey)
}

// RevokeKey is never served.
func (d *Driver) RevokeKey(context.Context, machine.Key) error {
	return fmt.Errorf("served: %w", machine.ErrNoKey)
}

func vmPath(name, rest string) string { return "/v1/vms/" + url.PathEscape(name) + rest }

// call makes one request, with body as JSON where it is not nil, and decodes
// the answer into out where it is not nil.
func (d *Driver) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://served"+d.cfg.Prefix+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set(VersionHeader, d.cfg.Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("served: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return readError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// readError is the server's error, wrapping the sentinel its code names.
func readError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e errorResponse
	if json.Unmarshal(raw, &e) != nil || e.Error == "" {
		return fmt.Errorf("served: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if e.Version != nil {
		return e.Version
	}
	return &remoteError{msg: e.Error, is: sentinelOf(e.Code)}
}

// upgrade asks the server to upgrade a connection to one of its streams,
// and returns the connection once it has.
func (d *Driver) upgrade(ctx context.Context, path, token string) (net.Conn, error) {
	conn, err := d.cfg.Dial(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://served"+d.cfg.Prefix+path, nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	req.Header.Set(VersionHeader, d.cfg.Version)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", token)
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req) //nolint:bodyclose // on 101 the body is the upgraded conn, returned as netutil.BufConn; otherwise conn is closed
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		err := readError(resp)
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return &netutil.BufConn{Conn: conn, R: reader}, nil
}

// machine is the server's running VM of a name, attached.
func (d *Driver) machine(name string) *remoteMachine {
	m := &remoteMachine{d: d, name: name, done: make(chan struct{})}
	m.Client = guest.NewClient(func(ctx context.Context) (net.Conn, error) {
		return d.upgrade(ctx, vmPath(name, "/guest"), guestUpgrade)
	})
	return m
}

// remoteMachine is a running VM the server holds. Its guest operations are
// the server's relay of the machine's; it watches for its stop only once
// Done or Err is called, by asking the server whether it still runs.
type remoteMachine struct {
	*guest.Client
	d        *Driver
	name     string
	done     chan struct{}
	err      error
	watching sync.Once
}

// allow refuses an operation the server does not allow before it is
// asked, so the refusal is ErrRefused here too. The server refuses it
// whatever the client does.
func (m *remoteMachine) allow(op Op) error {
	if !slices.Contains(m.d.info.Allow, op) {
		return refused(op)
	}
	return nil
}

func (m *remoteMachine) Exec(ctx context.Context, req machine.ExecRequest) (machine.Process, error) {
	if err := m.allow(OpExec); err != nil {
		return nil, err
	}
	return m.Client.Exec(ctx, req)
}

func (m *remoteMachine) CopyTo(ctx context.Context, dir string, tarStream io.Reader) error {
	if err := m.allow(OpCopy); err != nil {
		return err
	}
	return m.Client.CopyTo(ctx, dir, tarStream)
}

func (m *remoteMachine) CopyFrom(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := m.allow(OpCopy); err != nil {
		return nil, err
	}
	return m.Client.CopyFrom(ctx, path)
}

func (m *remoteMachine) Info(ctx context.Context) (machine.GuestInfo, error) {
	if err := m.allow(OpInfo); err != nil {
		return machine.GuestInfo{}, err
	}
	return m.Client.Info(ctx)
}

func (m *remoteMachine) Shutdown(ctx context.Context, reboot bool) error {
	if err := m.allow(OpStop); err != nil {
		return err
	}
	return m.Client.Shutdown(ctx, reboot)
}

// Kill forces the VM off; one already stopped is not an error.
func (m *remoteMachine) Kill(ctx context.Context) error {
	err := m.d.call(ctx, http.MethodPost, vmPath(m.name, "/kill"), nil, nil)
	if errors.Is(err, machine.ErrNotRunning) || errors.Is(err, machine.ErrNotFound) {
		return nil
	}
	return err
}

// Pause pauses the VM on the server; one already paused is not an error.
// A driver the server does not report Capabilities.Pause for refuses it.
func (m *remoteMachine) Pause(ctx context.Context) error {
	if !m.d.Capabilities().Pause {
		return fmt.Errorf("the served driver cannot pause a VM: %w", machine.ErrUnsupported)
	}
	return m.d.call(ctx, http.MethodPost, vmPath(m.name, "/pause"), nil, nil)
}

// Dial opens a stream to the agent's port or the service's, through the
// server.
func (m *remoteMachine) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	if !guest.DialablePort(port) {
		return nil, fmt.Errorf("guest port %d: a caller reaches the agent's port (%d) and the service's (%d) alone: %w", port, guest.AgentPort, guest.ServicePort, machine.ErrUnsupported)
	}
	return m.d.upgrade(ctx, vmPath(m.name, "/dial?port="+strconv.FormatUint(uint64(port), 10)), dialUpgrade)
}

// giveUp is how long the server may fail to answer before a machine is
// given up, as a remote driver whose API stops answering does.
const giveUp = 30 * time.Second

// Done is closed once the server finds the VM stopped, or has not answered
// for giveUp.
func (m *remoteMachine) Done() <-chan struct{} {
	m.watching.Do(func() {
		go func() {
			var failing time.Time
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				running, err := m.d.running(ctx, machine.Instance{Name: m.name})
				cancel()
				switch {
				case errors.Is(err, machine.ErrNotFound):
					close(m.done)
					return
				case err != nil:
					if failing.IsZero() {
						failing = time.Now()
					} else if time.Since(failing) > giveUp {
						m.err = fmt.Errorf("served: instance %s: the server stopped answering: %w", m.name, err)
						close(m.done)
						return
					}
				case !running:
					close(m.done)
					return
				default:
					failing = time.Time{}
				}
				time.Sleep(250 * time.Millisecond)
			}
		}()
	})
	return m.done
}

// Err is nil for a VM found stopped, and why it was given up otherwise.
func (m *remoteMachine) Err() error {
	<-m.Done()
	return m.err
}

// asked is each scope whose request the server's driver acts on: a mode of
// a URL it gives or withholds as asked. Any other is met already, or refused
// by URLRequests.Check, and is not sent.
func asked(urls machine.URLRequests, reach machine.URLCapabilities) []machine.URLScope {
	var scopes []machine.URLScope
	for _, scope := range machine.URLScopes {
		if urls.Of(scope).Mode != machine.URLDefault && reach.Of(scope) == machine.URLOptional {
			scopes = append(scopes, scope)
		}
	}
	return scopes
}

// strict reports whether any of the scopes' requests is strict.
func strict(urls machine.URLRequests, scopes []machine.URLScope) bool {
	return slices.ContainsFunc(scopes, func(scope machine.URLScope) bool { return urls.Of(scope).Strict })
}

// joinScopes names scopes for an error, "ingress or pool".
func joinScopes(scopes []machine.URLScope) string {
	names := make([]string, len(scopes))
	for i, scope := range scopes {
		names[i] = string(scope)
	}
	return strings.Join(names, " or ")
}
