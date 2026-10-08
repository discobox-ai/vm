package boxd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// fakeAPI is boxd's API with no cloud behind it. A machine is a directory
// standing in for its disk, and a guest agent process (`disco-vm guest
// --fake`) while it runs; a snapshot is a copy of that directory. Memory is not
// modelled: a restored machine's agent starts fresh. Exec understands exactly
// the commands the driver sends.
//
// As on boxd, a guest that shuts itself down leaves its machine "running": the
// agent exiting stands in for the guest halting, and sets the marker the real
// shutdown hook writes.
type fakeAPI struct {
	boxdapi.UnimplementedBoxdApiServer
	agent string
	dir   string

	mu        sync.Mutex
	vms       map[string]*fakeVM
	snapshots map[string]*fakeSnapshot

	// down makes GetVm and StopVm fail as an unreachable API does.
	down atomic.Bool
}

type fakeVM struct {
	id, name  string
	dir       string
	status    string
	installed bool
	vcpu      uint32
	memory    uint64
	agent     *exec.Cmd
	halted    string
}

type fakeSnapshot struct {
	id, name  string
	dir       string
	installed bool
	vcpu      uint32
	memory    uint64
}

func newFakeAPI(agent, dir string) *fakeAPI {
	return &fakeAPI{agent: agent, dir: dir, vms: map[string]*fakeVM{}, snapshots: map[string]*fakeSnapshot{}}
}

func (f *fakeAPI) root(v *fakeVM) string   { return filepath.Join(v.dir, "root") }
func (f *fakeAPI) socket(v *fakeVM) string { return filepath.Join(v.dir, "agent.sock") }

func (f *fakeAPI) vm(id string) (*fakeVM, error) {
	for _, v := range f.vms {
		if v.id == id || v.name == id {
			return v, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "no machine %q", id)
}

func (f *fakeAPI) newVM(name string, vcpu uint32, memory uint64) *fakeVM {
	id := "vm_" + fsutil.RandomHex(4)
	if name == "" {
		name = id
	}
	if vcpu == 0 {
		vcpu, memory = 2, 8<<30
	}
	v := &fakeVM{id: id, name: name, dir: filepath.Join(f.dir, id), status: "running", vcpu: vcpu, memory: memory}
	f.vms[id] = v
	return v
}

// boot starts the guest's agent, if it has one, and marks the machine stopped
// when the agent exits: with --fake, an orderly shutdown exits the agent.
func (f *fakeAPI) boot(v *fakeVM) error {
	v.status, v.halted = "running", ""
	if !v.installed {
		return nil
	}
	cmd := exec.Command(f.agent, "guest", "--listen", "unix:"+f.socket(v), "--root", f.root(v), "--fake")
	if err := cmd.Start(); err != nil {
		return err
	}
	v.agent = cmd
	go func() {
		_ = cmd.Wait()
		f.mu.Lock()
		defer f.mu.Unlock()
		if v.agent == cmd {
			v.agent, v.halted = nil, "poweroff"
		}
	}()
	return nil
}

func (f *fakeAPI) halt(v *fakeVM) {
	if cmd := v.agent; cmd != nil {
		v.agent = nil
		_ = cmd.Process.Kill()
	}
	v.status, v.halted = "stopped", ""
}

func (f *fakeAPI) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.vms {
		f.halt(v)
	}
}

func (f *fakeAPI) Whoami(context.Context, *boxdapi.WhoamiRequest) (*boxdapi.WhoamiResponse, error) {
	return &boxdapi.WhoamiResponse{UserId: "usr_fake"}, nil
}

func (f *fakeAPI) CreateVm(_ context.Context, req *boxdapi.CreateVmRequest) (*boxdapi.CreateVmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.newVM(req.GetName(), req.GetConfig().GetVcpu(), req.GetConfig().GetMemoryBytes())
	if err := os.MkdirAll(f.root(v), 0o755); err != nil {
		return nil, err
	}
	return &boxdapi.CreateVmResponse{VmId: v.id, Name: v.name, Status: v.status}, nil
}

func (f *fakeAPI) CreateVmFromSnapshot(_ context.Context, req *boxdapi.CreateVmFromSnapshotRequest) (*boxdapi.CreateVmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap, ok := f.snapshots[req.GetSnapshot()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no snapshot %q", req.GetSnapshot())
	}
	v := f.newVM(req.GetName(), snap.vcpu, snap.memory)
	v.installed = snap.installed
	if err := fsutil.CopyTree(snap.dir, f.root(v)); err != nil {
		return nil, err
	}
	if err := f.boot(v); err != nil {
		return nil, err
	}
	return &boxdapi.CreateVmResponse{VmId: v.id, Name: v.name, Status: v.status}, nil
}

