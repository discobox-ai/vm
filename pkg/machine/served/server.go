package served

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/discobox-ai/vm/internal/netutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Options is what an embedder allows the client of a served driver.
type Options struct {
	// Version is the server's engine version. A request that carries
	// another is refused with a *VersionError. Required.
	Version string
	// Allow is every operation the client may call; any other is refused.
	Allow []Op
	// Fields is every field a create may set past its name; a create that
	// sets another is refused.
	Fields []Field
	// Create, when set, is called with every create's spec, its From found
	// in the driver's pool, before the driver is: it may change the spec
	// (force a fence, a size, a forward of its own) or refuse it, and its
	// error is the create's. The client's fields are checked before it.
	Create func(ctx context.Context, spec *machine.InstanceSpec) error
}

// Handler serves driver to one client under opts. The driver is made for
// the pool the client is confined to. The handler authenticates no one: the
// embedder serves it only on a channel it has authenticated, to the caller
// it means. It needs HTTP/1.1 connections, since a guest operation and a
// dial upgrade theirs.
func Handler(driver machine.Driver, opts Options) (http.Handler, error) {
	if opts.Version == "" {
		return nil, errors.New("served: a server needs its version")
	}
	for _, op := range opts.Allow {
		if !slices.Contains(Ops, op) {
			return nil, fmt.Errorf("served: no operation %q", op)
		}
	}
	for _, f := range opts.Fields {
		if !slices.Contains(Fields, f) {
			return nil, fmt.Errorf("served: no create field %q", f)
		}
	}
	s := &server{driver: driver, opts: opts}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/driver", s.op(OpCapabilities, s.info))
	mux.HandleFunc("POST /v1/check", s.op(OpCapabilities, s.check))
	mux.HandleFunc("GET /v1/vms", s.op(OpList, s.list))
	mux.HandleFunc("POST /v1/vms", s.op(OpCreate, s.create))
	mux.HandleFunc("GET /v1/vms/{name}", s.vm(OpGet, s.get))
	mux.HandleFunc("DELETE /v1/vms/{name}", s.vm(OpRemove, s.remove))
	mux.HandleFunc("POST /v1/vms/{name}/start", s.vm(OpStart, s.start))
	mux.HandleFunc("GET /v1/vms/{name}/state", s.vm(OpState, s.state))
	mux.HandleFunc("POST /v1/vms/{name}/kill", s.vm(OpStop, s.kill))
	mux.HandleFunc("POST /v1/vms/{name}/commit", s.vm(OpCommit, s.commit))
	mux.HandleFunc("POST /v1/vms/{name}/upgrade", s.vm(OpUpgrade, s.upgrade))
	mux.HandleFunc("GET /v1/vms/{name}/service", s.vm(OpService, s.service))
	mux.HandleFunc("GET /v1/vms/{name}/guest", s.guestOps)
	mux.HandleFunc("GET /v1/vms/{name}/dial", s.vm(OpDial, s.dial))
	return s.versioned(mux), nil
}

type server struct {
	driver machine.Driver
	opts   Options
}

func (s *server) allowed(op Op) bool { return slices.Contains(s.opts.Allow, op) }

func (s *server) field(f Field) bool { return slices.Contains(s.opts.Fields, f) }

