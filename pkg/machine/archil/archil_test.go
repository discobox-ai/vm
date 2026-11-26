package archil

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/discobox-ai/vm/pkg/machine"
)

func testDriver(pool string) *Driver {
	return &Driver{api: &api{}, pool: pool}
}

// A VM's data volume is named for it, and a disk of another pool, or not a
// VM's, is none; an upgrade's sandbox fits Archil's limit and is never a
// name a create gives a VM's sandbox.
func TestNames(t *testing.T) {
	d := testDriver("p1")
	for _, name := range []string{"dev", "a-b-c"} {
		if got := d.nameOfDisk(d.dataDiskName(name)); got != name {
			t.Errorf("nameOfDisk(%s) = %q", d.dataDiskName(name), got)
		}
		if got := testDriver("p").nameOfDisk(d.dataDiskName(name)); got != "" {
			t.Errorf("pool p reads p1's disk %s as %q", d.dataDiskName(name), got)
		}
	}
	for _, dk := range []string{"dvm-p1--dev", "dvm-p1--dev--data--data", "dvm-p1-0a1b2c3d-data", "other--data"} {
		if got := d.nameOfDisk(dk); got != "" {
			t.Errorf("disk %s read as VM %q's data volume", dk, got)
		}
	}
	long := strings.Repeat("x", maxSandboxName-len(d.sandboxName("")))
	for _, name := range []string{"dev", long} {
		up := d.upgradeName(name)
		rest, ok := strings.CutPrefix(up, d.prefix())
		if len(up) > maxSandboxName || !ok || !strings.Contains(rest, "--") || machine.ValidateName(rest) == nil {
			t.Errorf("upgradeName(%s) = %q: want at most %d characters, under the pool's prefix, and no VM's sandbox name", name, up, maxSandboxName)
		}
		if up == d.upgradeName(name) {
			t.Errorf("upgradeName(%s) gave %q twice", name, up)
		}
	}
}

// One sandbox is each VM (ADR 26-11-24-905 §3): an upgrade's new sandbox is
// not while the one it replaces lives, and is once it is gone; of two that
// both carry a VM's labels, the oldest is, then the lowest ID; a sandbox of
// another pool, being deleted, or with no VM name is none.
func TestVMs(t *testing.T) {
	d := testDriver("p")
	at := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	sb := func(id, name string, created time.Time, extra ...string) sandbox {
		labels := map[string]string{labelPool: "p", labelInstance: name}
		for i := 0; i+1 < len(extra); i += 2 {
			labels[extra[i]] = extra[i+1]
		}
		return sandbox{ID: id, Status: "stopped", Created: created, Labels: labels}
	}
	deleting := sb("gone", "c", at)
	deleting.Status = "deleting"
	all := []sandbox{
		sb("a-old", "a", at),
		sb("a-new", "a", at.Add(time.Hour), labelReplaces, "a-old"),
		sb("b-new", "b", at.Add(time.Hour), labelReplaces, "b-gone"),
		sb("c-2", "c", at.Add(time.Minute)),
		sb("c-1", "c", at.Add(time.Minute)),
		sb("c-late", "c", at.Add(2*time.Minute)),
		deleting,
		sb("other", "d", at, labelPool, "q"),
		sb("unnamed", "Not-A-Name", at),
	}
	got := map[string]string{}
	for name, s := range d.vms(all) {
		got[name] = s.ID
	}
	want := map[string]string{"a": "a-old", "b": "b-new", "c": "c-1"}
	if !maps.Equal(got, want) {
		t.Errorf("vms = %v, want %v", got, want)
	}
}

// A pool and a name are at most 57 characters together, so the sandbox's
// name fits Archil's 63.
func TestNameLimits(t *testing.T) {
	p32 := strings.Repeat("p", 32)
	d := testDriver(p32)
	if err := d.checkName(strings.Repeat("n", 57-32)); err != nil {
		t.Errorf("a name that fits: %v", err)
	}
	if err := d.checkName(strings.Repeat("n", 58-32)); err == nil {
		t.Error("a name one too long was taken")
	}
}

