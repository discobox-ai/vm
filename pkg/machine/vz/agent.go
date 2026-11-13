package vz

import (
	"archive/tar"
	"bytes"
	"context"
	"debug/buildinfo"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// The agent's place in a macOS guest, and the launchd daemon that starts it
// at boot, before anyone logs in, as root, and starts it again whenever it
// exits (KeepAlive).
const (
	agentPath = "/usr/local/libexec/disco-vm"
	daemonID  = "ai.discobox.vm.guest"
)

// agentMoveTimeout bounds how long a moved agent has to come back speaking
// this host's protocol.
const agentMoveTimeout = 2 * time.Minute

// moveScript runs as root in a macOS guest, by the agent it replaces, with
// the protocol the new agent must speak ($1). The new agent, uploaded beside
// the old one, is run first: unless it runs in this guest, exits within 10 s,
// and prints that protocol, it is killed and removed, nothing is touched, and
// the old agent goes on serving (exit 3).
// Then the old one is kept as disco-vm.old, for a person to put back by hand,
// the new one takes its place, and the old one is killed, so launchd starts
// the daemon again (KeepAlive) on the new one. The kill ends the agent running
// this before its exec can answer; a graceful restart (kickstart -k) would
// let the old agent answer for a moment after it, as if it were the new one.
var moveScript = `set -e
new=` + agentPath + `.new
chown root:wheel "$new"
chmod 755 "$new"
# Its own process group (job control), so a timeout kills all it started.
set -m
"$new" guest --protocol >"$new.out" 2>&1 &
pid=$!
set +m
i=0
while kill -0 "$pid" 2>/dev/null; do
	i=$((i + 1))
	if [ "$i" -gt 50 ]; then
		kill -9 -- "-$pid" 2>/dev/null || kill -9 "$pid" 2>/dev/null || true
		rm -f "$new" "$new.out"
		echo "the uploaded agent did not exit within 10 s of guest --protocol"
		exit 3
	fi
	sleep 0.2
done
wait "$pid" || true
got=$(head -c 200 "$new.out")
rm -f "$new.out"
if [ "$got" != "$1" ]; then
	rm -f "$new"
	echo "the uploaded agent does not speak protocol $1: $got"
	exit 3
fi
cp -p ` + agentPath + ` ` + agentPath + `.old
mv -f "$new" ` + agentPath + `
launchctl kill SIGKILL system/` + daemonID + `
`

// moveAgent brings a macOS guest's agent to this host's protocol (discobox
// ADR 26-11-11-963 §9). A macOS guest's agent is baked in at install, so a
// committed VM, and every VM created from it, keeps the agent it was installed
// with; installing macOS again takes many minutes. An agent that speaks
// another protocol is replaced with agent, the disco-vm built for darwin that
// is booting it, as an install's is the one installing it. It is moved with
// only what every agent has served: its info, a file put, and an exec. Once
// the daemon restarts, the agent must answer with this host's protocol before
// the guest is ready; a guest that stops first, or an agent that does not, is
// an error.
func moveAgent(ctx context.Context, g machine.Guest, agent string, stopped <-chan struct{}, log io.Writer) error {
	info, err := g.Info(ctx)
	if err != nil {
		return fmt.Errorf("vz: the guest's agent: %w", err)
	}
	if info.Protocol == guest.Protocol {
		return nil
	}
	fmt.Fprintf(log, "vz: the guest's agent (disco-vm %s) speaks protocol %d; moving it to this host's, %d\n", info.Version, info.Protocol, guest.Protocol)
	binary, err := os.ReadFile(agent)
	if err != nil {
		return fmt.Errorf("vz: read the agent: %w", err)
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: path.Base(agentPath) + ".new", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(binary); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := g.CopyTo(ctx, path.Dir(agentPath), &archive); err != nil {
		return fmt.Errorf("vz: upload the agent to the guest: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, agentMoveTimeout)
	defer cancel()
	// The exec's answer is not awaited: the restart it asks for ends the
	// agent that would give it. What the agent says afterwards is the result.
	var out bytes.Buffer
	execCtx, cancelExec := context.WithTimeout(ctx, 30*time.Second)
	code, execErr := machine.Run(execCtx, g, moveRequest(), nil, &out, &out)
	timedOut := execCtx.Err() != nil
	cancelExec()
	if execErr == nil && code != 0 {
		return fmt.Errorf("vz: move the guest's agent: exit %d: %s", code, bytes.TrimSpace(out.Bytes()))
	}
	// The script bounds its own check, so an exec still running at its
	// deadline is a move that did not finish, not an agent that was killed.
	if timedOut {
		return fmt.Errorf("vz: move the guest's agent: no answer within 30 s: %s", bytes.TrimSpace(out.Bytes()))
	}
	// The new agent is the one that answers once the old one has gone: its
	// exec cut off, or a request it no longer answers. launchd may refuse the
	// first start of a binary that replaced another at the same path (a
	// launch constraint) and starts it again within seconds.
	gone := execErr != nil
	for {
		attempt, cancelAttempt := context.WithTimeout(ctx, 5*time.Second)
		now, err := g.Info(attempt)
		cancelAttempt()
		if err != nil {
			gone = true
		}
		if err == nil && gone && now.Protocol == guest.Protocol {
			fmt.Fprintf(log, "vz: the guest's agent is disco-vm %s, protocol %d\n", now.Version, now.Protocol)
			return nil
		}
		if err == nil {
			info = now
		}
		select {
		case <-stopped:
			return fmt.Errorf("vz: the guest stopped while its agent was moved to protocol %d", guest.Protocol)
		case <-ctx.Done():
			return fmt.Errorf("vz: the guest's agent (disco-vm %s) still speaks protocol %d %s after it was moved, and this host speaks %d", info.Version, info.Protocol, agentMoveTimeout, guest.Protocol)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// moveRequest is the exec that runs moveScript for this host's protocol.
func moveRequest() machine.ExecRequest {
	return machine.ExecRequest{Argv: []string{"/bin/sh", "-c", moveScript, "move-agent", strconv.Itoa(guest.Protocol)}}
}

// agentMain is the main package of the binary a macOS guest's agent is moved
// to: disco-vm's own.
const agentMain = "github.com/discobox-ai/vm/cmd/disco-vm"

// checkAgentBinary refuses, on the host and before anything reaches the
// guest, a binary that is not disco-vm: an embedder's program that runs a
// shim is not an agent, and run in the guest as one it would not exit.
func checkAgentBinary(path string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("vz: %s is not a Go binary, so not disco-vm, which a macOS guest's agent is moved to: %w", path, err)
	}
	if info.Path != agentMain {
		return fmt.Errorf("vz: %s is %s, not disco-vm (%s), which a macOS guest's agent is moved to; boot it with disco-vm's shim", path, info.Path, agentMain)
	}
	return nil
}
