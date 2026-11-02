# hcs driver (Windows host, Windows guest): implementation brief

**Scope:** `pkg/machine/hcs` and the Windows-only parts of `pkg/guest`. Nothing
above the `machine.Driver` seam should need to change. If it does, that is a
design finding: raise it rather than work around it. `hcs.Hypervisor` is a
`local.Hypervisor`: `pkg/machine/local` keeps its VMs, committed or not, in
its directory (`--root`, `$DISCO_VM_ROOT`, or `%LOCALAPPDATA%\disco-vm`) and
runs each started VM under a shim, so the hypervisor installs, copies
(Prepare), boots, freezes (Commit), and destroys disks in the directories it
is given (ADR 26-10-23-417, 26-11-03-606). hcs has no base: a fresh VM is
refused, and a configured `machine.Config.Base` is an error; every VM comes
from an install or from a committed VM. Its disks chain (`Chains`): a VM's
disk is a differencing VHDX over the disk of the committed VM it was created
from, which stays where it was made, so `local` hands Prepare, Boot, and
Destroy the whole chain, and refuses to remove a committed VM another VM's
disk still needs (`machine.ErrInUse`).

**Done means:**

1. `machinetest.Run` passes against the hcs driver on a real Windows host.
2. `internal/e2e` passes with `DISCO_VM_DRIVER=hcs` and a real install.
3. `disco-vm install NAME` makes a committed Windows VM from a retail ISO, a
   VM created from it is shaped through `exec` and committed, and removing
   the first committed VM is refused with `ErrInUse` while a VM created from
   the second, whose disk differences over both, exists.

## Status

Implemented. On a Windows 11 Pro host (elevated, Virtual Machine Platform, no
Hyper-V role needed) with a Windows 11 Pro 25H2 guest:

- (1) passed before ADR 26-10-23-417: the suite ran in about 45 s. Since, it
  is `DISCO_VM_HCS_BASE=<a committed VM an install made> DISCO_VM_TEST_BINARY=<a
  disco-vm.exe> go test ./pkg/machine/hcs`, and has not run.
- (2) passed before ADR 26-10-23-417: `DISCO_VM_DRIVER=hcs DISCO_VM_E2E_ROOT=<root>
  DISCO_VM_E2E_ISO=<iso> go test ./internal/e2e` from an elevated shell. The
  first run installs and tags `e2e/base` (about 15 min in all); later runs
  reuse it.
- (3) a build of the since-removed `docs/examples/windows.yaml` installed
  from the retail ISO (the 128 GiB fixed disk alone takes over ten minutes
  to allocate), and its OpenSSH step downloaded from Windows Update through
  the guest's NIC. `disco-vm install NAME` and a chain of committed VMs have
  not run here; `local`'s refusal to remove a committed VM another VM's disk
  needs is proven with a chaining stand-in on the fake.

What building it found, beyond the recipes below:

- **`ShouldTerminateOnLastHandleClosed: true`.** Each VM lives exactly as long
  as the shim holding its handle, so a crashed shim cannot orphan a system and
  no orphan sweep is needed.
- **Every VM gets a NIC** on an HNS network of the driver's own (`disco-vm`,
  ICS, Flags 35, as sandboxi measured), since the example builds download.
  An instance asks for the same MAC on every boot.
- **Commit freezes the disk where it is**, so its parent locator, written
  when Prepare made it over its parent's final path, is never rewritten; a
  committed VM keeps `disk.vhdx` alone, its guest and runtime state and its
  MAC removed. A fork clone from before fork clones were removed differences
  over the template disk hard-linked into its own directory, so it cannot be
  committed; one never booted is refused at start.
- **Install waits for Setup through the agent**, which starts during Setup:
  `ImageState` reaching `IMAGE_STATE_COMPLETE`, then `explorer.exe` for the
  automatic logon, then OOBE's completion (the `OOBECompleteTimestamp` value
  under `HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\OOBE\OOBECompleteTimestamp`),
  then a minute to settle. The desktop is not the end of OOBE: at the first
  networked logon it installs a Zero Day Patch and reboots
  (`CloudExperienceHostBroker`, event 1074). A base cut before that made every
  instance reboot itself a minute or ten after starting, which killed a
  build's step midway. Windows Update policy does not stop it; waiting for it
  does.
- **Run as user is LogonUser**, with the password Install's unattend gives
  the account, its own name (an image from before has a blank one, which is
  tried second; `LimitBlankPasswordUse` is turned off in the offline hive),
  the linked elevated token, the profile loaded, and the account granted the
  agent's window station and desktop (without which anything that loads
  user32 fails with 0xC0000142). A TTY session as the account gets a console
  of the account's own (`disco-vm console-host`): one the SYSTEM agent made
  could be written to but not read back, which broke every PowerShell
  progress bar.