func (f *fakeAPI) GetVm(_ context.Context, req *boxdapi.GetVmRequest) (*boxdapi.GetVmResponse, error) {
	if f.down.Load() {
		return nil, status.Error(codes.Unavailable, "fake boxd is down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	return &boxdapi.GetVmResponse{VmId: v.id, Name: v.name, Status: v.status, Vcpu: v.vcpu, MemoryBytes: v.memory}, nil
}

func (f *fakeAPI) DestroyVm(_ context.Context, req *boxdapi.DestroyVmRequest) (*boxdapi.DestroyVmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	f.halt(v)
	delete(f.vms, v.id)
	return &boxdapi.DestroyVmResponse{}, os.RemoveAll(v.dir)
}

func (f *fakeAPI) StartVm(_ context.Context, req *boxdapi.StartVmRequest) (*boxdapi.StartVmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	if v.status != "stopped" {
		return nil, status.Errorf(codes.FailedPrecondition, "%s is %s", v.name, v.status)
	}
	return &boxdapi.StartVmResponse{}, f.boot(v)
}

func (f *fakeAPI) StopVm(_ context.Context, req *boxdapi.StopVmRequest) (*boxdapi.StopVmResponse, error) {
	if f.down.Load() {
		return nil, status.Error(codes.Unavailable, "fake boxd is down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	f.halt(v)
	return &boxdapi.StopVmResponse{}, nil
}

func (f *fakeAPI) setStatus(id, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(id)
	if err != nil {
		return err
	}
	if v.status != from {
		return status.Errorf(codes.FailedPrecondition, "%s is %s, not %s", v.name, v.status, from)
	}
	v.status = to
	return nil
}

func (f *fakeAPI) SuspendVm(_ context.Context, req *boxdapi.SuspendVmRequest) (*boxdapi.SuspendVmResponse, error) {
	return &boxdapi.SuspendVmResponse{}, f.setStatus(req.GetVmId(), "running", "suspended")
}

func (f *fakeAPI) ResumeVm(_ context.Context, req *boxdapi.ResumeVmRequest) (*boxdapi.ResumeVmResponse, error) {
	return &boxdapi.ResumeVmResponse{}, f.setStatus(req.GetVmId(), "suspended", "running")
}

func (f *fakeAPI) SetAutoSuspendTimeout(context.Context, *boxdapi.SetAutoSuspendTimeoutRequest) (*boxdapi.SetAutoSuspendTimeoutResponse, error) {
	return &boxdapi.SetAutoSuspendTimeoutResponse{}, nil
}

func (f *fakeAPI) SetAutoHibernateTimeout(context.Context, *boxdapi.SetAutoHibernateTimeoutRequest) (*boxdapi.SetAutoHibernateTimeoutResponse, error) {
	return &boxdapi.SetAutoHibernateTimeoutResponse{}, nil
}

func (f *fakeAPI) ResizeVm(_ context.Context, req *boxdapi.ResizeVmRequest) (*boxdapi.ResizeVmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVmId())
	if err != nil {
		return nil, err
	}
	if v.status != "stopped" {
		return nil, status.Error(codes.FailedPrecondition, "the fake resizes stopped machines only")
	}
	v.vcpu, v.memory = req.GetVcpu(), req.GetMemoryBytes()
	return &boxdapi.ResizeVmResponse{Vcpu: v.vcpu, MemoryBytes: v.memory}, nil
}

func (f *fakeAPI) CreateSnapshot(_ context.Context, req *boxdapi.CreateSnapshotRequest) (*boxdapi.CreateSnapshotResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.vm(req.GetVm())
	if err != nil {
		return nil, err
	}
	if v.status != "running" {
		return nil, status.Errorf(codes.FailedPrecondition, "%s is %s, not running", v.name, v.status)
	}
	snap := &fakeSnapshot{
		id: "snap_" + fsutil.RandomHex(4), name: req.GetName(), installed: v.installed, vcpu: v.vcpu, memory: v.memory,
	}
	snap.dir = filepath.Join(f.dir, snap.id)
	if err := fsutil.CopyTree(f.root(v), snap.dir); err != nil {
		return nil, err
	}
	f.snapshots[snap.name] = snap
	return &boxdapi.CreateSnapshotResponse{SnapshotId: snap.id, Name: snap.name, Version: 1, Status: "pending"}, nil
}

func (f *fakeAPI) GetSnapshot(_ context.Context, req *boxdapi.GetSnapshotRequest) (*boxdapi.GetSnapshotResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap, ok := f.snapshots[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no snapshot %q", req.GetName())
	}
	return &boxdapi.GetSnapshotResponse{Snapshot: &boxdapi.SnapshotInfo{
		SnapshotId: snap.id, Name: snap.name, Version: 1, Status: "ready", Vcpu: snap.vcpu, MemoryBytes: snap.memory,
	}}, nil
}

func (f *fakeAPI) DeleteSnapshot(_ context.Context, req *boxdapi.DeleteSnapshotRequest) (*boxdapi.DeleteSnapshotResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap, ok := f.snapshots[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no snapshot %q", req.GetName())
	}
	delete(f.snapshots, snap.name)
	return &boxdapi.DeleteSnapshotResponse{}, os.RemoveAll(snap.dir)
}

func (f *fakeAPI) UploadFileStream(stream grpc.ClientStreamingServer[boxdapi.UploadFileChunk, boxdapi.UploadFileResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	f.mu.Lock()
	v, err := f.vm(first.GetVmId())
	f.mu.Unlock()
	if err != nil {
		return err
	}
	// The fake keeps uploads beside the root, not in it: they are guest
	// paths outside anything a test reads back.
	out, err := os.Create(filepath.Join(v.dir, "upload-"+filepath.Base(first.GetPath())))
	if err != nil {
		return err
	}
	defer out.Close()
	written, _ := out.Write(first.GetData())
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&boxdapi.UploadFileResponse{BytesWritten: uint64(written)})
		}
		if err != nil {
			return err
		}
		n, _ := out.Write(chunk.GetData())
		written += n
	}
}

func (f *fakeAPI) Exec(stream grpc.BidiStreamingServer[boxdapi.ExecChunk, boxdapi.ExecChunk]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	f.mu.Lock()
	v, err := f.vm(first.GetVmId())
	var running bool
	if err == nil {
		running = v.status == "running"
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if !running {
		return status.Errorf(codes.FailedPrecondition, "%s is not running", v.name)
	}
	exit := func(code int32, stderr string) error {
		if stderr != "" {
			if err := stream.Send(&boxdapi.ExecChunk{Data: []byte(stderr), IsStderr: true}); err != nil {
				return err
			}
		}
		return stream.Send(&boxdapi.ExecChunk{ExitCode: code})
	}

	command := first.GetCommand()
	pipePrefix := "sudo -n " + agentPath + " pipe "
	switch {
	case command == "uname -m":
		arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
		if err := stream.Send(&boxdapi.ExecChunk{Data: []byte(arch + "\n")}); err != nil {
			return err
		}
		return exit(0, "")
	case command == "cat "+haltedMarker+" 2>/dev/null":
		f.mu.Lock()
		how := v.halted
		f.mu.Unlock()
		if how == "" {
			return exit(1, "")
		}
		if err := stream.Send(&boxdapi.ExecChunk{Data: []byte(how + "\n")}); err != nil {
			return err
		}
		return exit(0, "")
	case command == installScript:
		f.mu.Lock()
		v.installed = true
		err := f.boot(v)
		f.mu.Unlock()
		if err != nil {
			return exit(1, err.Error())
		}
		return exit(0, "")
	case strings.HasPrefix(command, pipePrefix):
		target := strings.TrimPrefix(command, pipePrefix)
		if target == "unix:"+agentSocket {
			target = "unix:" + f.socket(v)
		}
		stdin, feed := io.Pipe()
		go func() {
			for {
				chunk, err := stream.Recv()
				if err != nil {
					// The client half-closed (EOF) or went away.
					_ = feed.Close()
					return
				}
				if _, err := feed.Write(chunk.GetData()); err != nil {
					return
				}
			}
		}()
		err := guest.Pipe(stream.Context(), target, stdin, execWriter{stream})
		_ = stdin.Close()
		if err != nil {
			return exit(1, err.Error())
		}
		return exit(0, "")
	default:
		return exit(127, fmt.Sprintf("fake boxd: unknown command %q", command))
	}
}

type execWriter struct {
	stream grpc.BidiStreamingServer[boxdapi.ExecChunk, boxdapi.ExecChunk]
}

func (w execWriter) Write(p []byte) (int, error) {
	if err := w.stream.Send(&boxdapi.ExecChunk{Data: p}); err != nil {
		return 0, err
	}
	return len(p), nil
}
