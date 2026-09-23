package hcs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Fork is HCS's live template: boot the image, wait for its agent, pause it,
// and save it AsTemplate, which freezes it in memory (it writes no restorable
// file). Clones are created with RestoreState.TemplateSystemId and share its
// memory copy-on-write; sandboxi measured 0.44-0.86 s to create and start one.
//
// Two facts shape the code:
//
//   - A template can only be forked by the process that created it, through
//     its handle: from any other process the create fails 0xC0370400. So the
//     warm shim that holds the template also creates every clone, over a named
//     pipe, and hands the running clone to the instance's shim, which opens it
//     by ID. The clone belongs to vmcompute, so the instance shim owns it like
//     any other VM once the warm shim lets go of its handle.
//   - A clone's memory believes every write the template made while booting
//     is on disk, so a clone differences over the template's disk, not the
//     image's. The template's disk is immutable once frozen, and each clone
//     hard-links it into its own directory, so a stopped clone stays bootable
//     after the image is cooled.
//
// A running clone lives on its template's memory: cooling the image ends it.

const (
	templateJSON     = "template.json"
	templateDiskName = "template.vhdx"
	templateGuest    = "template.vmgs"
	templateState    = "template.vmrs"
	// forkDiskName is the clone's hard link to its template's disk.
	forkDiskName = "template.vhdx"

	suspend    = `{"SuspensionLevel":"Suspend"}`
	asTemplate = `{"SaveType":"AsTemplate"}`
	pipeSDDL   = "D:P(A;;GA;;;BA)(A;;GA;;;SY)"
)

// template is what a clone needs to find a warm image's template.
type template struct {
	ID     string `json:"id"`
	Pipe   string `json:"pipe"`
	CPUs   int    `json:"cpus,omitempty"`
	Memory uint64 `json:"memory,omitempty"`
}

// forkTicket is a prepared clone's claim on a template, kept until its first
// boot.
type forkTicket struct {
	Template string `json:"template"`
	Pipe     string `json:"pipe"`
}

// cloneRequest asks the template's holder to fork a clone.
type cloneRequest struct {
	ID        string    `json:"id"`
	Disk      string    `json:"disk"`
	GuestFile string    `json:"guestFile"`
	StateFile string    `json:"stateFile"`
	NIC       *endpoint `json:"nic,omitempty"`
	// Console is the clone's own console pipe, and who may open it.
	Console    string `json:"console,omitempty"`
	ConsoleSID string `json:"consoleSID,omitempty"`
}

type cloneReply struct {
	Error string `json:"error,omitempty"`
}

