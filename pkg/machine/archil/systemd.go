package archil

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
)

// An archil guest runs systemd, as every Linux guest of disco-vm's does (ADR
// 26-11-20-447). Archil's init is the sandbox's PID 1 and runs nothing of the
// image's, so the driver registers systemd, once, as a service of Archil's
// own supervisor in the guest, which starts it at every boot and again if it
// exits. systemd runs as PID 1 of a PID and mount namespace of its own, and
// every guest operation (command) enters its namespaces.

// systemdService is the name of the Archil service that runs systemd.
const systemdService = "disco-vm-systemd"

// archilClient is Archil's own client in every guest, on its read-only
// /opt/archil, whose services subcommand reaches Archil's supervisor.
const archilClient = "/opt/archil/archil-client"

const (
	// systemdScript runs systemd as PID 1 of new PID and mount namespaces.
	// The guest's root is made shared first and systemd's mounts its slaves,
	// so a mount made outside after systemd starts (a data disk) reaches its
	// units. --kill-child ends systemd with the service.
	systemdScript = `mount --make-rshared / || exit
exec unshare --pid --fork --mount-proc --propagation slave --kill-child /lib/systemd/systemd --system --unit=multi-user.target`

	// ensureScript makes the guest one whose boot runs systemd, at Create,
	// in Archil's own namespaces, and fails naming what the base lacks. $1
	// is systemdScript, and $2 "fenced" for a VM whose egress is fenced.
	//
	// systemd is installed where the base has none, with apt, under the
	// VM's fence: a VM that can't reach its base's mirror needs a base with
	// systemd. A fenced VM's install fails at the first fetch that fails
	// rather than retrying it, since a fence that drops the mirror's traffic
	// held Create for three minutes on Archil; an open one keeps apt's
	// retries.
	//
	// Then systemdService is registered with Archil's supervisor where
	// `services get` says it has none (exit 1, as Archil answers, seen
	// 2026-11-21): a fresh VM's. A copy carries its source's. Any other
	// answer fails rather than risk a second systemd.
	ensureScript = `need() {
	[ -x /lib/systemd/systemd ] || return 1
	for c in nsenter unshare setpriv runuser mount; do command -v "$c" >/dev/null 2>&1 || return 1; done
}
if ! need; then
	if ! command -v apt-get >/dev/null 2>&1; then
		echo "the base has no /lib/systemd/systemd, and no apt-get to install it: an archil VM's base needs systemd and util-linux" >&2
		exit 3
	fi
	log=$(mktemp -p /var/tmp disco-vm-systemd.XXXXXX) || exit 1
	apt=-q update=
	if [ "$2" = fenced ]; then
		apt="$apt -o Acquire::Retries=0 -o Acquire::http::Timeout=15 -o Acquire::https::Timeout=15"
		update="-o APT::Update::Error-Mode=any"
	fi
	if ! { DEBIAN_FRONTEND=noninteractive apt-get $apt $update update && DEBIAN_FRONTEND=noninteractive apt-get $apt install -y --no-install-recommends systemd systemd-sysv dbus util-linux; } >"$log" 2>&1 || ! need; then
		echo "the base has no /lib/systemd/systemd, and installing it failed (a VM fenced from its base's package mirror, as --egress none is, needs a base with systemd):" >&2
		tail -n 5 "$log" >&2
		rm -f "$log"
		exit 3
	fi
	rm -f "$log"
fi
if [ ! -x ` + archilClient + ` ]; then
	echo "the guest has no ` + archilClient + `, Archil's supervisor, to run systemd" >&2
	exit 3
fi
out=$(` + archilClient + ` services get ` + systemdService + ` 2>&1)
case $? in
0) exit 0 ;;
1) exec ` + archilClient + ` services create ` + systemdService + ` -- sh -c "$1" ;;
*) echo "Archil's supervisor did not answer for ` + systemdService + `: $out" >&2; exit 3 ;;
esac`

	// enterScript runs a command in systemd's namespaces: $1 is its
	// directory, $2 the account it runs as ("" is root), and the rest its
	// argv. systemd is the process whose PID in its own namespace is 1.
	// Archil's kill was seen to SIGKILL the process's whole group, which the
	// command in systemd's namespaces is still in (2026-11-21). Should
	// nsenter alone die, the command is sent SIGTERM (setpriv's parent-death
	// signal, TERM so a script's traps clean up); another account's
	// command, which runuser forks and a change of account clears it for,
	// is given its own, tied to runuser's. It fails with enterNotRunning
	// while systemd does not run.
	enterScript = `p=
for d in /proc/[0-9]*; do
	[ "$(cat "$d/comm" 2>/dev/null)" = systemd ] || continue
	grep -Eq '^NSpid:[[:space:]]+[0-9]+[[:space:]]+1$' "$d/status" 2>/dev/null || continue
	p=${d#/proc/}
	break
done
if [ -z "$p" ]; then echo "systemd is not running in the guest" >&2; exit 125; fi
exec nsenter --target "$p" --all -- setpriv --pdeathsig TERM -- sh -c 'cd -- "$1" || exit; u=$2; shift 2
if [ -z "$u" ]; then exec "$@"; fi
exec runuser -u "$u" -- setpriv --pdeathsig TERM -- env HOME="$(getent passwd "$u" | cut -d: -f6)" "$@"' sh "$@"`
	enterNotRunning = 125

	// settleScript asks systemd how far its boot is: running, degraded and
	// maintenance are settled.
	settleScript = `exec systemctl is-system-running`
)

// ensureSystemd makes the sandbox's guest one whose every boot runs systemd
// (ensureScript), and fails naming what the guest lacks. fenced is a VM
// whose egress is fenced, whose install fails fast.
func (a *api) ensureSystemd(ctx context.Context, id, name string, fenced bool) error {
	fence := ""
	if fenced {
		fence = "fenced"
	}
	line := "exec " + quoteAll([]string{"sh", "-c", ensureScript, "sh", systemdScript, fence})
	code, _, stderr, err := a.run(ctx, id, line, "/", nil)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d: %s", code, bytes.TrimSpace(stderr))
	}
	if err != nil {
		return fmt.Errorf("archil: systemd in %s: %w", name, err)
	}
	return nil
}

// settleLimit bounds a start's wait for systemd to settle.
var settleLimit = 2 * time.Minute

// waitSystemd waits until systemd runs in the guest and has settled, asking
// it every 200 ms, so a command run after Start reaches it and the units
// its boot starts.
func (m *guestMachine) waitSystemd(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, settleLimit)
	defer cancel()
	last := ""
	for {
		var out, stderr bytes.Buffer
		code, err := machine.Run(ctx, m, script(settleScript), nil, &out, &stderr)
		state := strings.TrimSpace(out.String())
		switch {
		case err != nil && ctx.Err() == nil:
			last = err.Error()
		case err != nil:
		case state == "running" || state == "degraded" || state == "maintenance":
			return nil
		case code == enterNotRunning:
			last = strings.TrimSpace(stderr.String())
		default:
			last = strings.TrimSpace(state + " " + stderr.String())
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("archil: systemd in %s did not settle: %w (last: %s)", m.name, ctx.Err(), last)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