- **Nested virtualization is on** for every VM (`Processor.ExposeVirtualizationExtensions`),
  so a guest runs WSL 2, Hyper-V, Windows Sandbox, or Docker Desktop. Memory
  stays `Virtual`-backed, committed as the guest touches it: nesting rules out
  only dynamic memory, which these VMs do not use. Proven by hand on Win 11
  (2026-11-02): VBS running in the guest, and a WSL 2 Debian booting.
- **A window is the guest's own Remote Desktop, or the agent's stream of its
  desktop** (ADR 26-11-02-274), never the VM worker's video console, which
  serves RDP only on some host editions (a Windows 11 Home host closes its
  pipe without a byte). `View` asks the guest through the agent whether
  `127.0.0.1:3389` answers an RDP request. If so, `mstsc` opens full screen
  at the host's resolution on a loopback port relayed to it through the
  agent's `pipe`, signed in as the image's user by a Domain Password
  credential for `TERMSRV/127.0.0.1` named `COMPUTER\user` (mstsc ignores a
  password in the `.rdp` file and a generic credential). Otherwise a window
  of disco-vm's own shows the stream on `guest.DisplayPort`, which the agent
  serves from `disco-vm display-host` in the console session (sandboxi's
  guestfb: GDI capture, changed rows as raw BGRA, the cursor's shape,
  `SendInput`). `DISCO_VM_HCS_VIEW=display` skips Remote Desktop. Documents
  keep `VideoMonitor`, `Keyboard` and `Mouse`, as the guest's adapter and
  input. `disco-vm gui` opens a window on a running instance; `run --gui`
  and `start --gui` open it from the shim once the guest is up, and again
  after it restarts. `install --gui` shows nothing: Setup has no logged-on
  desktop. Install turns Remote Desktop on except on the `Core*` (Home)
  editions, its firewall left closed. Checked by hand on a Win 11 Pro guest
  on a Home host (2026-11-02): the Remote Desktop window signs in with no
  prompt at the host's size, and the stream's window shows the desktop and
  takes the mouse and keyboard.
- **A TTY session ignores end of input.** The client closes stdin when it has
  none, and for a terminal that closed the whole pseudo console before the
  process started (0xC0000142 again). This was a neutral server bug; on Unix it
  hung up the pty.

`pkg/machine/hcs/hcs_windows_test.go` is gated on an env var naming a
committed VM that an install made (so CI skips it), and calls
`machinetest.Run` with `Config.From`. That way an OS install isn't redone on
every run.

**Prior art:** every recipe below is proven in Rust in the sandboxi repository
(`crates/sandboxw-core`, `crates/hcs-sys`, `crates/guestexec`, `crates/guestfb`).
Port the behavior, not the structure.

## Hard rules

- **Pure HCS only:** `vmcompute.dll` / `computecore.dll`. No vmms/Hyper-V
  management, no CmService/Windows Sandbox, no WSL. It must work on Home. Both
  the Hyper-V role and Windows Sandbox may be absent.
- **Never mount a disk on the host except inside Install**, and there only with
  the guards from sandboxi's `scripts/build-image.ps1`:
  - a fixed-size VHDX
  - `Assert-OurDisk` verifying disk identity before any destructive cmdlet
  - a free-space preflight
  - a breadcrumb file plus cleanup

  A dynamic VHDX expanding under DISM caused vhdmp bus resets that hung the
  machine. Every other path hands VHDX paths to HCS and lets the guest mount
  them.

## Method by method

**Check:** confirm vmcompute is available and the process is elevated. Name
the fix in the error: enable Virtual Machine Platform, or run elevated.

**Install(spec, dst):** retail ISO to a bootable VHDX in `dst.Dir`, the
committed VM's own directory. The flow is the one sandboxi's
`build-image.ps1` proved:

1. Create a fixed VHDX.
2. Partition it GPT: ESP 300 MB FAT32, MSR 16 MB, Windows. `Initialize-Disk`
   auto-creates an MSR, so clear it first.
3. `Expand-WindowsImage` the edition from `install.wim`.
4. Run the **host's** `bcdboot` (the image's own exits 0xC0E90002 silently),
   check its exit code, and assert that `\EFI\Microsoft\Boot\BCD` exists.
5. Write `unattend.xml`.
6. **Bake in the agent:** copy `spec.Agent` into the image and register
   `disco-vm guest` as an auto-start SYSTEM service through the offline SYSTEM
   hive, the same way guestexec was baked in.

