package vz

import (
	"archive/tar"
	"bytes"
	"context"
	"debug/buildinfo"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
)

// A macOS guest's agent (issue #83). launchd's daemon runs agentPath at boot,
// before anyone logs in, as root, and again whenever it exits (KeepAlive).
// agentPath is the stub: the agent itself comes on a read-only disk the host
// attaches at every boot (agentISOName), the macOS twin of the agent a Linux
// guest's boot initrd carries. The stub copies it to agentCopy and runs it, or
// runs the copy it made last when no disk is attached.
const (
	agentDir  = "/usr/local/libexec"
	agentPath = agentDir + "/disco-vm"
	agentCopy = agentDir + "/disco-vm-agent"
	daemonID  = "ai.discobox.vm.guest"
	// agentVolume is the volume name of the agent's disk; macOS mounts it at
	// /Volumes/DISCO_VM_AGENT. agentFile is the agent's name on it.
	agentVolume = "DISCO_VM_AGENT"
	agentFile   = "disco-vm"
)

// stubMark is the stub's second line, by which a boot knows a guest runs
// this stub. A change to the stub changes its number, and a guest with
// another is given this one (adoptStub).
const stubMark = "# disco-vm agent stub 1"

// stubWait is how many fifths of a second the stub waits for the agent's
// disk to be mounted. diskarbitrationd mounts it before launchd starts the
// daemon, or within a second or two.
const stubWait = 50

// agentStub is the stub as a guest holds it.
var agentStub = stubScript("/Volumes/"+agentVolume, agentCopy, stubWait)

// stubScript is the stub for the agent's disk mounted at disk, and the copy
// at copyPath, waiting up to wait fifths of a second for the disk. Only a
// read-only ISO 9660 mount is taken, so nothing but the host's disk of that
// name hands the guest its agent.
func stubScript(disk, copyPath string, wait int) string {
	return `#!/bin/sh
` + stubMark + `
# launchd runs this at boot (issue #83): it copies the agent from the disk
# the host attached, and runs it, or the copy it made last.
disk=` + disk + `
agent=` + copyPath + `
mounted() { mount | grep -F " on $disk (cd9660, " | grep -qF read-only; }
i=0
while ! mounted && [ "$i" -lt ` + strconv.Itoa(wait) + ` ]; do
	sleep 0.2
	i=$((i + 1))
done
if mounted && [ -f "$disk/` + agentFile + `" ]; then
	if ! cmp -s "$disk/` + agentFile + `" "$agent"; then
		if ! { cp "$disk/` + agentFile + `" "$agent.new" && chmod 755 "$agent.new" && mv -f "$agent.new" "$agent"; }; then
			echo "disco-vm stub: could not copy the agent from $disk; running the copy made last" >&2
		fi
	fi
else
	echo "disco-vm stub: no agent disk at $disk; running the copy made last" >&2
fi
exec "$agent" "$@"
`
}

// adoptScript runs as root in a macOS guest, by its agent, once the stub is
// uploaded beside agentPath as disco-vm.stub. An agent a guest was installed
// with before the stub becomes the stub's copy, for it to run until a disk
// hands it another; then the stub takes agentPath, and the running agent is
// killed, so launchd starts the daemon again (KeepAlive) on the stub. The
// kill ends the agent running this before its exec can answer.
func adoptScript(dir string) string {
	return `set -e
cd ` + dir + `
chown root:wheel ` + path.Base(agentPath) + `.stub
chmod 755 ` + path.Base(agentPath) + `.stub
if [ "$(head -c 2 ` + path.Base(agentPath) + `)" != "#!" ]; then
	cp -p ` + path.Base(agentPath) + ` ` + path.Base(agentCopy) + `.new
	mv -f ` + path.Base(agentCopy) + `.new ` + path.Base(agentCopy) + `
fi
mv -f ` + path.Base(agentPath) + `.stub ` + path.Base(agentPath) + `
launchctl kill SIGKILL system/` + daemonID + `
`
}

// agentProbe prints the path of the agent running it (its parent's command)
// and agentPath's second line: a guest on the stub runs agentCopy, and its
// agentPath carries stubMark.
var agentProbe = machine.ExecRequest{Argv: []string{"/bin/sh", "-c", `ps -o comm= -p "$PPID"; head -n 2 ` + agentPath + ` | tail -n 1`}}

