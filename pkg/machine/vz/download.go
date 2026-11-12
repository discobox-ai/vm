package vz

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// progressEvery is how often a download logs how far it has got.
var progressEvery = 30 * time.Second

// downloadResumable downloads url into dst through rt, the driver's
// transport (machine.Config.HTTPTransport), so an embedder's dialer dials it.
// It writes to dst+".download" and renames that into place only once it is
// complete, so a dst that exists is whole. A partial file left by an
// interrupted download is resumed with a range request; a server that
// answers the range with the whole body has the partial file started again,
// and one that finds the range past the end has it checked as complete.
func downloadResumable(ctx context.Context, rt http.RoundTripper, url, dst string, log io.Writer) error {
	partial := dst + ".download"
	//nolint:gosec // G304: the driver's own cache path.
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	client := &http.Client{Transport: rt}
	// A second attempt follows only a range the server refused while the
	// partial file was not whole, from the start.
	for attempt := 0; ; attempt++ {
		done, err := fetch(ctx, client, url, f, log)
		if err != nil {
			return err
		}
		if done {
			break
		}
		if attempt > 0 {
			return fmt.Errorf("download %s: the server refused a request for the whole file", url)
		}
		if err := f.Truncate(0); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(partial, dst)
}

// fetch requests what f lacks of url and writes it to f. It reports false,
// and writes nothing, when the server refused the range and f is not the
// whole file, which the caller starts again.
func fetch(ctx context.Context, client *http.Client, url string, f *os.File, log io.Writer) (bool, error) {
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	have := info.Size()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()

	var total int64
	switch resp.StatusCode {
	case http.StatusOK:
		// The whole file, a range asked for or not.
		if have > 0 {
			fmt.Fprintf(log, "vz: the server sent the whole file, not the rest from %d MiB; downloading again\n", have>>20)
		}
		if err := f.Truncate(0); err != nil {
			return false, err
		}
		have, total = 0, resp.ContentLength
	case http.StatusPartialContent:
		start, size, err := contentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return false, fmt.Errorf("download %s: %w", url, err)
		}
		if start != have {
			return false, fmt.Errorf("download %s: asked for the rest from byte %d, sent from %d", url, have, start)
		}
		total = size
		fmt.Fprintf(log, "vz: resuming from %d MiB\n", have>>20)
	case http.StatusRequestedRangeNotSatisfiable:
		// Past the end: the partial file is whole if it is as long as the
		// server says the file is.
		if _, size, err := contentRange(resp.Header.Get("Content-Range")); err == nil && size == have {
			return true, nil
		}
		fmt.Fprintf(log, "vz: the server refused the rest from %d MiB; downloading again\n", have>>20)
		return false, nil
	default:
		return false, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	if _, err := f.Seek(have, io.SeekStart); err != nil {
		return false, err
	}
	w := &progressWriter{w: f, n: have, total: total, log: log, next: time.Now().Add(progressEvery)}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return false, fmt.Errorf("download %s: %w", url, err)
	}
	if total >= 0 && w.n != total {
		return false, fmt.Errorf("download %s: got %d bytes of %d", url, w.n, total)
	}
	return true, nil
}

// contentRange reads a Content-Range header's first byte and the file's
// size: "bytes 100-199/200", or "bytes */200" (the start is then -1). An
// unknown size ("/*") is -1.
func contentRange(h string) (start, size int64, err error) {
	spec, ok := strings.CutPrefix(h, "bytes ")
	if !ok {
		return 0, 0, fmt.Errorf("content range %q: not bytes", h)
	}
	rng, sz, ok := strings.Cut(spec, "/")
	if !ok {
		return 0, 0, fmt.Errorf("content range %q: no size", h)
	}
	size = -1
	if sz != "*" {
		if size, err = strconv.ParseInt(sz, 10, 64); err != nil {
			return 0, 0, fmt.Errorf("content range %q: %w", h, err)
		}
	}
	start = -1
	if rng != "*" {
		first, _, ok := strings.Cut(rng, "-")
		if !ok {
			return 0, 0, fmt.Errorf("content range %q: no range", h)
		}
		if start, err = strconv.ParseInt(first, 10, 64); err != nil {
			return 0, 0, fmt.Errorf("content range %q: %w", h, err)
		}
	}
	if start < 0 && size < 0 {
		return 0, 0, fmt.Errorf("content range %q: no range and no size", h)
	}
	return start, size, nil
}

// progressWriter counts what is written through it and logs how far a
// download has got every progressEvery.
type progressWriter struct {
	w        io.Writer
	n, total int64
	log      io.Writer
	next     time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.n += int64(n)
	if now := time.Now(); !now.Before(p.next) {
		p.next = now.Add(progressEvery)
		if p.total > 0 {
			fmt.Fprintf(p.log, "vz: downloaded %.0f%% (%d MiB)\n", float64(p.n)*100/float64(p.total), p.n>>20)
		} else {
			fmt.Fprintf(p.log, "vz: downloaded %d MiB\n", p.n>>20)
		}
	}
	return n, err
}