Then boot once through Windows Setup (about 4.6 min) and finish only after an
orderly shutdown through the agent, so the committed VM is past Setup. Do the
PowerShell by embedding scripts (`//go:embed`), as sandboxi does.

**Prepare(inst):** create a differencing VHDX in `inst.Dir` whose parent is
`inst.Parent().Dir`'s disk: the committed VM it is created from, last in
`inst.Chain`. With no `Chain` (a fresh VM) it is refused: hcs has no base.

- Parents record absolute paths. Never `os.Rename` a differencing VHDX across
  directories: the moved file names itself as its parent and the chain breaks
  (0xC03A000E).
- Call `HcsGrantVmAccess` on **every** file in the chain, grandparents included.
- go-winio `vhd.CreateDiffVhd` and hcsshim's patterns are fine to use; hcsshim's
  `internal/*` packages are not importable, so copy what you need.

**Boot(inst, opts):** `HcsCreateComputeSystem` plus `HcsStartComputeSystem`.
Three document fields silently prevent boot:

- **Omit** `Chipset.Uefi.BootThis`. Setting it gives Worker event 18603.
- `SecureBootTemplateId` requires `ApplySecureBootTemplate: "Apply"`. Without
  it you get 18604.
- `StopOnReset` must be **false**, or Setup's reboots shut the VM down (18515).

Also use forward-slash paths, no BOM, and 6 vCPUs by default (1 vCPU was
unusably slow).

List `00001C84-FACB-11E6-BD58-64006A7986D3` (port 7300, via
`winio.VsockServiceID`) in the HvSocket `ServiceTable`, or the agent's bind is
refused. List 7302 (`guest.ServicePort`, `00001C86-…`) too: it is every
instance's service, which the shim serves on the host's loopback by dialing
it over hvsocket. List 7301 (`guest.DisplayPort`, `00001C85-…`), the
agent's display stream.

`Machine.Dial(port)` is `winio.Dial` of an `HvsockAddr{VMID: <RuntimeId>,
ServiceID: VsockServiceID(port)}`. `Done` fires from an HCS system-exit
notification, not from polling.

**Commit(inst):** the VM is stopped (the engine has already shut it down
through the agent). Freeze it in place: refuse a running system and a fork
clone, and remove its guest and runtime state and `state.json`. Its
differencing VHDX stays where Prepare made it, so the rename rule above never
applies.

**Destroy:** remove files. For a running or orphaned compute system,
terminate it by ID first. A crashed shim can orphan a registered system, so
do an orphan sweep in Check or at shim start. The local driver refuses to
destroy a committed VM while another VM's disk differences over it.

## No fast clones

Every clone is cold (ADR 26-10-15-851). A live-template fork was built here
and removed with warm stages; it is in the history before that ADR. What it
measured, for a return: a template frozen with `HcsSaveComputeSystem`
`{"SaveType":"AsTemplate"}` forks clones through
`RestoreState.TemplateSystemId` in 0.44–0.86 s, only through the handle of
the process that created it (0xC0370400 otherwise), and the save needs
commit headroom (0x800705AA otherwise). Suspend-to-disk (save a `.vmrs`, then
restore) did **not** work on image VMs: HCS cold-booted instead.

## Guest-side work (`pkg/guest`, Windows files)

- **ConPTY** in `pty_windows.go`: `CreatePseudoConsole`, then `CreateProcess`
  with `PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE`. Close the pseudo console after the
  process exits, or the output pipe never reaches EOF. guestexec's `OP_CONSOLE`
  is the proven Rust version.
- **runAs** in `user_windows.go`: use `WTSQueryUserToken` for a logged-on user,
  or `LogonUser` with credentials the unattend created.
- **hvsocket listen** in `listen_windows.go`: this has to be verified inside a
  guest, including whether `VMID: HvsockGUIDWildcard()` is the right bind for
  host-initiated connections.
- **Service wrapper:** `disco-vm guest` must run under the SCM. Add a
  `golang.org/x/sys/windows/svc` handler when running as a service.
- **Display:** done, as guestfb's protocol on port 7301 with GDI capture
  rather than Desktop Duplication, and the guest's own Remote Desktop where it
  has one (ADR 26-11-02-274).

## Known numbers (this hardware)

| | time |
|---|---|
| first boot, full Setup | 4.6 min (sandboxi); 3.9–5.5 min to a settled desktop (disco-vm) |
| boot from a captured disk | about 37 s to desktop; `run` to the agent answering in 5.8 s |