// A fence is read back as it was made, closing DNS where it denies every
// resolver, and a policy that allows by default is none.
func TestFences(t *testing.T) {
	for _, e := range []*machine.Egress{
		nil, {}, {Allow: []string{"example.com", "1.2.3.0/24"}},
		{Allow: []string{"example.com"}, CloseDNS: true},
	} {
		var p egressPolicy
		if q := policy(e); q != nil {
			raw, err := json.Marshal(q)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatal(err)
			}
		}
		if got := fenceOf(p); !sameFence(got, e) {
			t.Errorf("fence %v → policy → %v", e, got)
		}
	}
	var open egressPolicy
	_ = json.Unmarshal([]byte(`{"egress":{"default":"allow","deny":["x.com"]}}`), &open)
	if fenceOf(open) != nil {
		t.Error("a policy that allows by default read as a fence")
	}
	// A VM made before DNS could close: its fence leaves DNS open.
	var old egressPolicy
	_ = json.Unmarshal([]byte(`{"egress":{"default":"deny","allow":["example.com"]}}`), &old)
	if f := fenceOf(old); f == nil || f.CloseDNS {
		t.Errorf("an allow-list with no resolver denied read as %+v", f)
	}
	var half egressPolicy
	_ = json.Unmarshal([]byte(`{"egress":{"default":"deny","allow":["example.com"],"deny":["8.8.8.8"]}}`), &half)
	if f := fenceOf(half); f == nil || f.CloseDNS {
		t.Errorf("an allow-list with one resolver of two denied read as %+v", f)
	}
	if err := checkAllow(&machine.Egress{Allow: []string{"example.com", "8.8.8.8"}, CloseDNS: true}); err == nil {
		t.Error("a fence that closes DNS and allows a resolver was taken")
	}
}

// The hosts Archil adds to a fenced sandbox's policy for its mounts are not
// the fence's: a fence reads without them, a policy set for it keeps them
// where Archil put them, so a fence already held is not set again, a fence
// that names one is refused, and any other host is still a difference.
func TestMountHostsAreArchils(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want *machine.Egress
		// held: the policy is the fence's as the driver sets it, so is
		// not set again; rules as objects would be, as strings.
		held bool
	}{
		// As Archil held them, seen 2026-11-26.
		{`{"egress":{"default":"deny","allow":["example.com","mount.*.archil.com","data.*.archil.com"],"deny":["8.8.8.8","1.1.1.1"]}}`,
			&machine.Egress{Allow: []string{"example.com"}, CloseDNS: true}, true},
		{`{"egress":{"default":"deny","allow":["mount.*.archil.com","data.*.archil.com"],"deny":["8.8.8.8","1.1.1.1"]}}`,
			&machine.Egress{}, true},
		{`{"egress":{"default":"deny","allow":[{"target":"example.com"},{"target":"mount.*.archil.com"}]}}`,
			&machine.Egress{Allow: []string{"example.com"}}, false},
	} {
		var had egressPolicy
		if err := json.Unmarshal([]byte(c.raw), &had); err != nil {
			t.Fatal(err)
		}
		if got := fenceOf(had); !sameFence(got, c.want) {
			t.Errorf("%s reads as %+v, want %+v", c.raw, got, c.want)
		}
		if want := wantPolicy(had, c.want, false); c.held && !samePolicy(had, want) {
			t.Errorf("%s, holding %+v, would be set to %+v", c.raw, c.want, want.Egress)
		}
	}
	var other egressPolicy
	_ = json.Unmarshal([]byte(`{"egress":{"default":"deny","allow":["example.com","mount.*.archil.com","evil.example"],"deny":["8.8.8.8","1.1.1.1"]}}`), &other)
	if fence := (&machine.Egress{Allow: []string{"example.com"}, CloseDNS: true}); sameFence(fenceOf(other), fence) {
		t.Errorf("a policy that allows another host reads as %+v", fence)
	}
	for _, h := range mountHosts {
		if err := checkAllow(&machine.Egress{Allow: []string{"example.com", h}}); err == nil {
			t.Errorf("a fence that names %s was taken", h)
		}
	}
}