// versioned refuses a request whose version is not the server's, before
// anything else of it is read.
func (s *server) versioned(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get(VersionHeader); v != s.opts.Version {
			writeError(w, &VersionError{Server: s.opts.Version, Client: v})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// op refuses a call the allow-list does not list.
func (s *server) op(op Op, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.allowed(op) {
			writeError(w, refused(op))
			return
		}
		h(w, r)
	}
}

func refused(op Op) error { return fmt.Errorf("%s: %w", op, ErrRefused) }

// vm is op on the pool's VM the path names, found in the driver's pool. A
// client names a VM, never its ID: what the driver is handed it found
// itself.
func (s *server) vm(op Op, h func(http.ResponseWriter, *http.Request, machine.Instance)) http.HandlerFunc {
	return s.op(op, func(w http.ResponseWriter, r *http.Request) {
		inst, err := s.find(r.Context(), r.PathValue("name"))
		if err != nil {
			writeError(w, err)
			return
		}
		h(w, r, inst)
	})
}

// notAnother refuses a From the client found in a pool other than the
// one the server found its name in: another pool's VM is never served
// (Capabilities.AcrossPools is never reported), and the pool's own VM of
// the name is not the one the client meant.
func notAnother(found machine.Instance, pool string) error {
	if pool != "" && pool != found.Pool {
		return fmt.Errorf("committed VM %s of pool %s: another pool's VM is never served: %w", found.Name, pool, machine.ErrUnsupported)
	}
	return nil
}

// find is the pool's VM of a name.
func (s *server) find(ctx context.Context, name string) (machine.Instance, error) {
	if err := machine.ValidateName(name); err != nil {
		return machine.Instance{}, fmt.Errorf("%w: %w", err, machine.ErrNotFound)
	}
	return s.driver.Instance(ctx, name)
}

// capabilities are the driver's, narrowed to what the allow-list lets a
// client use, so an engine on the client refuses the rest before it asks.
func (s *server) capabilities() machine.Capabilities {
	caps := s.driver.Capabilities()
	out := machine.Capabilities{
		GuestOS:    caps.GuestOS,
		MaxRunning: caps.MaxRunning,
		Agent:      caps.Agent,
		Keys:       machine.KeyCapabilities{Mode: machine.KeysNone},
		Ports:      caps.Ports && s.allowed(OpDial),
		Commit:     caps.Commit && s.allowed(OpCommit) && s.field(FieldFrom),
		Isolation:  caps.Isolation && s.field(FieldIsolated) && s.field(FieldNetworks),
	}
	if s.field(FieldEgress) {
		out.Egress = caps.Egress
	}
	if s.field(FieldData) && caps.Attaches(machine.VolumeData) {
		out.Volumes = []machine.Volume{machine.VolumeData}
	}
	return out
}

func (s *server) info(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, driverInfo{
		Name:         s.driver.Name(),
		Capabilities: s.capabilities(),
		Allow:        s.opts.Allow,
		Fields:       s.opts.Fields,
	})
}

func (s *server) check(w http.ResponseWriter, r *http.Request) {
	if err := s.driver.Check(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	all, err := s.driver.Instances(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, all)
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, fmt.Errorf("create: %w: %w", err, ErrRefused))
		return
	}
	for _, f := range req.set() {
		if !s.field(f) {
			writeError(w, fmt.Errorf("create: field %s: %w", f, ErrRefused))
			return
		}
	}
	if err := machine.ValidateName(req.Name); err != nil {
		writeError(w, err)
		return
	}
	// A host socket (ErrRefused) and a forward that names a pool
	// (ErrUnsupported: AcrossPools is never reported) are never served:
	// only the embedder's hook may set either. The engine refuses
	// the rest of a malformed forward on the client, which is the caller's,
	// and the driver refuses it again here.
	for _, f := range req.Forwards {
		switch {
		case !f.Peer():
			writeError(w, fmt.Errorf("create: port %d: a forward to a host socket: %w", f.Port, ErrRefused))
			return
		case f.Pool != "":
			writeError(w, fmt.Errorf("create: port %d: a forward that names a pool: another pool's instance is never served: %w", f.Port, machine.ErrUnsupported))
			return
		}
	}
	spec := machine.InstanceSpec{
		Name: req.Name, CPUs: req.CPUs, Memory: req.Memory, Forwards: req.Forwards,
		Isolated: req.Isolated, Networks: req.Networks, Egress: req.Egress, Data: req.Data,
	}
	if req.From != "" {
		from, err := s.find(r.Context(), req.From)
		if err == nil {
			err = notAnother(from, req.FromPool)
		}
		if err != nil {
			writeError(w, err)
			return
		}
		spec.From = &from
	} else if req.FromPool != "" {
		writeError(w, fmt.Errorf("create: a pool with no From: %w", ErrRefused))
		return
	}
	if s.opts.Create != nil {
		if err := s.opts.Create(r.Context(), &spec); err != nil {
			writeError(w, err)
			return
		}
	}
	inst, err := s.driver.Create(r.Context(), spec)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, inst)
}

