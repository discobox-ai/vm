package converge

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
)

// Guest is what a run needs of a guest, which every machine offers: exec and
// copy. *guest.Client is one.
type Guest interface {
	Info(ctx context.Context) (guest.Info, error)
	Run(ctx context.Context, req guest.ExecRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	CopyTo(ctx context.Context, dir string, tarStream io.Reader) error
}

// Options is one run.
type Options struct {
	// Paths are the state files to apply: each a NAME.yaml, or a directory
	// of them.
	Paths []string
	// Out receives what the run does and what the guest's tools print.
	Out io.Writer
	// Lease is how long the guest stays locked to this run after its last
	// heartbeat, which is sent every half of it. It defaults to a minute.
	Lease time.Duration
	// Holder names this client in the lease, for the client that finds the
	// guest locked. It defaults to the host's name and this process's ID.
	Holder string
}

// DefaultLease is how long a guest stays locked to a run that has stopped
// sending heartbeats: long enough to ride out a slow exec, short enough that
// a client that died does not hold a guest for long.
const DefaultLease = time.Minute

// ErrLeaseHeld is a run that could not have the guest, or lost it: another
// client holds the lease.
var ErrLeaseHeld = errors.New("the guest is leased to another client")

// leaseHeldExit is how a lease script says so.
const leaseHeldExit = 75

// The states a guest is in.
const (
	Converged  = "converged"
	Converging = "converging"
	Failed     = "failed"
)

// Status is where a guest is, as its last run left it. It is kept in the
// guest, so any client can ask.
type Status struct {
	State string `json:"state"`
	// Files is the digest of each state file the guest has been converged
	// to, by name.
	Files map[string]string `json:"files,omitempty"`
	// Resources is how many the last run's state files declare, and Done how
	// many of them the guest has.
	Resources int `json:"resources"`
	Done      int `json:"done"`
	// Current is what was being applied when the run failed.
	Current string `json:"current,omitempty"`
	Error   string `json:"error,omitempty"`
	// Holder is the client that ran it.
	Holder   string     `json:"holder,omitempty"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
}

// applied is what the guest's record holds for one resource: the digest it
// was applied at, and the state file that declared it.
type applied struct {
	Digest string `json:"digest"`
	File   string `json:"file"`
}

const (
	statusName = "status.json"
	recordName = "record.json"
	ledgerName = "ledger"
)

// session is one client's hold on one guest.
type session struct {
	guest Guest
	shell dialect
	// root is the directory a guest that is a directory lives under.
	root string
	// dir is where the guest keeps the record, the status, and the lease, as
	// a guest path. A process in the guest sees it at host(dir).
	dir    string
	out    io.Writer
	holder string
	lease  time.Duration
}

func open(ctx context.Context, g Guest, out io.Writer) (*session, error) {
	info, err := g.Info(ctx)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = io.Discard
	}
	shell := dialectFor(info.OS)
	return &session{guest: g, shell: shell, root: info.Root, dir: shell.stateDir(), out: out}, nil
}

// host is a guest path as a process in the guest sees it, which differs only
// when the guest is a directory.
func (s *session) host(path string) string { return guest.Resolve(s.root, path) }

// ledger is where a guest that is a directory writes down the packages,
// services, and owners it is asked for, instead of changing the host it is
// on. A real guest has none.
func (s *session) ledger() string {
	if s.root == "" {
		return ""
	}
	return s.host(s.shell.join(s.dir, ledgerName))
}

// ReadStatus reads a guest's status. A guest no run has touched has none,
// and its State is empty.
func ReadStatus(ctx context.Context, g Guest) (Status, error) {
	var status Status
	s, err := open(ctx, g, nil)
	if err != nil {
		return status, err
	}
	return status, s.read(ctx, statusName, &status)
}

// Run makes a guest match state files. It reads them, and the files they
// name, on this side; takes a lease on the guest and keeps it alive; and
// applies, through exec and copy, each resource whose digest is not the one
// the guest's record holds. It returns the first failure, which the guest's
// status holds too.
func Run(ctx context.Context, g Guest, opts Options) error {
	// Before the guest is touched: state files that do not parse change
	// nothing.
	desired, err := Load(opts.Paths...)
	if err != nil {
		return err
	}
	s, err := open(ctx, g, opts.Out)
	if err != nil {
		return err
	}
	s.lease, s.holder = opts.Lease, opts.Holder
	if s.lease <= 0 {
		s.lease = DefaultLease
	}
	if s.holder == "" {
		name, _ := os.Hostname()
		s.holder = fmt.Sprintf("%s-%d", name, os.Getpid())
	}
	// Two runs of one client are two holders.
	s.holder = strings.Join(strings.Fields(s.holder), "-") + "-" + randomID()[:6]

	if err := s.script(ctx, s.shell.acquire(s.host(s.dir), s.holder, s.lease), nil, s.out, s.out); err != nil {
		return err
	}
	// The lease outlives a run only by its timeout, so everything from here
	// ends by giving it back, whatever became of ctx.
	run, cancel := context.WithCancelCause(ctx)
	var beating sync.WaitGroup
	beating.Go(func() { s.heartbeat(run, cancel) })
	status, err := s.converge(run, desired)
	if cause := context.Cause(run); err != nil && cause != nil && ctx.Err() == nil {
		err = cause
	}
	cancel(nil)
	beating.Wait()

	now := time.Now().UTC()
	status.Finished = &now
	status.State = Converged
	if err != nil {
		status.State, status.Error = Failed, err.Error()
	}
	// A run that lost the lease leaves the status to the client that has it.
	if !errors.Is(err, ErrLeaseHeld) {
		closing, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer stop()
		if finishErr := s.finish(closing, status); err == nil {
			err = finishErr
		}
	}
	return err
}

// heartbeat renews the lease every half of it, and ends the run once the
// guest says another client has it or has not answered for a whole lease:
// past that, another client may be converging the guest.
func (s *session) heartbeat(ctx context.Context, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(s.lease / 2)
	defer ticker.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// Not ctx: a heartbeat the run's end cut short would leave its
		// script behind in the guest. The run waits out one beat instead.
		beat, stop := context.WithTimeout(context.WithoutCancel(ctx), s.lease/2)
		err := s.script(beat, s.shell.renew(s.host(s.dir), s.holder, s.lease), nil, io.Discard, io.Discard)
		stop()
		switch {
		case err == nil:
			last = time.Now()
		case ctx.Err() != nil:
			return
		case errors.Is(err, ErrLeaseHeld):
			cancel(err)
			return
		case time.Since(last) > s.lease:
			cancel(fmt.Errorf("%w: no heartbeat reached it for %s: %w", ErrLeaseHeld, s.lease, err))
			return
		}
	}
}

// converge applies the difference between the state files and the guest's
// record, one state file after another.
func (s *session) converge(ctx context.Context, desired *Desired) (Status, error) {
	status := Status{State: Converging, Holder: s.holder, Started: time.Now().UTC(), Files: map[string]string{}}
	var previous Status
	if err := s.read(ctx, statusName, &previous); err != nil {
		return status, err
	}
	maps.Copy(status.Files, previous.Files)
	record := map[string]applied{}
	if err := s.read(ctx, recordName, &record); err != nil {
		return status, err
	}

	given := map[string]bool{}
	declared := map[string]bool{}
	for _, file := range desired.Files {
		given[file.Name] = true
		status.Resources += len(file.Resources)
		// Not converged to it until the run says so.
		delete(status.Files, file.Name)
		for _, r := range file.Resources {
			declared[r.ID] = true
		}
	}
	if err := s.write(ctx, statusName, status); err != nil {
		return status, err
	}

	for _, file := range desired.Files {
		var pending []Resource
		changed := false
		for _, r := range file.Resources {
			was, ok := record[r.ID]
			switch {
			case ok && was.File > file.Name && !given[was.File]:
				// A later state file, not given to this run, declared it
				// too, and its declaration is the resource.
				status.Done++
			case ok && was.Digest == r.Digest:
				if was.File != file.Name {
					record[r.ID], changed = applied{Digest: r.Digest, File: file.Name}, true
				}
				status.Done++
			default:
				pending = append(pending, r)
			}
		}
		// What left this state file goes first.
		var gone []string
		for _, id := range slices.Sorted(maps.Keys(record)) {
			if record[id].File == file.Name && !declared[id] {
				gone = append(gone, id)
			}
		}
		if len(gone) > 0 {
			status.Current = file.Name + ": removals"
			if err := s.remove(ctx, gone); err != nil {
				return status, fmt.Errorf("%s: %w", status.Current, err)
			}
			for _, id := range gone {
				delete(record, id)
			}
			changed = true
		}
		for _, batch := range batches(file.Name, pending) {
			if err := ctx.Err(); err != nil {
				return status, s.save(ctx, record, changed, err)
			}
			status.Current = batch.label
			fmt.Fprintf(s.out, "--> %s\n", batch.label)
			done, err := s.apply(ctx, batch)
			for _, r := range done {
				record[r.ID], changed = applied{Digest: r.Digest, File: file.Name}, true
			}
			status.Done += len(done)
			if err != nil {
				return status, s.save(ctx, record, changed, fmt.Errorf("%s: %w", batch.label, err))
			}
			// After every batch, so a run that dies here has lost nothing
			// it applied.
			if err := s.save(ctx, record, changed, nil); err != nil {
				return status, err
			}
			changed = false
		}
		if err := s.save(ctx, record, changed, nil); err != nil {
			return status, err
		}
		status.Files[file.Name] = file.Digest
	}
	status.Current = ""
	return status, nil
}

// save writes the record if it changed, and returns err, or the failure to
// write when there was none. A run that is ending for a failure still writes
// what it applied.
func (s *session) save(ctx context.Context, record map[string]applied, changed bool, err error) error {
	if !changed {
		return err
	}
	if err != nil {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer stop()
	}
	if writeErr := s.write(ctx, recordName, record); err == nil {
		return writeErr
	}
	return err
}

// batch is resources applied by one exec in the guest: a state file's files,
// a run of one provider's packages, a run of its services, or one script.
type batch struct {
	label     string
	resources []Resource
}

func batches(file string, pending []Resource) []batch {
	var out []batch
	for _, r := range pending {
		last := len(out) - 1
		joins := last >= 0 && out[last].resources[0].Kind == r.Kind &&
			(r.Kind == KindFile || r.Kind == KindService || (r.Kind == KindPackage && out[last].resources[0].Provider == r.Provider))
		if !joins {
			out = append(out, batch{})
			last++
		}
		out[last].resources = append(out[last].resources, r)
	}
	for i, b := range out {
		first := b.resources[0]
		switch {
		case first.Kind == KindPackage:
			names := make([]string, len(b.resources))
			for j, r := range b.resources {
				names[j] = r.Package
			}
			out[i].label = KindPackage + ":" + first.Provider + "/" + strings.Join(names, ",")
		case len(b.resources) == 1:
			out[i].label = first.ID
		default:
			out[i].label = fmt.Sprintf("%s: %d %ss", file, len(b.resources), first.Kind)
		}
	}
	return out
}

// apply applies a batch, and returns the resources that were applied, which
// is all of them unless it failed part of the way through.
func (s *session) apply(ctx context.Context, b batch) ([]Resource, error) {
	first := b.resources[0]
	switch first.Kind {
	case KindFile:
		return s.applyFiles(ctx, b.resources)
	case KindPackage:
		names := make([]string, len(b.resources))
		for i, r := range b.resources {
			names[i] = r.Package
		}
		body, err := s.shell.packages(first.Provider, names, s.ledger())
		if err != nil {
			return nil, err
		}
		if err := s.script(ctx, body, nil, s.out, s.out); err != nil {
			return nil, err
		}
		return b.resources, nil
	case KindExec:
		argv, script := first.Exec.Shell, first.Exec.Run
		if len(argv) == 0 {
			argv, script = s.shell.execShell(), s.shell.execPrelude()+script
		}
		tail := &tailWriter{}
		out := io.MultiWriter(s.out, tail)
		code, err := s.guest.Run(ctx, guest.ExecRequest{Argv: append(slices.Clone(argv), script)}, nil, out, out)
		if err != nil {
			return nil, err
		}
		if code != 0 {
			return nil, exitError(code, tail)
		}
		return b.resources, nil
	case KindService:
		var body strings.Builder
		for i, r := range b.resources {
			body.WriteString(s.shell.service(i, r.ID, r.Unit, r.UnitState))
		}
		return s.applyMarked(ctx, s.shell.services(body.String(), s.ledger()), nil, b.resources)
	default:
		return nil, fmt.Errorf("no resource kind %q", first.Kind)
	}
}

// applyFiles copies a batch of files into the guest beside the script that
// puts each one at its path, and runs it.
func (s *session) applyFiles(ctx context.Context, resources []Resource) ([]Resource, error) {
	var body strings.Builder
	var files []payload
	for i, r := range resources {
		path := s.host(r.File.Path)
		mode := strconv.FormatUint(uint64(r.Mode.Perm()), 8)
		switch {
		case r.File.Directory:
			body.WriteString(s.shell.directory(i, r.ID, mode, r.File.Owner, path))
		case r.File.Link != "":
			body.WriteString(s.shell.link(i, r.ID, r.File.Owner, r.File.Link, path))
		default:
			staged := "f" + strconv.Itoa(i)
			if r.SourcePath != "" {
				files = append(files, payload{name: staged, source: r.SourcePath})
			} else {
				files = append(files, payload{name: staged, data: []byte(*r.File.Content)})
			}
			body.WriteString(s.shell.file(i, r.ID, mode, r.File.Owner, staged, path))
		}
	}
	return s.applyMarked(ctx, s.shell.files(body.String(), s.ledger()), files, resources)
}

// applyMarked runs a script that prints a marker as it finishes each
// resource, so a script that fails has still applied the ones before.
func (s *session) applyMarked(ctx context.Context, body string, files []payload, resources []Resource) ([]Resource, error) {
	var done []Resource
	stdout := &markerWriter{out: s.out, done: func(n int) {
		if n >= 0 && n < len(resources) {
			done = append(done, resources[n])
		}
	}}
	err := s.script(ctx, body, files, stdout, s.out)
	stdout.flush()
	return done, err
}

// remove deletes the files that left a state file. A package or a service
// that left is only forgotten.
func (s *session) remove(ctx context.Context, ids []string) error {
	var body strings.Builder
	for _, id := range ids {
		if path, ok := strings.CutPrefix(id, KindFile+":"); ok {
			fmt.Fprintf(s.out, "--> remove %s\n", id)
			body.WriteString(s.shell.gone(s.host(path)))
		}
	}
	if body.Len() == 0 {
		return nil
	}
	return s.script(ctx, s.shell.files(body.String(), s.ledger()), nil, s.out, s.out)
}

// finish writes the status and gives the lease back, in one exec.
func (s *session) finish(ctx context.Context, status Status) error {
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	dir := s.host(s.dir)
	body := s.shell.install(statusName, s.shell.join(dir, statusName)) + s.shell.release(dir, s.holder)
	return s.script(ctx, body, []payload{{name: statusName, data: append(data, '\n')}}, s.out, s.out)
}

// read reads one of the guest's JSON files into v, and leaves v alone when
// the guest has none.
func (s *session) read(ctx context.Context, name string, v any) error {
	var data bytes.Buffer
	if err := s.script(ctx, s.shell.read(s.shell.join(s.host(s.dir), name)), nil, &data, s.out); err != nil {
		return err
	}
	if len(bytes.TrimSpace(data.Bytes())) == 0 {
		return nil
	}
	if err := json.Unmarshal(data.Bytes(), v); err != nil {
		return fmt.Errorf("the guest's %s: %w", name, err)
	}
	return nil
}

// write replaces one of the guest's JSON files, in one rename.
func (s *session) write(ctx context.Context, name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	body := s.shell.install(name, s.shell.join(s.host(s.dir), name))
	return s.script(ctx, body, []payload{{name: name, data: append(data, '\n')}}, s.out, s.out)
}

// payload is a file copied into the guest beside a script: data, or a file
// on this side, which is streamed.
type payload struct {
	name   string
	data   []byte
	source string
}

// script runs a script in the guest: it copies the script, and the files it
// works on, into a directory of their own, which the script removes as it
// ends, and runs it with the guest's shell. A script is a file, not an
// argument, because a batch of files has no bound on its length.
func (s *session) script(ctx context.Context, body string, files []payload, stdout, stderr io.Writer) error {
	stage := s.shell.join(s.dir, "run", randomID())
	name := s.shell.scriptName()
	files = append([]payload{{name: name, data: []byte(s.shell.wrap(s.host(stage), body))}}, files...)

	reader, writer := io.Pipe()
	go func() { writer.CloseWithError(pack(writer, files)) }()
	err := s.guest.CopyTo(ctx, stage, reader)
	_ = reader.Close()
	if err != nil {
		return err
	}
	tail := &tailWriter{}
	code, err := s.guest.Run(ctx, guest.ExecRequest{Argv: s.shell.argv(s.shell.join(s.host(stage), name))}, nil,
		io.MultiWriter(stdout, tail), io.MultiWriter(stderr, tail))
	switch {
	case err != nil:
		return err
	case code == leaseHeldExit:
		return fmt.Errorf("%w: %s", ErrLeaseHeld, strings.TrimSpace(tail.String()))
	case code != 0:
		return exitError(code, tail)
	}
	return nil
}

// pack writes files as a tar stream. Their modes are the script's to set.
func pack(w io.Writer, files []payload) error {
	tw := tar.NewWriter(w)
	for _, file := range files {
		header := &tar.Header{Name: file.name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(file.data))}
		if file.source == "" {
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			if _, err := tw.Write(file.data); err != nil {
				return err
			}
			continue
		}
		f, err := os.Open(file.source)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err == nil {
			header.Size = info.Size()
			if err = tw.WriteHeader(header); err == nil {
				_, err = io.Copy(tw, f)
			}
		}
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", file.source, err)
		}
	}
	return tw.Close()
}

func randomID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// exitError is a script's failure, with the end of what it printed.
func exitError(code int, tail *tailWriter) error {
	var lines []string
	for line := range strings.Lines(tail.String()) {
		if !strings.HasPrefix(line, markerPrefix) {
			lines = append(lines, line)
		}
	}
	if out := strings.TrimSpace(strings.Join(lines, "")); out != "" {
		return fmt.Errorf("exit status %d\n%s", code, out)
	}
	return fmt.Errorf("exit status %d", code)
}

// markerPrefix starts the line a script prints when it has applied the
// resource of that index.
const markerPrefix = "::done "

// markerWriter takes the markers out of a script's output and passes the
// rest on.
type markerWriter struct {
	out  io.Writer
	done func(int)
	line []byte
}

func (m *markerWriter) Write(p []byte) (int, error) {
	m.line = append(m.line, p...)
	for {
		i := bytes.IndexByte(m.line, '\n')
		if i < 0 {
			return len(p), nil
		}
		m.emit(m.line[:i+1])
		m.line = m.line[i+1:]
	}
}

func (m *markerWriter) emit(line []byte) {
	if rest, ok := strings.CutPrefix(strings.TrimSpace(string(line)), markerPrefix); ok {
		if n, err := strconv.Atoi(rest); err == nil {
			m.done(n)
			return
		}
	}
	_, _ = m.out.Write(line)
}

func (m *markerWriter) flush() {
	if len(m.line) > 0 {
		m.emit(m.line)
		m.line = nil
	}
}

// tailWriter keeps the end of what a script printed, for its error.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
}

const tailBytes = 2048

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailBytes {
		t.buf = t.buf[len(t.buf)-tailBytes:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