// The ingress mark goes on a policy alongside a fence, or on none, keeping
// the provider's default, and comes off with nothing else.
func TestIngressMark(t *testing.T) {
	fence := &machine.Egress{Allow: []string{"example.com"}, CloseDNS: true}
	for _, c := range []struct {
		name  string
		had   egressPolicy
		fence *machine.Egress
	}{
		{"none", egressPolicy{}, nil},
		{"a fence", *policy(fence), fence},
		{"a fork of a fenced VM", *policy(fence), nil},
	} {
		for _, off := range []bool{true, false} {
			want := wantPolicy(c.had, c.fence, off)
			if ingressOf(want) == off {
				t.Errorf("%s, ingress off %t: %+v", c.name, off, want.Egress)
			}
			if !sameFence(fenceOf(want), c.fence) {
				t.Errorf("%s, ingress off %t: fence %+v, want %+v", c.name, off, fenceOf(want), c.fence)
			}
			// Marked and unmarked again, the policy is as it would be
			// never marked.
			if !samePolicy(wantPolicy(want, c.fence, false), wantPolicy(c.had, c.fence, false)) {
				t.Errorf("%s: unmarking %+v leaves more", c.name, want.Egress)
			}
		}
	}
}

// fakeArchil serves the control plane's connections endpoint and a process
// API that runs one command: it answers started, echoes stdin to stdout,
// resends a frame from before the cursor, writes to stderr, and exits 3.
func fakeArchil(t *testing.T) *api {
	t.Helper()
	srv := fakeArchilServer(t, httptest.NewServer)
	return &api{base: srv.URL, keys: machine.StaticKey(machine.Key{Secret: "secret"}), http: srv.Client()}
}