func (s *server) get(w http.ResponseWriter, _ *http.Request, inst machine.Instance) {
	writeJSON(w, http.StatusOK, inst)
}

func (s *server) remove(w http.ResponseWriter, r *http.Request, inst machine.Instance) {
	if err := s.driver.Destroy(r.Context(), inst); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// start starts the VM under its driver, as the server's: the machine
// outlives the call and the client, and the client attaches to it.
func (s *server) start(w http.ResponseWriter, r *http.Request, inst machine.Instance) {
	if _, err := s.driver.Start(r.Context(), inst, machine.BootOptions{}); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) state(w http.ResponseWriter, r *http.Request, inst machine.Instance) {
	_, err := s.driver.Attach(r.Context(), inst)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, stateResponse{Running: true})
	case errors.Is(err, machine.ErrNotRunning):
		writeJSON(w, http.StatusOK, stateResponse{Running: false})
	default:
		writeError(w, err)
	}
}

func (s *server) kill(w http.ResponseWriter, r *http.Request, inst machine.Instance) {
	m, err := s.driver.Attach(r.Context(), inst)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := m.Kill(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) commit(w http.ResponseWriter, r *http.Request, inst machine.Instance) {
	committed, err := s.driver.Commit(r.Context(), inst)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, committed)
}

func (s *server) upgrade(w http.ResponseWriter, r *http.Request, inst machine.Instance) {
	var req upgradeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, fmt.Errorf("upgrade: %w: %w", err, ErrRefused))
		return
	}
	var from *machine.Instance
	if req.From == "" && req.FromPool != "" {
		writeError(w, fmt.Errorf("upgrade: a pool with no From: %w", ErrRefused))
		return
	}
	if req.From != "" {
		if !s.field(FieldFrom) {
			writeError(w, fmt.Errorf("upgrade: field %s: %w", FieldFrom, ErrRefused))
			return
		}
		src, err := s.find(r.Context(), req.From)
		if err == nil {
			err = notAnother(src, req.FromPool)
		}
		switch {
		case errors.Is(err, machine.ErrNotFound) && machine.ValidateName(req.From) == nil:
			// An upgrade caught between its roots finishes without the
			// committed VM it is from, as Engine.Upgrade passes it: by
			// name, for the driver to say whether it still needs it.
			src = machine.Instance{Name: req.From, GuestOS: inst.GuestOS}
		case err != nil:
			writeError(w, err)
			return
		}
		from = &src
	}
	upgraded, err := s.driver.Upgrade(r.Context(), inst, from)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, upgraded)
}

func (s *server) service(w http.ResponseWriter, r *http.Request, inst machine.Instance) {
	service, err := s.driver.Service(r.Context(), inst)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, service)
}

// guestOp is the operation of a request in the agent's protocol, which a
// connection upgraded to a VM's guest operations carries.
func guestOp(r *http.Request) (Op, bool) {
	switch r.URL.Path {
	case "/v1/exec":
		return OpExec, true
	case "/v1/files":
		return OpCopy, true
	case "/v1/info", "/v1/health":
		return OpInfo, true
	case "/v1/shutdown":
		return OpStop, true
	}
	return "", false
}

