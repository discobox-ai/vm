package archil

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// A VM's sandbox and data volume are named for it, and a sandbox of another
// pool, of an earlier driver, or not a VM's, is none.
func TestNames(t *testing.T) {
	d := testDriver("p1")
	for _, name := range []string{"dev", "a-b-c"} {
		if got := d.nameOf(d.sandboxName(name)); got != name {
			t.Errorf("nameOf(%s) = %q", d.sandboxName(name), got)
		}
		if got := testDriver("p").nameOf(d.sandboxName(name)); got != "" {
			t.Errorf("pool p reads p1's %s as %q", d.sandboxName(name), got)
		}
		if got := d.nameOf(d.dataDiskName(name)); got != "" {
			t.Errorf("the data volume %s read as VM %q", d.dataDiskName(name), got)
		}
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
	for _, sb := range []string{"dvm-p1-0a1b2c3d", "dvm-p1--i--dev--0a1b2c3d", "dvm-p1--", "other"} {
		if got := d.nameOf(sb); got != "" {
			t.Errorf("%s read as VM %q", sb, got)
		}
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

func TestFences(t *testing.T) {
	for _, e := range []*machine.Egress{nil, {}, {Allow: []string{"example.com", "1.2.3.0/24"}}} {
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
}

// fakeArchil serves the control plane's connections endpoint and a process
// API that runs one command: it answers started, echoes stdin to stdout,
// resends a frame from before the cursor, writes to stderr, and exits 3.
func fakeArchil(t *testing.T) *api {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sandboxes/sb1/connections":
			if r.Header.Get("Authorization") != "Bearer key-secret" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"success":false,"error":"no"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]string{"url": "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"}})
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
	return &api{base: srv.URL, keys: machine.StaticKey(machine.Key{Secret: "secret"}), http: srv.Client()}
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
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]string{"url": "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"}})
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
