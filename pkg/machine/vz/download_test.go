package vz

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
)

// restoreServer is a stand-in for Apple's restore image host: it serves
// image, honoring a range unless ignoreRange, and records each request's
// Range header.
type restoreServer struct {
	*httptest.Server
	image       []byte
	ignoreRange bool
	mu          sync.Mutex
	ranges      []string
}

func newRestoreServer(t *testing.T, image []byte) *restoreServer {
	t.Helper()
	s := &restoreServer{image: image}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.ranges = append(s.ranges, r.Header.Get("Range"))
		ignore := s.ignoreRange
		s.mu.Unlock()
		if r.URL.Path != "/UniversalMac_27.0_26A428_Restore.ipsw" {
			http.NotFound(w, r)
			return
		}
		if ignore {
			r.Header.Del("Range")
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(s.image))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *restoreServer) url() string { return s.URL + "/UniversalMac_27.0_26A428_Restore.ipsw" }

func (s *restoreServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...)
}

// dialRecorder is an embedder's dialer that records each address it is given
// and refuses all of them when refuse is set.
type dialRecorder struct {
	mu     sync.Mutex
	addrs  []string
	refuse bool
}

func (d *dialRecorder) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	refuse := d.refuse
	d.mu.Unlock()
	if refuse {
		return nil, errors.New("refused by the embedder")
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, addr)
}

func (d *dialRecorder) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