// stubAdoptTimeout bounds how long the agent has to come back on the stub.
const stubAdoptTimeout = 2 * time.Minute

// adoptStub puts a booted macOS guest on the stub, once: a guest installed
// before it runs the agent it was installed with at agentPath, so it is never
// handed another, and a committed VM, and every VM created from it, would
// keep it. It is moved with only what every agent has served: a file put and
// an exec. A guest whose daemon already runs the stub's agent and holds this
// stub is left alone. Once the daemon restarts, the agent must answer from
// the stub's copy before the guest is ready; a guest that stops first, or an
// agent that does not, is an error.
func adoptStub(ctx context.Context, g machine.Guest, stopped <-chan struct{}, log io.Writer) error {
	running, mark, err := probeAgent(ctx, g)
	if err != nil {
		return err
	}
	if running == agentCopy && mark == stubMark {
		return nil
	}
	fmt.Fprintf(log, "vz: the guest's agent runs from %s; putting it on the agent stub\n", running)
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: path.Base(agentPath) + ".stub", Mode: 0o755, Size: int64(len(agentStub)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(agentStub)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := g.CopyTo(ctx, agentDir, &archive); err != nil {
		return fmt.Errorf("vz: upload the agent stub to the guest: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, stubAdoptTimeout)
	defer cancel()
	// The exec's answer is not awaited: the restart it asks for ends the
	// agent that would give it. Which agent answers afterwards is the result.
	var out bytes.Buffer
	execCtx, cancelExec := context.WithTimeout(ctx, 30*time.Second)
	code, execErr := machine.Run(execCtx, g, machine.ExecRequest{Argv: []string{"/bin/sh", "-c", adoptScript(agentDir)}}, nil, &out, &out)
	timedOut := execCtx.Err() != nil
	cancelExec()
	if execErr == nil && code != 0 {
		return fmt.Errorf("vz: put the guest on the agent stub: exit %d: %s", code, bytes.TrimSpace(out.Bytes()))
	}
	if timedOut {
		return fmt.Errorf("vz: put the guest on the agent stub: no answer within 30 s: %s", bytes.TrimSpace(out.Bytes()))
	}
	// launchd may refuse the first start of a program that replaced another
	// at the same path (a launch constraint) and starts it again within
	// seconds.
	for {
		attempt, cancelAttempt := context.WithTimeout(ctx, 10*time.Second)
		running, mark, err = probeAgent(attempt, g)
		cancelAttempt()
		if err == nil && running == agentCopy && mark == stubMark {
			fmt.Fprintf(log, "vz: the guest's agent runs from %s, on the agent stub\n", running)
			return nil
		}
		select {
		case <-stopped:
			return fmt.Errorf("vz: the guest stopped while it was put on the agent stub")
		case <-ctx.Done():
			return fmt.Errorf("vz: the guest's agent runs from %q %s after it was put on the agent stub, not %s (%v)", running, stubAdoptTimeout, agentCopy, err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// probeAgent runs agentProbe: the running agent's path, and agentPath's
// second line.
func probeAgent(ctx context.Context, g machine.Guest) (running, mark string, err error) {
	var out, errOut bytes.Buffer
	code, err := machine.Run(ctx, g, agentProbe, nil, &out, &errOut)
	if err != nil {
		return "", "", fmt.Errorf("vz: the guest's agent: %w", err)
	}
	if code != 0 {
		return "", "", fmt.Errorf("vz: the guest's agent: exit %d: %s", code, bytes.TrimSpace(errOut.Bytes()))
	}
	running, mark, _ = strings.Cut(strings.TrimRight(out.String(), "\n"), "\n")
	return strings.TrimSpace(running), strings.TrimSpace(mark), nil
}

// agentMain is the main package of the binary a macOS guest's agent disk
// carries: disco-vm's own.
const agentMain = "github.com/discobox-ai/vm/cmd/disco-vm"

// checkAgentBinary refuses, on the host, a binary that is not disco-vm: an
// embedder's program that runs a shim is not an agent, and run in the guest
// as one it would not serve.
func checkAgentBinary(path string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("vz: %s is not a Go binary, so not disco-vm, which a macOS guest's agent disk carries: %w", path, err)
	}
	if info.Path != agentMain {
		return fmt.Errorf("vz: %s is %s, not disco-vm (%s), which a macOS guest's agent disk carries; boot it with disco-vm's shim", path, info.Path, agentMain)
	}
	return nil
}
