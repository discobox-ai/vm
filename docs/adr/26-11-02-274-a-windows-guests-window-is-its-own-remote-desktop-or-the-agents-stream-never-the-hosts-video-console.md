# 26-11-02-274 — A Windows guest's window is its own Remote Desktop or the agent's stream, never the host's video console

- **Status**: Accepted
- **Date**: 2026-11-02

## Context

hcs showed a guest's display as the VM's basic video console: the VM worker
(`vmwp.exe`) serves the emulated video card as RDP on a named pipe the VM's
document names (`VideoMonitor.ConnectionOptions`), and the shim relayed a
loopback port to it for `mstsc`. sandboxi proved the recipe on Windows 11 Pro.

On a Windows 11 Home host (build 26300) the worker accepts the pipe and closes
it without a byte, to any RDP request, on any VM, with nested virtualization
on or off, and with the code from before this change: the console is the
host's, and the host's edition does not serve it. disco-vm targets hosts with
only Virtual Machine Platform, Home among them, so its display cannot depend
on the host's RDP server.

The window could also open only at boot: the shim's relay accepted one
connection and closed, so one failed mstsc attempt meant restarting the VM to
see it.

Two ways to a guest's desktop need nothing of the host's but hvsocket, which
the agent already rides:

- **The guest's own Remote Desktop**, on editions that host it (Pro and up).
  It is full RDP: clipboard, dynamic resolution, audio, the host's scale.
- **sandboxi's guestfb**: the desktop captured in the guest with GDI `BitBlt`
  (not Desktop Duplication: it works on every edition and survives GPU
  partitioning), sent as raw BGRA rectangles of what changed, with the cursor
  as a standard shape and input injected with `SendInput`. It works on Home
  guests and with Remote Desktop off.

## Decision

1. **A window is the guest's own Remote Desktop where the guest serves one,
   and otherwise the agent's stream of its desktop.** hcs's `View` asks the
   guest, through the agent, whether `127.0.0.1:3389` answers an RDP
   connection request. If it does, it opens `mstsc` full screen at the host's
   resolution on a loopback port that relays each connection to the guest's
   3389 through the agent's `disco-vm pipe`, signed in as the image's user: a
   Domain Password credential for `TERMSRV/127.0.0.1`, for the length of the
   session, as mstsc's own "Remember me" makes it, and none written over one
   the user saved. Otherwise it dials `guest.DisplayPort` (7301) and shows
   the stream in a window of disco-vm's own. `DISCO_VM_HCS_VIEW=display`
   skips Remote Desktop, to try a Home guest's path on any guest.
2. **The display stream is one stream on 7301**, framed as exec is: the guest
   sends `DisplayScreen` first, then rectangles and a flush per frame, and
   `DisplayCursor` on a change; the viewer sends `DisplayInput`. The agent is
   a SYSTEM service in session 0, so it starts `disco-vm display-host` in
   the console session, as the user logged on to it and elevated, and relays.
   A session Remote Desktop left disconnected is brought back to the console
   first, as `tscon ID /dest:console` does. One viewer at a time.
3. **A window opens on a running instance from any process**:
   `disco-vm gui INSTANCE` (`Engine.GUI`, the driver's `machine.Viewer`,
   which the local driver gives through the shim to a hypervisor's
   `local.MachineViewer`). `start --gui` and `run --gui` open the same window
   from the shim once the guest is up, and again after the guest restarts.
   vz opens its native window only at boot, and refuses `gui` as not
   implemented yet.
4. **The VM worker's console is not used.** Documents keep `VideoMonitor`,
   `Keyboard`, and `Mouse` (the guest's display adapter and input), without
   `ConnectionOptions`.
5. **An install's window is the agent's stream, from the first logon.**
   `install --gui` keeps one open from OOBE's automatic logon, through the
   Zero Day Patch reboot, to the shutdown that commits the VM; Setup
   before it has no desktop to show. Never the guest's Remote Desktop: its
   sign-in would take the console session from the logon Setup is
   finishing. **`disco-vm gui --install [NAME]`** opens the same window from
   another process, on an install started without `--gui`, by the name of
   the VM it makes: the local driver marks the VM's directory as an install
   in progress while it runs (`installing.json`, its record), and hcs records the install's VM beside its disk (`.install-vm`: the
   installing process and the VM's runtime ID). The window dials the guest
   over hvsocket by that ID, through no shim and without opening the compute
   system, whose last handle closing ends the VM: the install stays the VM's
   only owner. `machine.Viewer` gains `ViewInstall`, and
   `local.MachineViewer` with it.
6. **Install makes the image's account a password, its own name, and turns
   Remote Desktop on where the edition hosts it** (not the `Core*` editions),
   with its firewall rules left closed: the agent reaches it from inside the
   guest. The agent's `LogonUser` tries the name, then a blank password, for
   images from before.

## Alternatives rejected

- **Keep the VM worker's console.** It works only where the host's edition
  serves it, and it is the one host dependency disco-vm otherwise avoids.
  Its one strength, showing Setup and the screen before a logon, is not worth
  a display that is dark on Home hosts.
- **Only guestfb.** Remote Desktop, where the guest has it, is better in
  every way that shows: clipboard, sound, crisp scaling, dynamic resolution.
- **Only Remote Desktop.** Home guests do not host it.
- **Turn NLA off so mstsc uses a stored password.** NLA was never what made
  mstsc ask: it ignores a password in the `.rdp` file and a generic
  credential, and takes a Domain Password one named `COMPUTER\user`. No
  guest setting needs to change.
- **`gui --install` opening the install's compute system** to watch it stop.
  The document sets `ShouldTerminateOnLastHandleClosed`, so a viewer's
  handle would keep the VM alive past an installing process that died. The
  viewer watches the installing process and its record instead.
- **Three hvsocket channels, as guestfb has.** One stream through the shim's
  existing `Dial` needs no new seam, and one ServiceTable entry.
- **Desktop Duplication.** sandboxi measured it losing its keyed mutex under
  GPU partitioning; `BitBlt` works everywhere.

## Consequences

- hcs's display works on Home hosts, and on Home guests.
- `disco-vm gui` can be run whenever the instance runs, as often as wanted.
- Nothing shows Windows Setup, a blue screen, or the sign-in screen of a
  guest nobody is logged on to. The image logs its user on automatically,
  so an install shows from its first logon.
- The stream is raw and uncompressed: fine over hvsocket, not over a network.
- mstsc still warns once about the unsigned `.rdp` file.
- `guest.DisplayPort` is in every hcs VM's ServiceTable.

## Deferred

- **Signing the `.rdp` file**, to lose mstsc's publisher warning: when a
  certificate to sign with is at hand.
- **A window on a running vz instance**: when the shim can open one after
  boot.
- **Compression of the display stream**: if it ever leaves the host.