func readTemplate(dir string) (*template, error) {
	var t template
	if err := fsutil.ReadJSON(filepath.Join(dir, templateJSON), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// alive reports whether the template system exists. A warm shim that crashed
// closed its handle, and the template went with it.
func (t *template) alive() bool {
	sys, err := openSystem(t.ID)
	if err != nil {
		return false
	}
	defer sys.close()
	_, err = sys.properties()
	return err == nil
}

// Warm boots a template from spec.Chain, waits for its agent, and freezes it.
// The returned stage holds the template, and serves clones, until closed.
func (d *Driver) Warm(ctx context.Context, spec machine.WarmSpec) (machine.Stage, error) {
	if len(spec.Chain) == 0 {
		return nil, errors.New("hcs: warm: no image")
	}
	log := spec.Log
	if log == nil {
		log = io.Discard
	}
	if t, err := readTemplate(spec.Dir); err == nil && t.alive() {
		return nil, fmt.Errorf("hcs: warm: template %s is already held", t.ID)
	}
	if err := clearDir(spec.Dir); err != nil {
		return nil, err
	}
	disk := filepath.Join(spec.Dir, templateDiskName)
	if err := createDiff(disk, layerDisk(spec.Chain[len(spec.Chain)-1])); err != nil {
		return nil, err
	}
	gf, sf := filepath.Join(spec.Dir, templateGuest), filepath.Join(spec.Dir, templateState)
	if err := createGuestStateFile(gf); err != nil {
		return nil, err
	}
	if err := createRuntimeStateFile(sf); err != nil {
		return nil, err
	}
	id := newID()
	if err := grantAll(id, grantPaths(spec.Dir, spec.Chain)); err != nil {
		return nil, err
	}
	fmt.Fprintf(log, "hcs: booting template %s\n", id)
	m, err := startVM(id, vmConfig{Disk: disk, GuestFile: gf, StateFile: sf, CPUs: spec.CPUs, Memory: spec.Memory,
		Console: consolePipe(id), ConsoleSID: currentUserSID()})
	if err != nil {
		return nil, err
	}
	fail := func(err error) (machine.Stage, error) {
		m.shutdown()
		return nil, err
	}
	client := guest.NewClient(func(ctx context.Context) (net.Conn, error) { return m.Dial(ctx, guest.AgentPort) })
	err = client.WaitReady(ctx, m.Done())
	// Nothing of ours may be connected into the memory clones inherit.
	client.Close()
	if err != nil {
		return fail(fmt.Errorf("hcs: warm: the template's agent: %w", err))
	}
	fmt.Fprintf(log, "hcs: freezing template %s\n", id)
	if err := m.sys.pause(suspend); err != nil {
		return fail(err)
	}
	if err := m.sys.save(asTemplate); err != nil {
		if hresultOf(err) == eResources {
			err = fmt.Errorf("%w: the host lacks the commit headroom to freeze the template's memory; warm with less --memory or free some RAM", err)
		}
		return fail(err)
	}
	pipe := `\\.\pipe\disco-vm-fork-` + id
	listener, err := winio.ListenPipe(pipe, &winio.PipeConfig{SecurityDescriptor: pipeSDDL})
	if err != nil {
		return fail(err)
	}
	st := &stage{vm: m, listener: listener, dir: spec.Dir, id: id, cpus: spec.CPUs, memory: spec.Memory}
	if err := fsutil.WriteJSON(filepath.Join(spec.Dir, templateJSON), template{ID: id, Pipe: pipe, CPUs: spec.CPUs, Memory: spec.Memory}); err != nil {
		_ = listener.Close()
		return fail(err)
	}
	go st.serve()
	fmt.Fprintf(log, "hcs: template %s is frozen and serving clones on %s\n", id, pipe)
	return st, nil
}

// clearDir empties a directory, keeping it.
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// stage is a frozen template held by this process.
type stage struct {
	vm       *vm
	listener net.Listener
	dir      string
	id       string
	cpus     int
	memory   uint64

	closeOnce sync.Once
	wg        sync.WaitGroup
}

func (s *stage) Done() <-chan struct{} { return s.vm.Done() }

// Close ends the template, and with it every clone still running on its
// memory.
func (s *stage) Close(context.Context) error {
	s.closeOnce.Do(func() {
		_ = s.listener.Close()
		_ = os.Remove(filepath.Join(s.dir, templateJSON))
		s.vm.shutdown()
		s.wg.Wait()
	})
	return nil
}

func (s *stage) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.clone(conn)
		}()
	}
}

// clone forks one clone for a connection, and holds its handle until the
// requester has opened its own and hung up.
func (s *stage) clone(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	var req cloneRequest
	if err := json.NewDecoder(reader).Decode(&req); err != nil {
		return
	}
	reply := func(err error) {
		r := cloneReply{}
		if err != nil {
			r.Error = err.Error()
		}
		_ = json.NewEncoder(conn).Encode(r)
	}
	sys, err := createSystem(req.ID, newDocument(vmConfig{
		Disk: req.Disk, GuestFile: req.GuestFile, StateFile: req.StateFile,
		CPUs: s.cpus, Memory: s.memory, Template: s.id,
		Console: req.Console, ConsoleSID: req.ConsoleSID,
	}))
	if err != nil {
		reply(err)
		return
	}
	defer sys.close()
	// Start on the creating handle: it carries the forked device state that
	// reopening by ID loses.
	if err := sys.start(); err != nil {
		reply(err)
		return
	}
	// A clone's NIC can only be hot-added: the template's memory has no
	// adapter, and one in the create document fails the restore.
	if req.NIC != nil {
		if err := sys.modify(addNIC(req.NIC)); err != nil {
			_ = sys.terminate()
			reply(fmt.Errorf("give the clone a NIC: %w", err))
			return
		}
	}
	reply(nil)
	// The requester opens its own handle, then hangs up; until then this
	// handle keeps the clone alive. A requester that dies first takes the
	// clone with it.
	_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
	_, _ = io.Copy(io.Discard, reader)
}