// fakeArchilServer is fakeArchil's server, started by start: httptest's
// NewServer, or NewTLSServer for one whose process API is at a wss:// URL.
func fakeArchilServer(t *testing.T, start func(http.Handler) *httptest.Server) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sandboxes/sb1/connections":
			if r.Header.Get("Authorization") != "Bearer key-secret" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"success":false,"error":"no"}`)
				return
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]string{"url": "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"}}); err != nil {
				t.Error(err)
			}
		case "/ws":
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()
			ctx := r.Context()
			_, raw, err := c.Read(ctx)
			if err != nil {
				return
			}
			var req startRequest
			_ = json.Unmarshal(raw, &req)
			if req.Type != "start" || req.Command != "echo" {
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"error","error":"bad","message":"`+req.Command+`"}`))
				return
			}
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"started","process_id":"p1"}`))
			frame := func(stream byte, off uint64, b string) []byte {
				out := make([]byte, 9+len(b))
				out[0] = stream
				binary.BigEndian.PutUint64(out[1:9], off)
				copy(out[9:], b)
				return out
			}
			var in bytes.Buffer
			for {
				typ, data, err := c.Read(ctx)
				if err != nil {
					return
				}
				if typ == websocket.MessageBinary {
					in.Write(data)
					continue
				}
				if strings.Contains(string(data), "close_stdin") {
					break
				}
			}
			_ = c.Write(ctx, websocket.MessageBinary, frame(1, 0, in.String()))
			_ = c.Write(ctx, websocket.MessageBinary, frame(1, 0, in.String()))
			_ = c.Write(ctx, websocket.MessageBinary, frame(1, uint64(in.Len()), "!"))
			_ = c.Write(ctx, websocket.MessageBinary, frame(2, uint64(in.Len())+1, "oops"))
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"exit","status":"exited","exit_code":3}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProcess(t *testing.T) {
	a := fakeArchil(t)
	code, out, errOut, err := a.run(context.Background(), "sb1", "echo", "", strings.NewReader("hello"))
	if err != nil || code != 3 || string(out) != "hello!" || string(errOut) != "oops" {
		t.Fatalf("run = %d %q %q %v; want 3, hello! (the resent frame skipped), oops", code, out, errOut, err)
	}
	if _, _, _, err := a.run(context.Background(), "sb1", "other", "", nil); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("a refused start: %v", err)
	}
	a.keys = machine.StaticKey(machine.Key{Secret: "wrong"})
	if _, _, _, err := a.run(context.Background(), "sb1", "echo", "", nil); err == nil || !strings.Contains(err.Error(), machine.ErrUnauthorized.Error()) {
		t.Fatalf("a wrong key: %v, want ErrUnauthorized", err)
	}
}

// TestPipeBuffer: output past the limit waits for a reader, and an abort
// ends the wait.
func TestPipeBuffer(t *testing.T) {
	b := newPipeBuffer()
	b.write(make([]byte, pipeLimit))
	wrote := make(chan struct{})
	go func() { b.write([]byte("more")); close(wrote) }()
	select {
	case <-wrote:
		t.Fatal("a write past the limit did not wait")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := io.CopyN(io.Discard, b, pipeLimit); err != nil {
		t.Fatal(err)
	}
	<-wrote
	b.close()
	if rest, err := io.ReadAll(b); err != nil || string(rest) != "more" {
		t.Fatalf("read %q, %v", rest, err)
	}

	b = newPipeBuffer()
	b.write(make([]byte, pipeLimit))
	wrote = make(chan struct{})
	go func() { b.write([]byte("more")); close(wrote) }()
	b.abort()
	<-wrote
	if rest, err := io.ReadAll(b); err != nil || len(rest) != 0 {
		t.Fatalf("read %d bytes after an abort, %v", len(rest), err)
	}
}

// TestDiskPages: a listing follows nextCursor to the last page, and a name is
// passed on as Archil's filter.
func TestDiskPages(t *testing.T) {
	var names []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		names = append(names, r.URL.Query().Get("name"))
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = io.WriteString(w, `{"success":true,"data":[{"id":"dsk-1","name":"a"}],"nextCursor":"c2"}`)
		case "c2":
			_, _ = io.WriteString(w, `{"success":true,"data":[{"id":"dsk-2","name":"b"}]}`)
		default:
			http.Error(w, "bad cursor", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	a := &api{base: srv.URL, keys: machine.StaticKey(machine.Key{Driver: "archil", Secret: "k"}), http: srv.Client()}
	disks, err := a.disks(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 2 || disks[1].ID != "dsk-2" {
		t.Fatalf("listed %+v", disks)
	}
	if _, err := a.disks(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if names[len(names)-2] != "a" {
		t.Fatalf("names asked %q", names)
	}
}

// TestListingPages: the sandbox listing follows a nextCursor, at the
// envelope or in its data, to the last page, and a listing whose cursor
// comes back again fails rather than read short, as the disks' does. Its
// filter is sent with every page.
func TestListingPages(t *testing.T) {
	loop := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		if r.URL.Path == "/api/sandboxes" && r.URL.Query().Get("name_prefix") != "dvm-p--" {
			http.Error(w, "no filter", http.StatusBadRequest)
			return
		}
		if loop && cursor != "" {
			cursor = "again"
		}
		switch {
		case r.URL.Path == "/api/sandboxes" && cursor == "":
			_, _ = io.WriteString(w, `{"success":true,"data":{"sandboxes":[{"sandbox_id":"a","name":"a"}]},"nextCursor":"c2"}`)
		case r.URL.Path == "/api/sandboxes" && cursor == "c2":
			_, _ = io.WriteString(w, `{"success":true,"data":{"sandboxes":[{"sandbox_id":"b","name":"b"}],"nextCursor":"c3"}}`)
		case r.URL.Path == "/api/sandboxes" && cursor == "c3":
			_, _ = io.WriteString(w, `{"success":true,"data":{"sandboxes":[{"sandbox_id":"c","name":"c"}]}}`)
		case cursor == "again" && r.URL.Path == "/api/sandboxes":
			_, _ = io.WriteString(w, `{"success":true,"data":{"sandboxes":[]},"nextCursor":"again"}`)
		case r.URL.Path == "/api/disks" && loop:
			_, _ = io.WriteString(w, `{"success":true,"data":[],"nextCursor":"again"}`)
		default:
			http.Error(w, "bad cursor", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	a := &api{base: srv.URL, keys: machine.StaticKey(machine.Key{Driver: "archil", Secret: "k"}), http: srv.Client()}
	all, err := a.sandboxes(context.Background(), url.Values{"name_prefix": {"dvm-p--"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ID != "a" || all[2].ID != "c" {
		t.Fatalf("listed %+v", all)
	}
	loop = true
	if _, err := a.sandboxes(context.Background(), url.Values{"name_prefix": {"dvm-p--"}}); err == nil || !strings.Contains(err.Error(), "came back again") {
		t.Fatalf("a sandbox listing whose cursor repeats: %v", err)
	}
	if _, err := a.disks(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "came back again") {
		t.Fatalf("a disk listing whose cursor repeats: %v", err)
	}
}

// fakeCopy serves a process API that plays copyOutScript over blob: R, then
// a chunk for each count asked on stdin, as the script answers. With drop,
// the second chunk's bytes come after a gap in the offsets, as Archil sends
// output it dropped part of.
func fakeCopy(t *testing.T, blob []byte, drop bool, killed *atomic.Bool) *api {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sandboxes/sb1/connections":
			if err := json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]string{"url": "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"}}); err != nil {
				t.Error(err)
			}
		case "/ws":
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()
			ctx := r.Context()
			_, first, err := c.Read(ctx)
			if err != nil {
				return
			}
			if strings.Contains(string(first), `"kill"`) {
				killed.Store(true)
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"killed"}`))
				return
			}
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"started","process_id":"p1"}`))
			var cursor uint64
			send := func(b []byte) {
				out := make([]byte, 9+len(b))
				out[0] = 1
				binary.BigEndian.PutUint64(out[1:9], cursor)
				copy(out[9:], b)
				cursor += uint64(len(b))
				_ = c.Write(ctx, websocket.MessageBinary, out)
			}
			send([]byte("R"))
			var stdin bytes.Buffer
			pos, chunks := 0, 0
			for {
				typ, data, err := c.Read(ctx)
				if err != nil {
					return
				}
				if typ != websocket.MessageBinary {
					continue
				}
				stdin.Write(data)
				for {
					line, err := stdin.ReadString('\n')
					if err != nil {
						stdin.WriteString(line)
						break
					}
					var count int
					_, _ = fmt.Sscan(line, &count)
					chunk := blob[pos:min(pos+count, len(blob))]
					pos += len(chunk)
					chunks++
					send([]byte(fmt.Sprintf("%d\n", len(chunk))))
					if drop && chunks == 2 {
						cursor += 100
					}
					send(chunk)
					if len(chunk) < count {
						_ = c.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"type":"exit","status":"exited","exit_code":0,"cursor":%d}`, cursor)))
						return
					}
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &api{base: srv.URL, keys: machine.StaticKey(machine.Key{Secret: "secret"}), http: srv.Client()}
}

