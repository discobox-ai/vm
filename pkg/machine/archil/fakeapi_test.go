package archil

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	// tokens are every port token minted, revoked or not. A sandbox's
	// outlive it, and a fork is given its source's, since Archil says
	// neither that a delete revokes them nor that a fork does not copy
	// them: the worst case.
	tokens []*fakeToken
	// gateways serve each sandbox port's hostname, as Archil's edge does,
	// over TLS at a loopback address of their own.
	gateways []*httptest.Server
	// gatewayTLS trusts the gateways' certificate, httptest's, which every
	// TLS server it starts shares.
	gatewayTLS *tls.Config
	// creates are the body of every sandbox create, as sent.
	creates []map[string]any
	// listings are the query of every sandbox listing.
	listings []url.Values
	// containers, when set, runs each sandbox as a Docker container, and
	// its processes in it, rather than on this host (container_test.go).
	containers *fakeContainers
}

// fakeToken is a port token.
type fakeToken struct {
	id, secret, sandbox string
	port                int
	created, expires    time.Time
	revoked             bool
}

// honored reports whether the token still opens a connection.
func (t *fakeToken) honored() bool {
	return !t.revoked && (t.expires.IsZero() || time.Now().Before(t.expires))
}

type fakeSandbox struct {
	id, name, status, image string
	vcpus                   int
	memMiB                  uint64
	created                 time.Time
	mounts                  []mount
	network                 json.RawMessage
	ports                   map[int]bool
	// gateways are the hostnames of its ports, each served by a gateway of
	// the fake's, made at the first token or exposure.
	gateways       map[int]string
	serviceIngress bool
	forkOf, route  string
	// labels are set at create or fork and never changed: Archil has no
	// update.
	labels map[string]string
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
	probe := httptest.NewTLSServer(http.NotFoundHandler())
	f.gatewayTLS = probe.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	probe.Close()
	mux := http.NewServeMux()
	for pattern, h := range map[string]func(w http.ResponseWriter, r *http.Request){
		"GET /api/sandboxes":                                 f.listSandboxes,
		"POST /api/sandboxes":                                f.createSandbox,
		"GET /api/sandboxes/{sid}":                           f.getSandbox,
		"DELETE /api/sandboxes/{sid}":                        f.deleteSandbox,
		"POST /api/sandboxes/{sid}/start":                    f.startSandbox,
		"POST /api/sandboxes/{sid}/stop":                     f.stopSandbox,
		"POST /api/sandboxes/{sid}/pause":                    f.pauseSandbox,
		"POST /api/sandboxes/{sid}/resume":                   f.resumeSandbox,
		"POST /api/sandboxes/{sid}/fork":                     f.forkSandbox,
		"GET /api/sandboxes/{sid}/network":                   f.getNetwork,
		"PUT /api/sandboxes/{sid}/network":                   f.putNetwork,
		"PUT /api/sandboxes/{sid}/ports/{port}":              f.exposePort,
		"DELETE /api/sandboxes/{sid}/ports/{port}":           f.unexposePort,
		"GET /api/sandboxes/{sid}/port-tokens":               f.listTokens,
		"POST /api/sandboxes/{sid}/port-tokens":              f.mintToken,
		"DELETE /api/sandboxes/{sid}/port-tokens/{token_id}": f.revokeToken,
		"POST /api/sandboxes/{sid}/connections":              f.connection,
		"GET /api/disks":                                     f.listDisks,
		"POST /api/disks":                                    f.createDisk,
		"DELETE /api/disks/{id}":                             f.deleteDisk,
		"GET /ws/{sid}":                                      f.processAPI,
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
	t.Cleanup(func() {
		f.srv.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, g := range f.gateways {
			g.Close()
		}
	})
	return f
}

// honoredTokens are the IDs of a sandbox's tokens that still open a
// connection, whether or not the sandbox is still there.
func (f *fakeAPI) honoredTokens(sandbox string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, t := range f.tokens {
		if t.sandbox == sandbox && t.honored() {
			ids = append(ids, t.id)
		}
	}
	return ids
}

// gatewayClient reaches the fake's gateways, over HTTP/1.1, as Archil's
// edge is reached.
func (f *fakeAPI) gatewayClient() *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: f.gatewayTLS.Clone()}}
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
		"base_image": s.image, "created_at": s.created, "mounts": s.mounts, "labels": s.labels,
	}
}

// checkLabels refuses labels as Archil was seen to (2026-11-21): at most
// 32, keys and values 1-63 letters, digits, ".", "_", or "-", and no key
// under "archil.".
func checkLabels(labels map[string]string) error {
	if len(labels) > 32 {
		return errors.New("labels may have at most 32 entries")
	}
	for k, v := range labels {
		if strings.HasPrefix(strings.ToLower(k), "archil.") {
			return fmt.Errorf("label key %q uses the reserved archil. prefix", k)
		}
		if !labelPart(k) || !labelPart(v) {
			return fmt.Errorf("label %q value must be 1-63 letters, numbers, or . _ -", k)
		}
	}
	return nil
}

