package archil

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// fakeAPI is Archil's control plane and process API with no cloud behind
// them, as the driver calls them. A sandbox is a directory standing in for
// its root disk, and a fork a copy of it. A disk is a directory, which a
// sandbox that boots with it mounted sees at its path, a link in its root,
// removed when it stops. Memory is not modeled. The process API understands
// the command lines the driver sends: a request's argv runs on this host, in
// the sandbox's directory, and the driver's own scripts (copies, info) are
// done here instead.
//
// Every listing pages pageSize at a time, as Archil's disk listing pages,
// so a driver that reads one page loses sight of VMs.
type fakeAPI struct {
	dir string
	srv *httptest.Server

	mu sync.Mutex
	// sandboxes and disks are in the order made: Archil lists the oldest
	// first.
	sandboxes []*fakeSandbox
	disks     []*fakeDisk
	procs     map[string]*fakeProc
	seq       int
	// pageSize is how many sandboxes or disks a listing answers at once.
	pageSize int
	// terminals logs each TTY process's terminal, as {rows, cols} at its
	// start and at each resize.
	terminals [][2]uint16
}

type fakeSandbox struct {
	id, name, status, image string
	vcpus                   int
	memMiB                  uint64
	created                 time.Time
	mounts                  []mount
	network                 json.RawMessage
	ports                   map[int]string
	forkOf, route           string
	// root is the directory standing in for its root disk.
	root string
}

type fakeDisk struct {
	id, name string
	created  time.Time
	dir      string
}

// fakeProc is a process the fake runs, which a kill on another connection
// ends.
type fakeProc struct {
	sandbox string
	cancel  context.CancelFunc
}

// fakeDefaultImage is what Archil runs when a create names no image.
const fakeDefaultImage = "node:24-bookworm"

// imageMarker is a file in a fresh sandbox's root naming the image it was
// made from, which a fork carries.
const imageMarker = ".archil-image"