// guestOps upgrades the connection to the agent's protocol, answered from
// the running VM's guest operations (guest.Relay), each request checked
// against the allow-list.
func (s *server) guestOps(w http.ResponseWriter, r *http.Request) {
	if !slices.ContainsFunc([]Op{OpExec, OpCopy, OpInfo, OpStop}, s.allowed) {
		writeError(w, refused("guest"))
		return
	}
	inst, err := s.find(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	m, err := s.driver.Attach(r.Context(), inst)
	if err != nil {
		writeError(w, err)
		return
	}
	conn, err := upgradeConn(w, guestUpgrade)
	if err != nil {
		return
	}
	relay := guest.Relay(m)
	serveConn(conn, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op, ok := guestOp(r)
		if !ok || !s.allowed(op) {
			if !ok {
				op = Op(r.URL.Path)
			}
			http.Error(w, refused(op).Error(), http.StatusForbidden)
			return
		}
		relay.ServeHTTP(w, r)
	}))
}

// dial upgrades the connection to a stream to the running VM's agent port
// or service port.
func (s *server) dial(w http.ResponseWriter, r *http.Request, inst machine.Instance) {
	port, err := strconv.ParseUint(r.URL.Query().Get("port"), 10, 32)
	if err != nil || port == 0 {
		writeError(w, fmt.Errorf("dial: want ?port=N: %w", ErrRefused))
		return
	}
	if !guest.DialablePort(uint32(port)) {
		writeError(w, fmt.Errorf("dial: guest port %d: a caller reaches the agent's port (%d) and the service's (%d) alone: %w", port, guest.AgentPort, guest.ServicePort, machine.ErrUnsupported))
		return
	}
	m, err := s.driver.Attach(r.Context(), inst)
	if err != nil {
		writeError(w, err)
		return
	}
	dialCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	upstream, err := m.Dial(dialCtx, uint32(port))
	cancel()
	if err != nil {
		writeError(w, err)
		return
	}
	conn, err := upgradeConn(w, dialUpgrade)
	if err != nil {
		_ = upstream.Close()
		return
	}
	netutil.Splice(conn, upstream)
}

// upgradeConn takes the request's connection and answers 101 with token.
func upgradeConn(w http.ResponseWriter, token string) (net.Conn, error) {
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, err
	}
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + token + "\r\n\r\n")
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &netutil.BufConn{Conn: conn, R: rw.Reader}, nil
}

// serveConn serves h on one connection, and returns once it is closed or
// hijacked (an exec's frames): a hijacked one is the handler's to finish.
func serveConn(conn net.Conn, h http.Handler) {
	l := &oneConn{conn: conn, closed: make(chan struct{})}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				_ = l.Close()
			}
		},
	}
	_ = srv.Serve(l)
}

// oneConn is a listener that accepts one connection, then waits to be
// closed.
type oneConn struct {
	mu     sync.Mutex
	conn   net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *oneConn) Accept() (net.Conn, error) {
	l.mu.Lock()
	conn := l.conn
	l.conn = nil
	l.mu.Unlock()
	if conn != nil {
		return conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConn) Addr() net.Addr { return oneConnAddr{} }

type oneConnAddr struct{}

func (oneConnAddr) Network() string { return "served" }
func (oneConnAddr) String() string  { return "served" }

// statusOf is the HTTP status of an error's code.
func statusOf(code string) int {
	switch code {
	case "refused":
		return http.StatusForbidden
	case "version":
		return http.StatusPreconditionFailed
	case "not-found":
		return http.StatusNotFound
	case "exists", "in-use", "committed", "not-running", "stale":
		return http.StatusConflict
	case "capacity":
		return http.StatusServiceUnavailable
	case "unsupported", "not-implemented", "no-key":
		return http.StatusNotImplemented
	}
	return http.StatusInternalServerError
}

func writeError(w http.ResponseWriter, err error) {
	resp := errorResponse{Error: err.Error(), Code: codeOf(err)}
	if ve, ok := errors.AsType[*VersionError](err); ok {
		resp.Version = ve
	}
	writeJSON(w, statusOf(resp.Code), resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