// Warmth is a fork template serving any number of clones while it lives.
func (d *Driver) Warmth(_ context.Context, spec machine.WarmSpec) (machine.Warmth, error) {
	t, err := readTemplate(spec.Dir)
	if err != nil || !t.alive() {
		return machine.Warmth{Mode: machine.Cold}, nil
	}
	return machine.Warmth{Mode: machine.Fork, Clones: -1}, nil
}

// Cool ends a template a crashed warm shim may have left, and removes the
// stage's files.
func (d *Driver) Cool(_ context.Context, spec machine.WarmSpec) error {
	if t, err := readTemplate(spec.Dir); err == nil {
		terminateStale(t.ID)
	}
	return clearDir(spec.Dir)
}

// prepareFork lays out a clone of the warm image's template: a hard link to
// the template's disk, a differencing disk over it, and the template's guest
// state.
func (d *Driver) prepareFork(inst machine.InstanceSpec) error {
	t, err := readTemplate(inst.WarmDir)
	if err != nil || !t.alive() {
		return fmt.Errorf("hcs: no live template: %w", machine.ErrNotWarm)
	}
	link := filepath.Join(inst.Dir, forkDiskName)
	if err := linkOrCopy(filepath.Join(inst.WarmDir, templateDiskName), link); err != nil {
		return err
	}
	if err := createDiff(filepath.Join(inst.Dir, diskName), link); err != nil {
		return err
	}
	if err := fsutil.CopyFile(filepath.Join(inst.WarmDir, templateGuest), filepath.Join(inst.Dir, guestFileName), 0o644); err != nil {
		return err
	}
	st := readState(inst.Dir)
	st.Fork = &forkTicket{Template: t.ID, Pipe: t.Pipe}
	return writeState(inst.Dir, st)
}

// linkOrCopy hard-links src to dst, copying where a link is refused.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return fsutil.CopyFile(src, dst, 0o644)
}

// bootFork asks the template's holder to fork this instance, then takes the
// clone over. Only the first boot after Prepare forks; the ticket is spent
// whatever happens, and later boots are cold boots of the clone's disk.
func (d *Driver) bootFork(ctx context.Context, inst machine.InstanceSpec, st instanceState, log io.Writer) (*vm, error) {
	ticket := st.Fork
	st.Fork = nil
	if err := writeState(inst.Dir, st); err != nil {
		return nil, err
	}
	id := vmID(inst.Dir)
	terminateStale(id)
	sf := filepath.Join(inst.Dir, stateFileName)
	_ = os.Remove(sf)
	if err := createRuntimeStateFile(sf); err != nil {
		return nil, err
	}
	if err := grantAll(id, grantPaths(inst.Dir, inst.Chain)); err != nil {
		return nil, err
	}
	nic, err := createEndpoint(st.MAC)
	if err != nil {
		return nil, err
	}
	st.MAC = nic.MAC
	_ = writeState(inst.Dir, st)

	fmt.Fprintf(log, "hcs: forking %s as %s from template %s\n", inst.ID, id, ticket.Template)
	conn, err := winio.DialPipeContext(ctx, ticket.Pipe)
	if err != nil {
		deleteEndpoint(nic.ID)
		return nil, fmt.Errorf("hcs: reach the template's holder: %w", err)
	}
	defer conn.Close()
	req := cloneRequest{
		ID: id, Disk: filepath.Join(inst.Dir, diskName),
		GuestFile: filepath.Join(inst.Dir, guestFileName), StateFile: sf, NIC: nic,
		Console: consolePipe(id), ConsoleSID: currentUserSID(),
	}
	var reply cloneReply
	if err := json.NewEncoder(conn).Encode(req); err == nil {
		err = json.NewDecoder(conn).Decode(&reply)
		if err != nil {
			reply.Error = err.Error()
		}
	} else {
		reply.Error = err.Error()
	}
	if reply.Error != "" {
		deleteEndpoint(nic.ID)
		return nil, fmt.Errorf("hcs: fork: %s", reply.Error)
	}
	sys, err := openSystem(id)
	if err != nil {
		deleteEndpoint(nic.ID)
		return nil, err
	}
	m, err := watch(sys, nic)
	if err != nil {
		return nil, err
	}
	if err := m.readRuntimeID(); err != nil {
		m.shutdown()
		return nil, err
	}
	return m, nil
}