// TestCopyFrom: a copy out comes back whole over several chunks, including
// one whose size is a multiple of a chunk (its last chunk empty), and a
// chunk Archil dropped part of fails the copy rather than hang it, and
// kills the process, which never exited.
func TestCopyFrom(t *testing.T) {
	for _, size := range []int{10, 2*copyChunk + 100, 2 * copyChunk} {
		blob := make([]byte, size)
		for i := range blob {
			blob[i] = byte(i * 7)
		}
		var killed atomic.Bool
		m := newMachine(fakeCopy(t, blob, false, &killed), "sb1", "dev")
		rc, err := m.CopyFrom(context.Background(), "/x")
		if err != nil {
			t.Fatalf("%d bytes: %v", size, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || !bytes.Equal(got, blob) {
			t.Fatalf("%d bytes: read %d, %v", size, len(got), err)
		}
		if killed.Load() {
			t.Errorf("%d bytes: a copy that exited was killed", size)
		}
	}

	var killed atomic.Bool
	m := newMachine(fakeCopy(t, make([]byte, 3*copyChunk), true, &killed), "sb1", "dev")
	rc, err := m.CopyFrom(context.Background(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() { _, err := io.ReadAll(rc); read <- err }()
	select {
	case err := <-read:
		if err == nil || !strings.Contains(err.Error(), "dropped 100 bytes") {
			t.Fatalf("a dropped chunk: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a copy with a dropped chunk hung")
	}
	_ = rc.Close()
	if !killed.Load() {
		t.Error("a copy that ended at a gap, with no exit, was not killed in the guest")
	}
}

// TestUnavailable: a 502, 503, or 504, from Archil's REST API or its process
// API's handshake, is machine.ErrUnavailable; a 500 or a 429 is Archil
// answering, and is not.
func TestUnavailable(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusInternalServerError, false},
		{http.StatusTooManyRequests, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/sandboxes/sb1/connections" {
				_, _ = io.WriteString(w, `{"success":true,"data":{"url":"ws://`+r.Host+`/process"}}`)
				return
			}
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `{"success":false,"error":"down"}`)
		}))
		a := &api{base: srv.URL, keys: machine.StaticKey(machine.Key{Driver: "archil", Secret: "k"}), http: srv.Client()}
		_, err := a.sandboxes(context.Background(), nil)
		if err == nil || errors.Is(err, machine.ErrUnavailable) != tc.want {
			t.Errorf("REST %d: %v, want ErrUnavailable %v", tc.status, err, tc.want)
		}
		_, err = a.dialProcess(context.Background(), "sb1")
		if err == nil || errors.Is(err, machine.ErrUnavailable) != tc.want {
			t.Errorf("process API handshake %d: %v, want ErrUnavailable %v", tc.status, err, tc.want)
		}
		srv.Close()
	}
}