func labelPart(s string) bool {
	return len(s) >= 1 && len(s) <= 63 && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") == ""
}

// matches reports whether a sandbox passes a listing's filters, as
// Archil's: name, name_prefix, and every label, given as key=value.
func (s *fakeSandbox) matches(q url.Values) (bool, error) {
	if q.Has("name") && s.name != q.Get("name") || !strings.HasPrefix(s.name, q.Get("name_prefix")) {
		return false, nil
	}
	for _, l := range q["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return false, fmt.Errorf("label %q must be key=value", l)
		}
		if got, has := s.labels[k]; !has || got != v {
			return false, nil
		}
	}
	return true, nil
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
	var found []*fakeSandbox
	for _, s := range f.sandboxes {
		ok, err := s.matches(r.URL.Query())
		if err != nil {
			fail(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if ok {
			found = append(found, s)
		}
	}
	f.listings = append(f.listings, r.URL.Query())
	from, to, next, err := f.page(r, len(found))
	if err != nil {
		fail(w, http.StatusBadRequest, "", err.Error())
		return
	}
	views := []map[string]any{}
	for _, s := range found[from:to] {
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
		ports: map[int]bool{}, gateways: map[int]string{}, route: fmt.Sprintf("route%04d", f.seq), network: json.RawMessage(`{}`),
		root: filepath.Join(f.dir, fmt.Sprintf("sandbox-%d", f.seq)),
	}
	return s, os.MkdirAll(s.root, 0o755)
}

func (f *fakeAPI) createSandbox(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	var body createSandbox
	var fields map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &body)
	}
	if err == nil {
		err = json.Unmarshal(raw, &fields)
	}
	if err != nil {
		fail(w, http.StatusBadRequest, "", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, fields)
	if err := checkLabels(body.Labels); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
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
	s.mounts, s.serviceIngress, s.labels = body.Mounts, body.ServiceIngress, body.Labels
	if err := os.WriteFile(filepath.Join(s.root, imageMarker), []byte(s.image), 0o644); err != nil {
		fail(w, http.StatusInternalServerError, "", err.Error())
		return
	}
	if f.containers != nil {
		if err := f.containers.create(s, s.image); err != nil {
			fail(w, http.StatusInternalServerError, "", err.Error())
			return
		}
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
	if f.containers != nil {
		if err := f.containers.boot(f, s); err != nil {
			s.status = "failed"
			return err
		}
		s.status = "running"
		return nil
	}
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
	if f.containers != nil {
		f.containers.halt(s)
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

// pauseSandbox pauses a running sandbox, as Archil's pause does (and as it
// does itself at a session's limit), answering 202. Its guest keeps running:
// the fake's pause is its status alone.
func (f *fakeAPI) pauseSandbox(w http.ResponseWriter, r *http.Request) {
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
	s.status = "paused"
	reply(w, http.StatusAccepted, s.view(), "")
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
// with the source's mounts and network policy, and its labels unless the
// fork is given its own, as Archil's fork does.
func (f *fakeAPI) forkSandbox(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string             `json:"name"`
		Labels *map[string]string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "", err.Error())
		return
	}
	if body.Labels != nil {
		if err := checkLabels(*body.Labels); err != nil {
			fail(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
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
	s.serviceIngress = src.serviceIngress
	s.labels = maps.Clone(src.labels)
	if body.Labels != nil {
		s.labels = *body.Labels
	}
	for port := range src.ports {
		s.ports[port] = true
	}
	for _, t := range slices.Clone(f.tokens) {
		if t.sandbox == src.id && t.honored() {
			c := *t
			c.id, c.sandbox = randomTokenID(), s.id
			f.tokens = append(f.tokens, &c)
		}
	}
	if f.containers != nil {
		if err := f.containers.fork(src, s); err != nil {
			fail(w, http.StatusInternalServerError, "", err.Error())
			return
		}
	}
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
	if f.containers != nil {
		f.containers.remove(s)
	}
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

// port is r's port, or 0 after answering 400.
func port(w http.ResponseWriter, r *http.Request) int {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		fail(w, http.StatusBadRequest, "", "bad port")
		return 0
	}
	return port
}

func (f *fakeAPI) exposePort(w http.ResponseWriter, r *http.Request) {
	port := port(w, r)
	if port == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sandbox(w, r)
	if s == nil {
		return
	}
	status := http.StatusOK
	if !s.ports[port] {
		s.ports[port], status = true, http.StatusCreated
	}
	reply(w, status, map[string]any{"port": port, "hostname": f.gateway(s, port)}, "")
}

// The edge below (unexposePort, the port tokens, the gateway) answers as
// Archil did (2026-11-18, aws-us-east-1, by hand and under go tool task
// test:archil): DELETE …/ports/{port} of a port not exposed is a 404
// "not_found"; the port and port-token calls are taken on a stopped
// sandbox, and a mint there gives the hostname without waking it; a request
// without a token, or a wrong one, is a 401 that wakes nothing, and one with
// it wakes a stopped sandbox. Two things it does on purpose that Archil
// does not: a sandbox's tokens outlive its delete, and a fork is given its
// source's, where Archil was seen to answer 401 for both. That is the worst
// case, so a revoke the driver drops shows as a token left honored.
func (f *fakeAPI) unexposePort(w http.ResponseWriter, r *http.Request) {
	port := port(w, r)
	if port == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sandbox(w, r)
	switch {
	case s == nil:
		return
	case !s.ports[port]:
		fail(w, http.StatusNotFound, "not_found", "Sandbox port not found")
		return
	}
	delete(s.ports, port)
	w.WriteHeader(http.StatusNoContent)
}

func randomTokenID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (t *fakeToken) view() map[string]any {
	v := map[string]any{"id": t.id, "port": t.port, "created_at": t.created}
	if !t.expires.IsZero() {
		v["expires_at"] = t.expires
	}
	return v
}

func (f *fakeAPI) mintToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Port int    `json:"port"`
		TTL  string `json:"ttl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Port < 1 || body.Port > 65535 {
		fail(w, http.StatusBadRequest, "", "bad body")
		return
	}
	var ttl time.Duration
	if body.TTL != "" {
		var err error
		if ttl, err = time.ParseDuration(body.TTL); err != nil || ttl <= 0 || ttl > 8760*time.Hour {
			fail(w, http.StatusBadRequest, "", "bad ttl "+body.TTL)
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sandbox(w, r)
	if s == nil {
		return
	}
	t := &fakeToken{id: randomTokenID(), secret: randomTokenID(), sandbox: s.id, port: body.Port, created: time.Now()}
	if ttl > 0 {
		t.expires = t.created.Add(ttl)
	}
	f.tokens = append(f.tokens, t)
	v := t.view()
	v["token"], v["hostname"] = t.secret, f.gateway(s, body.Port)
	reply(w, http.StatusCreated, v, "")
}

func (f *fakeAPI) listTokens(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sandbox(w, r)
	if s == nil {
		return
	}
	var mine []*fakeToken
	for _, t := range f.tokens {
		if t.sandbox == s.id && t.honored() {
			mine = append(mine, t)
		}
	}
	from, to, next, err := f.page(r, len(mine))
	if err != nil {
		fail(w, http.StatusBadRequest, "", err.Error())
		return
	}
	views := []map[string]any{}
	for _, t := range mine[from:to] {
		views = append(views, t.view())
	}
	reply(w, http.StatusOK, map[string]any{"tokens": views}, next)
}

func (f *fakeAPI) revokeToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sandbox(w, r)
	if s == nil {
		return
	}
	for _, t := range f.tokens {
		if t.sandbox == s.id && t.id == r.PathValue("token_id") && t.honored() {
			t.revoked = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	fail(w, http.StatusNotFound, "port_token_not_found", "no port token "+r.PathValue("token_id"))
}

// gateway is the hostname of a sandbox's port, served from its first use
// for as long as the fake runs. The caller holds mu.
func (f *fakeAPI) gateway(s *fakeSandbox, port int) string {
	if host, ok := s.gateways[port]; ok {
		return host
	}
	id := s.id
	g := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.serveGateway(w, r, id, port)
	}))
	g.StartTLS()
	f.gateways = append(f.gateways, g)
	s.gateways[port] = g.Listener.Addr().String()
	return s.gateways[port]
}

// serveGateway is Archil's edge at a sandbox port's hostname: an exposed
// port answers anyone, and any other only a request carrying a token for it,
// refusing the rest with 401 before the sandbox sees them. A request let
// through starts a stopped sandbox, or resumes a paused one, and is answered
// 502, since no guest service runs behind the fake.
func (f *fakeAPI) serveGateway(w http.ResponseWriter, r *http.Request, id string, port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s *fakeSandbox
	for _, o := range f.sandboxes {
		if o.id == id {
			s = o
		}
	}
	if s == nil {
		http.Error(w, "no such sandbox", http.StatusNotFound)
		return
	}
	if !s.ports[port] && !slices.ContainsFunc(f.tokens, func(t *fakeToken) bool {
		return t.sandbox == id && t.port == port && t.honored() && t.secret == r.Header.Get(tokenHeader)
	}) {
		http.Error(w, "a port token is required", http.StatusUnauthorized)
		return
	}
	switch s.status {
	case "stopped", "exited":
		if err := f.boot(s); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case "paused":
		s.status = "running"
	}
	http.Error(w, "nothing serves the port", http.StatusBadGateway)
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
	root, name, sb, containers := s.root, s.name, s, f.containers
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
	var code int
	if containers != nil {
		code, err = containers.run(procCtx, sb, req.startRequest, stdin, out.stream(1), out.stream(2))
	} else {
		code, err = runCommand(procCtx, req.startRequest, root, name, stdin, out)
	}
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

// parseLine reads back a line the driver sends, by having sh read it: the
// line's exec becomes a printf of each word sh makes of the rest, so a word
// the driver quoted wrong comes back as sh would run it, not as the driver
// meant it. A line command made, which enters systemd's namespaces
// (enterScript), is read back as its directory, its account, and its argv,
// and entered is set; any other is its argv alone.
func parseLine(line string) (dir, user string, argv []string, entered bool, err error) {
	rest, ok := strings.CutPrefix(line, "exec ")
	if !ok {
		return "", "", nil, false, fmt.Errorf("unknown command %q", line)
	}
	out, err := exec.CommandContext(context.Background(), "sh", "-c", `printf '%s\0' `+rest).Output() //nolint:gosec // G204: the driver's own line, read by sh as the guest would
	if err != nil {
		return "", "", nil, false, fmt.Errorf("sh read %q: %w", line, err)
	}
	words := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if !isScript(words, enterScript) {
		return "", "", words, false, nil
	}
	if len(words) < 7 {
		return "", "", nil, false, fmt.Errorf("an entered line with no argv: %q", line)
	}
	return words[4], words[5], words[6:], true, nil
}

// isScript reports whether argv runs the driver's script text.
func isScript(argv []string, text string) bool {
	return len(argv) >= 4 && argv[0] == "sh" && argv[1] == "-c" && argv[2] == text
}

// fakeNoSystemd are the images the fake has no systemd in and no apt-get
// to install it with.
var fakeNoSystemd = []string{"alpine"}

// fakeServices is where the fake keeps a guest's Archil services, in its
// root, so a fork carries them.
const fakeServices = ".archil-services"

// fakeSystemd plays ensureScript in a sandbox's root: systemd installed but
// for an image in fakeNoSystemd, and the service registered.
func fakeSystemd(root string, stderr io.Writer) int {
	image, _ := os.ReadFile(filepath.Join(root, imageMarker))
	if !fileExists(filepath.Join(root, "lib/systemd/systemd")) {
		for _, no := range fakeNoSystemd {
			if strings.HasPrefix(string(image), no) {
				_, _ = io.WriteString(stderr, "the base has no /lib/systemd/systemd, and no apt-get to install it")
				return 3
			}
		}
	}
	for _, path := range []string{"lib/systemd/systemd", filepath.Join(fakeServices, systemdService)} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(path)), 0o755); err != nil {
			_, _ = io.WriteString(stderr, err.Error())
			return 1
		}
		if err := os.WriteFile(filepath.Join(root, path), nil, 0o755); err != nil { //nolint:gosec // G306: the fake's systemd, an executable
			_, _ = io.WriteString(stderr, err.Error())
			return 1
		}
	}
	return 0
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// runCommand runs a started process in a sandbox's root and returns its exit
// code. An entered command fails while the guest has no systemd service.
func runCommand(ctx context.Context, req startRequest, root, hostname string, stdin io.Reader, out *frameWriter) (int, error) {
	stdout, stderr := out.stream(1), out.stream(2)
	if req.Command == "true" {
		return 0, nil
	}
	dir, _, argv, entered, err := parseLine(req.Command)
	if err != nil {
		return 0, err
	}
	if len(argv) == 0 {
		return 0, errors.New("no argv")
	}
	if !entered {
		if !isScript(argv, ensureScript) {
			return 0, fmt.Errorf("a line that enters no namespace: %q", req.Command)
		}
		return fakeSystemd(root, stderr), nil
	}
	if !fileExists(filepath.Join(root, fakeServices, systemdService)) {
		_, _ = io.WriteString(stderr, "systemd is not running in the guest")
		return enterNotRunning, nil
	}
	arg := func() string { return guest.Resolve(root, argv[len(argv)-1]) }
	switch {
	case isScript(argv, infoScript):
		arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
		_, _ = io.WriteString(stdout, arch+"\n"+hostname+"\n")
		return 0, nil
	case isScript(argv, settleScript):
		_, _ = io.WriteString(stdout, "running\n")
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
	cmd.Dir = guest.Resolve(root, dir)
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