func newFakeAPI(t *testing.T, dir string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{dir: dir, procs: map[string]*fakeProc{}, pageSize: 2}
	mux := http.NewServeMux()
	for pattern, h := range map[string]func(w http.ResponseWriter, r *http.Request){
		"GET /api/sandboxes":                    f.listSandboxes,
		"POST /api/sandboxes":                   f.createSandbox,
		"GET /api/sandboxes/{sid}":              f.getSandbox,
		"DELETE /api/sandboxes/{sid}":           f.deleteSandbox,
		"POST /api/sandboxes/{sid}/start":       f.startSandbox,
		"POST /api/sandboxes/{sid}/stop":        f.stopSandbox,
		"POST /api/sandboxes/{sid}/resume":      f.resumeSandbox,
		"POST /api/sandboxes/{sid}/fork":        f.forkSandbox,
		"GET /api/sandboxes/{sid}/network":      f.getNetwork,
		"PUT /api/sandboxes/{sid}/network":      f.putNetwork,
		"PUT /api/sandboxes/{sid}/ports/{port}": f.exposePort,
		"POST /api/sandboxes/{sid}/connections": f.connection,
		"GET /api/disks":                        f.listDisks,
		"POST /api/disks":                       f.createDisk,
		"DELETE /api/disks/{id}":                f.deleteDisk,
		"GET /ws/{sid}":                         f.processAPI,
	} {
		mux.HandleFunc(pattern, h)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/ws/") && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer key-") {
			fail(w, http.StatusUnauthorized, "", "no key")
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// driver is an archil driver of pool whose fresh VMs run base, calling the
// fake.
func (f *fakeAPI) driver(t *testing.T, pool, base string) *Driver {
	t.Helper()
	d, err := New(machine.Config{
		Pool: pool, Base: base, Keys: machine.StaticKey(machine.Key{Driver: "archil", Secret: "key-fake"}),
		DriverConfig: json.RawMessage(`{"region":"aws-us-east-1","baseUrl":"` + f.srv.URL + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func reply(w http.ResponseWriter, status int, data any, next string) {
	env := map[string]any{"success": true, "data": data}
	if next != "" {
		env["nextCursor"] = next
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(env); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{"success": false, "error": msg, "code": code}); err != nil {
		panic(http.ErrAbortHandler)
	}
}

// page is the part of n items a listing's cursor asks for, and the next
// page's cursor: "" for the last.
func (f *fakeAPI) page(r *http.Request, n int) (from, to int, next string, err error) {
	if c := r.URL.Query().Get("cursor"); c != "" {
		if from, err = strconv.Atoi(c); err != nil || from < 0 || from > n {
			return 0, 0, "", fmt.Errorf("bad cursor %q", c)
		}
	}
	size := f.pageSize
	if limit, _ := strconv.Atoi(r.URL.Query().Get("limit")); limit > 0 && (size == 0 || limit < size) {
		size = limit
	}
	to = n
	if size > 0 && from+size < n {
		to = from + size
		next = strconv.Itoa(to)
	}
	return from, to, next, nil
}

func (s *fakeSandbox) view() map[string]any {
	return map[string]any{
		"sandbox_id": s.id, "name": s.name, "status": s.status, "vcpu_count": s.vcpus, "mem_size_mib": s.memMiB,
		"base_image": s.image, "created_at": s.created, "mounts": s.mounts,
	}
}

// sandbox is the sandbox of r's sid, or nil after answering 404. The caller
// holds mu.
func (f *fakeAPI) sandbox(w http.ResponseWriter, r *http.Request) *fakeSandbox {
	for _, s := range f.sandboxes {
		if s.id == r.PathValue("sid") {
			return s
		}
	}
	fail(w, http.StatusNotFound, "sandbox_not_found", "no sandbox "+r.PathValue("sid"))
	return nil
}

func (f *fakeAPI) listSandboxes(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	from, to, next, err := f.page(r, len(f.sandboxes))
	if err != nil {
		fail(w, http.StatusBadRequest, "", err.Error())
		return
	}
	views := []map[string]any{}
	for _, s := range f.sandboxes[from:to] {
		views = append(views, s.view())
	}
	reply(w, http.StatusOK, map[string]any{"sandboxes": views}, next)
}

// taken reports whether a sandbox has the name. The caller holds mu.
func (f *fakeAPI) taken(name string) bool {
	return slices.ContainsFunc(f.sandboxes, func(s *fakeSandbox) bool { return s.name == name })
}

// newSandbox makes a sandbox's record and root. The caller holds mu.
func (f *fakeAPI) newSandbox(name string) (*fakeSandbox, error) {
	f.seq++
	s := &fakeSandbox{
		id: fmt.Sprintf("01a1e580-0000-7000-8000-%012x", f.seq), name: name, created: time.Now(),
		ports: map[int]string{}, route: fmt.Sprintf("route%04d", f.seq), network: json.RawMessage(`{}`),
		root: filepath.Join(f.dir, fmt.Sprintf("sandbox-%d", f.seq)),
	}
	return s, os.MkdirAll(s.root, 0o755)
}

func (f *fakeAPI) createSandbox(w http.ResponseWriter, r *http.Request) {
	var body createSandbox
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.taken(body.Name) {
		fail(w, http.StatusConflict, "sandbox_name_conflict", "a sandbox named "+body.Name+" exists")
		return
	}
	s, err := f.newSandbox(body.Name)
	if err != nil {
		fail(w, http.StatusInternalServerError, "", err.Error())
		return
	}
	s.image, s.vcpus, s.memMiB = cmp.Or(body.BaseImage, fakeDefaultImage), cmp.Or(body.VCPUs, 1), cmp.Or(body.MemoryMiB, 2048)
	if body.Network != nil {
		if s.network, err = json.Marshal(body.Network); err != nil {
			fail(w, http.StatusBadRequest, "", err.Error())
			return
		}
	}
	s.mounts = body.Mounts
	if err := os.WriteFile(filepath.Join(s.root, imageMarker), []byte(s.image), 0o644); err != nil {
		fail(w, http.StatusInternalServerError, "", err.Error())
		return
	}
	f.sandboxes = append(f.sandboxes, s)
	if err := f.boot(s); err != nil {
		fail(w, http.StatusInternalServerError, "", err.Error())
		return
	}
	reply(w, http.StatusCreated, s.view(), "")
}

func (f *fakeAPI) getSandbox(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sandbox(w, r); s != nil {
		reply(w, http.StatusOK, s.view(), "")
	}
}

// disk is the disk of an ID, or nil. The caller holds mu.
func (f *fakeAPI) disk(id string) *fakeDisk {
	for _, d := range f.disks {
		if d.id == id {
			return d
		}
	}
	return nil
}

// boot runs a sandbox with its mounts: each disk a link at its path in the
// root. The caller holds mu.
func (f *fakeAPI) boot(s *fakeSandbox) error {
	for _, m := range s.mounts {
		d := f.disk(m.DiskID)
		if d == nil {
			s.status = "failed"
			return fmt.Errorf("no disk %s", m.DiskID)
		}
		target := guest.Resolve(s.root, m.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		_ = os.Remove(target)
		if err := os.Symlink(d.dir, target); err != nil {
			return err
		}
	}
	s.status = "running"
	return nil
}

// halt stops a sandbox: its processes are ended, and its disks unmounted.
// The caller holds mu.
func (f *fakeAPI) halt(s *fakeSandbox) {
	for id, p := range f.procs {
		if p.sandbox == s.id {
			p.cancel()
			delete(f.procs, id)
		}
	}
	for _, m := range s.mounts {
		_ = os.Remove(guest.Resolve(s.root, m.Path))
	}
	s.status = "stopped"
}

func (f *fakeAPI) startSandbox(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mounts *[]mount `json:"mounts"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			fail(w, http.StatusBadRequest, "", err.Error())
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sandbox(w, r)
	switch {
	case s == nil:
		return
	case s.status == "running" && body.Mounts == nil:
		reply(w, http.StatusOK, s.view(), "")
		return
	case s.status != "stopped" && s.status != "exited" && s.status != "paused":
		fail(w, http.StatusConflict, "sandbox_invalid_state", "sandbox is "+s.status)
		return
	}
	if body.Mounts != nil {
		s.mounts = *body.Mounts
	}
	if err := f.boot(s); err != nil {
		fail(w, http.StatusInternalServerError, "", err.Error())
		return
	}
	reply(w, http.StatusOK, s.view(), "")
}

func (f *fakeAPI) stopSandbox(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sandbox(w, r)
	switch {
	case s == nil:
		return
	case s.status != "running" && s.status != "paused":
		fail(w, http.StatusConflict, "sandbox_invalid_state", "sandbox is "+s.status)
		return
	}
	f.halt(s)
	reply(w, http.StatusOK, s.view(), "")
}

func (f *fakeAPI) resumeSandbox(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sandbox(w, r)
	switch {
	case s == nil:
		return
	case s.status == "running":
	case s.status != "paused":
		fail(w, http.StatusConflict, "sandbox_invalid_state", "sandbox is "+s.status)
		return
	default:
		s.status = "running"
	}
	reply(w, http.StatusOK, s.view(), "")
}

// forkSandbox branches the source's disk into a sandbox that boots at once,
// with the source's mounts and network policy, as Archil's fork does.
func (f *fakeAPI) forkSandbox(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	src := f.sandbox(w, r)
	if src == nil {
		return
	}
	if f.taken(body.Name) {
		fail(w, http.StatusConflict, "sandbox_name_conflict", "a sandbox named "+body.Name+" exists")
		return
	}
	s, err := f.newSandbox(body.Name)
	if err == nil {
		err = os.Remove(s.root)
	}
	if err == nil {
		err = fsutil.CopyTree(src.root, s.root)
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "", err.Error())
		return
	}
	// A running source's mounts are links in its root, which the copy
	// carries; boot makes them again.
	s.image, s.vcpus, s.memMiB, s.forkOf = src.image, src.vcpus, src.memMiB, src.id
	s.mounts, s.network = slices.Clone(src.mounts), slices.Clone(src.network)
	f.sandboxes = append(f.sandboxes, s)
	if err := f.boot(s); err != nil {
		fail(w, http.StatusInternalServerError, "", err.Error())
		return
	}
	reply(w, http.StatusCreated, s.view(), "")
}

func (f *fakeAPI) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sandbox(w, r)
	switch {
	case s == nil:
		return
	case s.status != "stopped" && s.status != "exited" && s.status != "failed":
		fail(w, http.StatusConflict, "sandbox_invalid_state", "a "+s.status+" sandbox is not deleted")
		return
	case slices.ContainsFunc(f.sandboxes, func(o *fakeSandbox) bool { return o.forkOf == s.id }):
		fail(w, http.StatusConflict, "sandbox_has_forks", "the sandbox has forks")
		return
	}
	f.sandboxes = slices.DeleteFunc(f.sandboxes, func(o *fakeSandbox) bool { return o == s })
	_ = os.RemoveAll(s.root)
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeAPI) getNetwork(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sandbox(w, r); s != nil {
		reply(w, http.StatusOK, s.network, "")
	}
}

// putNetwork replaces a running sandbox's policy, as Archil applies one only
// to a running sandbox.
func (f *fakeAPI) putNetwork(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil || !json.Valid(raw) {
		fail(w, http.StatusBadRequest, "", "not JSON")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sandbox(w, r)
	switch {
	case s == nil:
		return
	case s.status != "running":
		fail(w, http.StatusConflict, "sandbox_invalid_state", "sandbox is "+s.status)
		return
	}
	s.network = raw
	reply(w, http.StatusOK, s.network, "")
}

func (f *fakeAPI) exposePort(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		fail(w, http.StatusBadRequest, "", "bad port")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sandbox(w, r)
	if s == nil {
		return
	}
	status := http.StatusOK
	if _, ok := s.ports[port]; !ok {
		s.ports[port], status = fmt.Sprintf("%d-%s.prod-aws-us-east-1-green.archil.test", port, s.route), http.StatusCreated
	}
	reply(w, status, map[string]any{"port": port, "hostname": s.ports[port]}, "")
}

func (f *fakeAPI) connection(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sandbox(w, r)
	switch {
	case s == nil:
		return
	case s.status != "running":
		fail(w, http.StatusConflict, "sandbox_invalid_state", "sandbox is "+s.status)
		return
	}
	reply(w, http.StatusCreated, map[string]any{"url": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws/" + s.id}, "")
}

func (f *fakeAPI) listDisks(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var named []*fakeDisk
	for _, d := range f.disks {
		if n := r.URL.Query().Get("name"); n == "" || n == d.name {
			named = append(named, d)
		}
	}
	from, to, next, err := f.page(r, len(named))
	if err != nil {
		fail(w, http.StatusBadRequest, "", err.Error())
		return
	}
	views := []disk{}
	for _, d := range named[from:to] {
		views = append(views, disk{ID: d.id, Name: d.name, Created: d.created})
	}
	reply(w, http.StatusOK, views, next)
}

// createDisk answers the disk of the name when there is one, as Archil does.
func (f *fakeAPI) createDisk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" || len(body.Name) > 100 {
		fail(w, http.StatusBadRequest, "", "bad name")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.disks {
		if d.name == body.Name {
			reply(w, http.StatusOK, map[string]any{"diskId": d.id}, "")
			return
		}
	}
	f.seq++
	d := &fakeDisk{id: fmt.Sprintf("dsk-%016x", f.seq), name: body.Name, created: time.Now(), dir: filepath.Join(f.dir, fmt.Sprintf("disk-%d", f.seq))}
	if err := os.MkdirAll(d.dir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, "", err.Error())
		return
	}
	f.disks = append(f.disks, d)
	reply(w, http.StatusCreated, map[string]any{"diskId": d.id}, "")
}

// deleteDisk removes a disk, refused while a running sandbox mounts it.
func (f *fakeAPI) deleteDisk(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.disk(r.PathValue("id"))
	if d == nil {
		fail(w, http.StatusNotFound, "", "no disk")
		return
	}
	for _, s := range f.sandboxes {
		if s.status == "running" && slices.ContainsFunc(s.mounts, func(m mount) bool { return m.DiskID == d.id }) {
			fail(w, http.StatusConflict, "disk_in_use", "a running sandbox mounts the disk")
			return
		}
	}
	f.disks = slices.DeleteFunc(f.disks, func(o *fakeDisk) bool { return o == d })
	_ = os.RemoveAll(d.dir)
	w.WriteHeader(http.StatusNoContent)
}

// processAPI is a connection of the process API: a process's start, or a
// kill or a resize of one.
func (f *fakeAPI) processAPI(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(-1)
	ctx := r.Context()
	_, raw, err := c.Read(ctx)
	if err != nil {
		return
	}
	var req struct {
		startRequest
		ProcessID string `json:"process_id"`
		Rows      uint16 `json:"rows"`
		Cols      uint16 `json:"cols"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}
	event := func(v map[string]any) {
		b, err := json.Marshal(v)
		if err == nil {
			_ = c.Write(ctx, websocket.MessageText, b)
		}
	}
	switch req.Type {
	case "kill":
		f.mu.Lock()
		if p := f.procs[req.ProcessID]; p != nil {
			p.cancel()
		}
		f.mu.Unlock()
		event(map[string]any{"type": "killed"})
		return
	case "resize":
		f.mu.Lock()
		p := f.procs[req.ProcessID]
		if p != nil {
			f.terminals = append(f.terminals, [2]uint16{req.Rows, req.Cols})
		}
		f.mu.Unlock()
		if p == nil {
			// The fake's own answer: what Archil answers a resize of a
			// process it is not running has not been checked on Archil.
			event(map[string]any{"type": "error", "error": "process_not_found", "message": "no process " + req.ProcessID})
			return
		}
		event(map[string]any{"type": "resized"})
		return
	case "start":
	default:
		event(map[string]any{"type": "error", "error": "bad_request", "message": "unknown " + req.Type})
		return
	}

	f.mu.Lock()
	var s *fakeSandbox
	for _, o := range f.sandboxes {
		if o.id == r.PathValue("sid") {
			s = o
		}
	}
	if s == nil || s.status != "running" {
		f.mu.Unlock()
		event(map[string]any{"type": "error", "error": "sandbox_not_running", "message": "the sandbox is not running"})
		return
	}
	if t, ok := req.Terminal.(map[string]any); ok {
		rows, _ := t["rows"].(float64)
		cols, _ := t["cols"].(float64)
		f.terminals = append(f.terminals, [2]uint16{uint16(rows), uint16(cols)})
	}
	f.seq++
	id := fmt.Sprintf("p%d", f.seq)
	procCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.procs[id] = &fakeProc{sandbox: s.id, cancel: cancel}
	root, name := s.root, s.name
	f.mu.Unlock()

	event(map[string]any{"type": "started", "process_id": id})
	out := &frameWriter{ctx: ctx, conn: c}
	stdin, feed := io.Pipe()
	defer stdin.Close()
	go func() {
		defer cancel()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				_ = feed.CloseWithError(err)
				return
			}
			if typ == websocket.MessageBinary {
				if _, err := feed.Write(data); err != nil {
					return
				}
			} else if strings.Contains(string(data), "close_stdin") {
				_ = feed.Close()
			}
		}
	}()
	code, err := runCommand(procCtx, req.startRequest, root, name, stdin, out)
	// Gone before its exit is sent, so nothing asked of it after the client
	// reads its exit finds it.
	f.mu.Lock()
	delete(f.procs, id)
	f.mu.Unlock()
	switch {
	case procCtx.Err() != nil:
		event(map[string]any{"type": "exit", "status": "killed", "exit_reason": "killed", "cursor": out.end()})
	case err != nil:
		_, _ = out.stream(2).Write([]byte("fake archil: " + err.Error()))
		event(map[string]any{"type": "exit", "status": "exited", "exit_code": 127, "cursor": out.end()})
	default:
		event(map[string]any{"type": "exit", "status": "exited", "exit_code": code, "cursor": out.end()})
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// frameWriter sends a process's output as Archil does: a frame per write, its
// stream's byte, then its offset in both streams' output together.
type frameWriter struct {
	ctx    context.Context
	conn   *websocket.Conn
	mu     sync.Mutex
	cursor uint64
}

func (w *frameWriter) end() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cursor
}

type streamWriter struct {
	w      *frameWriter
	stream byte
}

func (w *frameWriter) stream(n byte) io.Writer { return streamWriter{w, n} }

func (s streamWriter) Write(p []byte) (int, error) {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	frame := make([]byte, 9+len(p))
	frame[0] = s.stream
	binary.BigEndian.PutUint64(frame[1:9], s.w.cursor)
	copy(frame[9:], p)
	if err := s.w.conn.Write(s.w.ctx, websocket.MessageBinary, frame); err != nil {
		return 0, err
	}
	s.w.cursor += uint64(len(p))
	return len(p), nil
}

// parseLine reads back a line command made, the account it runs as and its
// argv, by having sh read it: the line's exec becomes a printf of each word
// sh makes of the rest, so a word the driver quoted wrong comes back as sh
// would run it, not as the driver meant it.
func parseLine(line string) (user string, argv []string, err error) {
	rest, ok := strings.CutPrefix(line, "exec ")
	if !ok {
		return "", nil, fmt.Errorf("unknown command %q", line)
	}
	out, err := exec.CommandContext(context.Background(), "sh", "-c", `printf '%s\0' `+rest).Output() //nolint:gosec // G204: the driver's own line, read by sh as the guest would
	if err != nil {
		return "", nil, fmt.Errorf("sh read %q: %w", line, err)
	}
	words := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if words[0] != "runuser" {
		return "", words, nil
	}
	// runuser -u USER -- env HOME=... ARGV...
	if len(words) < 7 || words[1] != "-u" || words[3] != "--" || words[4] != "env" || !strings.HasPrefix(words[5], "HOME=") {
		return "", nil, fmt.Errorf("unknown runuser line %q", line)
	}
	return words[2], words[6:], nil
}

// isScript reports whether argv runs the driver's script text.
func isScript(argv []string, text string) bool {
	return len(argv) >= 4 && argv[0] == "sh" && argv[1] == "-c" && argv[2] == text
}

// runCommand runs a started process in a sandbox's root and returns its exit
// code.
func runCommand(ctx context.Context, req startRequest, root, hostname string, stdin io.Reader, out *frameWriter) (int, error) {
	stdout, stderr := out.stream(1), out.stream(2)
	if req.Command == "true" {
		return 0, nil
	}
	_, argv, err := parseLine(req.Command)
	if err != nil {
		return 0, err
	}
	if len(argv) == 0 {
		return 0, errors.New("no argv")
	}
	arg := func() string { return guest.Resolve(root, argv[len(argv)-1]) }
	switch {
	case isScript(argv, infoScript):
		arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
		_, _ = io.WriteString(stdout, arch+"\n"+hostname+"\n")
		return 0, nil
	case isScript(argv, copyInScript):
		if err := guest.Unpack(stdin, arg()); err != nil {
			_, _ = io.WriteString(stderr, err.Error())
			return 2, nil
		}
		return 0, nil
	case isScript(argv, copyOutScript):
		return copyOut(arg(), stdin, stdout, stderr), nil
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: the test's own commands
	cmd.Dir = guest.Resolve(root, req.Cwd)
	cmd.Env = append(os.Environ(), guest.RootEnv+"="+root)
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	// Its stdin is a pipe the client may never close.
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

// copyOut plays copyOutScript: R once the path is tarred, then a chunk for
// each count stdin asks for, a short one last.
func copyOut(path string, stdin io.Reader, stdout, stderr io.Writer) int {
	if _, err := os.Lstat(path); err != nil {
		_, _ = io.WriteString(stderr, err.Error())
		return 1
	}
	var tarred bytes.Buffer
	if err := guest.Pack(&tarred, path, false); err != nil {
		_, _ = io.WriteString(stderr, err.Error())
		return 2
	}
	_, _ = stdout.Write([]byte{copyOutReady})
	asks := bufio.NewScanner(stdin)
	for asks.Scan() {
		count, err := strconv.Atoi(strings.TrimSpace(asks.Text()))
		if err != nil {
			return 2
		}
		chunk := tarred.Next(count)
		_, _ = fmt.Fprintf(stdout, "%d\n", len(chunk))
		_, _ = stdout.Write(chunk)
		if len(chunk) < count {
			return 0
		}
	}
	return 0
}