// TestLabels: every sandbox the driver makes is labeled with its pool, its
// VM, and the VM it was copied from (a fork with its own labels, never its
// source's), so a listing by the pool's label finds exactly the pool's VMs,
// and a listing reports a copy's From; every listing asks Archil for the
// pool's names alone.
func TestLabels(t *testing.T) {
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	p, q := f.driver(t, "p", "debian:sid"), f.driver(t, "q", "debian:sid")
	a, err := p.Create(ctx, machine.InstanceSpec{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Create(ctx, machine.InstanceSpec{Name: "b", From: &a})
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Create(ctx, machine.InstanceSpec{Name: "c", Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	if c, err = p.Upgrade(ctx, c, &a); err != nil {
		t.Fatal(err)
	}
	other, err := q.Create(ctx, machine.InstanceSpec{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]string{
		"dvm-p--a": {labelPool: "p", labelInstance: "a"},
		"dvm-p--b": {labelPool: "p", labelInstance: "b", labelFrom: "a"},
		"dvm-q--a": {labelPool: "q", labelInstance: "a"},
	}
	f.mu.Lock()
	for _, s := range f.sandboxes {
		if s.status == "deleted" {
			continue
		}
		if s.id == c.ID {
			// The upgrade's new sandbox, under a name of its own, names
			// the one it replaced.
			if !strings.HasPrefix(s.name, "dvm-p--c--") || s.labels[labelReplaces] == "" || s.labels[labelInstance] != "c" || s.labels[labelFrom] != "a" {
				t.Errorf("the upgraded sandbox %s is labeled %v", s.name, s.labels)
			}
			continue
		}
		if !maps.Equal(s.labels, want[s.name]) {
			t.Errorf("sandbox %s is labeled %v, want %v", s.name, s.labels, want[s.name])
		}
	}
	f.mu.Unlock()

	listed, err := f.driver(t, "p", "").Instances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	from := map[string]string{}
	for _, inst := range listed {
		from[inst.Name] = inst.From
	}
	if !maps.Equal(from, map[string]string{"a": "", "b": "a", "c": "a"}) {
		t.Errorf("listed with From %v", from)
	}

	// A pool's VMs, found by label, are the ones the driver lists.
	for d, insts := range map[*Driver][]machine.Instance{p: {a, b, c}, q: {other}} {
		byLabel, err := d.api.sandboxes(ctx, url.Values{"label": {labelPool + "=" + d.pool}})
		if err != nil {
			t.Fatal(err)
		}
		var got, wantIDs []string
		for _, sb := range byLabel {
			got = append(got, sb.ID)
		}
		for _, inst := range insts {
			wantIDs = append(wantIDs, inst.ID)
		}
		slices.Sort(got)
		slices.Sort(wantIDs)
		if !slices.Equal(got, wantIDs) {
			t.Errorf("pool %s by label: %v, want %v", d.pool, got, wantIDs)
		}
	}

	f.mu.Lock()
	for _, l := range f.listings {
		if !l.Has("label") && !strings.HasPrefix(l.Get("name"), "dvm-") && !strings.HasPrefix(l.Get("name_prefix"), "dvm-") {
			t.Errorf("a listing of every sandbox in the account: %v", l)
		}
	}
	f.mu.Unlock()
	for _, inst := range []machine.Instance{b, c, a} {
		if err := p.Destroy(ctx, inst); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Destroy(ctx, other); err != nil {
		t.Fatal(err)
	}
}

// TestUnlabeledSandboxesAreNoVMs: a sandbox is a VM by its labels alone
// (ADR 26-11-24-905 §1): one named as a VM's sandbox, with no labels, is
// not listed, and its name does not make a create of the VM's name ErrExists
// before Archil refuses it.
func TestUnlabeledSandboxesAreNoVMs(t *testing.T) {
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "p", "debian:sid")
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "old"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.sandboxes[0].labels = nil
	f.mu.Unlock()
	if got, err := d.Instance(ctx, "old"); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("an unlabeled sandbox: %+v, %v; want no VM", got, err)
	}
	if all, err := d.Instances(ctx); err != nil || len(all) != 0 {
		t.Fatalf("instances with an unlabeled sandbox: %+v, %v; want none", all, err)
	}
}