func testImage() []byte {
	b := make([]byte, 3<<20+17)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func TestRestoreImageDownloadDialsThroughConfigDial(t *testing.T) {
	image := testImage()
	srv := newRestoreServer(t, image)
	rec := &dialRecorder{}
	transport := machine.Config{Dial: rec.dial}.HTTPTransport()
	dst := filepath.Join(t.TempDir(), "UniversalMac_27.0_26A428_Restore.ipsw")

	if err := downloadResumable(context.Background(), transport, srv.url(), dst, io.Discard); err != nil {
		t.Fatal(err)
	}
	assertFile(t, dst, image)
	if _, err := os.Stat(dst + ".download"); !os.IsNotExist(err) {
		t.Fatalf("the partial file is left behind: %v", err)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	if got := rec.dialed(); len(got) == 0 || got[0] != host {
		t.Fatalf("dialed %q, want the server's own address %s", got, host)
	}
	if got := srv.requests(); len(got) != 1 || got[0] != "" {
		t.Fatalf("requests' ranges %q, want one request with none", got)
	}
}

func TestRestoreImageDownloadTheDialerCanRefuse(t *testing.T) {
	srv := newRestoreServer(t, testImage())
	rec := &dialRecorder{refuse: true}
	dst := filepath.Join(t.TempDir(), "image.ipsw")
	err := downloadResumable(context.Background(), machine.Config{Dial: rec.dial}.HTTPTransport(), srv.url(), dst, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "refused by the embedder") {
		t.Fatalf("got %v, want the dialer's refusal", err)
	}
	if len(srv.requests()) != 0 {
		t.Fatal("the server was reached past the dialer")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("an image is in place after a refused download: %v", err)
	}
}

func TestRestoreImageDownloadResumes(t *testing.T) {
	image := testImage()
	srv := newRestoreServer(t, image)
	dst := filepath.Join(t.TempDir(), "image.ipsw")
	half := int64(len(image) / 2)
	writeFile(t, dst+".download", image[:half])

	var log bytes.Buffer
	if err := downloadResumable(context.Background(), machine.Config{}.HTTPTransport(), srv.url(), dst, &log); err != nil {
		t.Fatal(err)
	}
	assertFile(t, dst, image)
	if got := srv.requests(); len(got) != 1 || got[0] != fmt.Sprintf("bytes=%d-", half) {
		t.Fatalf("requests' ranges %q, want one from byte %d", got, half)
	}
	if !strings.Contains(log.String(), "resuming") {
		t.Fatalf("log %q does not say it resumed", log.String())
	}
}

func TestRestoreImageDownloadStartsAgainWhenTheRangeIsIgnored(t *testing.T) {
	image := testImage()
	srv := newRestoreServer(t, image)
	srv.ignoreRange = true
	dst := filepath.Join(t.TempDir(), "image.ipsw")
	// Not a prefix of the image: appending the whole body to it, as the vz
	// library does, would leave it corrupt.
	writeFile(t, dst+".download", bytes.Repeat([]byte{0xff}, 4096))

	if err := downloadResumable(context.Background(), machine.Config{}.HTTPTransport(), srv.url(), dst, io.Discard); err != nil {
		t.Fatal(err)
	}
	assertFile(t, dst, image)
}

func TestRestoreImageDownloadOfAWholePartialFile(t *testing.T) {
	image := testImage()
	srv := newRestoreServer(t, image)
	dst := filepath.Join(t.TempDir(), "image.ipsw")
	writeFile(t, dst+".download", image)

	if err := downloadResumable(context.Background(), machine.Config{}.HTTPTransport(), srv.url(), dst, io.Discard); err != nil {
		t.Fatal(err)
	}
	assertFile(t, dst, image)
	if got := srv.requests(); len(got) != 1 {
		t.Fatalf("requests' ranges %q, want one", got)
	}
}

func TestRestoreImageDownloadStartsAgainFromAPartialFileTooLong(t *testing.T) {
	image := testImage()
	srv := newRestoreServer(t, image)
	dst := filepath.Join(t.TempDir(), "image.ipsw")
	writeFile(t, dst+".download", append(append([]byte(nil), image...), 1, 2, 3))

	if err := downloadResumable(context.Background(), machine.Config{}.HTTPTransport(), srv.url(), dst, io.Discard); err != nil {
		t.Fatal(err)
	}
	assertFile(t, dst, image)
	if got := srv.requests(); len(got) != 2 || got[0] == "" || got[1] != "" {
		t.Fatalf("requests' ranges %q, want a refused range then the whole file", got)
	}
}

func TestRestoreImageDownloadRefusesAnErrorStatus(t *testing.T) {
	srv := newRestoreServer(t, testImage())
	dst := filepath.Join(t.TempDir(), "image.ipsw")
	err := downloadResumable(context.Background(), machine.Config{}.HTTPTransport(), srv.URL+"/missing.ipsw", dst, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("got %v, want a 404", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("an image is in place after a failed download: %v", err)
	}
}

func TestRestoreImageDownloadKeepsWhatATruncatedBodyGot(t *testing.T) {
	image := testImage()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Promise the whole image, send half, and hang up.
		w.Header().Set("Content-Length", strconv.Itoa(len(image)))
		_, _ = w.Write(image[:len(image)/2])
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)
	dst := filepath.Join(t.TempDir(), "image.ipsw")
	if err := downloadResumable(context.Background(), machine.Config{}.HTTPTransport(), srv.URL, dst, io.Discard); err == nil {
		t.Fatal("a truncated download succeeded")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("an image is in place after a truncated download: %v", err)
	}
	got, err := os.ReadFile(dst + ".download")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || !bytes.Equal(got, image[:len(got)]) {
		t.Fatalf("the partial file holds %d bytes that are not the image's start", len(got))
	}
}

func TestContentRange(t *testing.T) {
	for _, tc := range []struct {
		in          string
		start, size int64
		bad         bool
	}{
		{in: "bytes 100-199/200", start: 100, size: 200},
		{in: "bytes 0-0/*", start: 0, size: -1},
		{in: "bytes */200", start: -1, size: 200},
		{in: "bytes */*", bad: true},
		{in: "items 0-1/2", bad: true},
		{in: "bytes 0-1", bad: true},
		{in: "bytes x-1/2", bad: true},
		{in: "", bad: true},
	} {
		start, size, err := contentRange(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("%q: no error", tc.in)
			}
			continue
		}
		if err != nil || start != tc.start || size != tc.size {
			t.Errorf("%q: %d, %d, %v; want %d, %d", tc.in, start, size, err, tc.start, tc.size)
		}
	}
}

func writeFile(t *testing.T, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(name, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, name string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: %d bytes that are not the image's %d", filepath.Base(name), len(got), len(want))
	}
}
