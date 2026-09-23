# hcs driver (Windows host, Windows guest): implementation brief

**Scope:** `pkg/machine/hcs` and the Windows-only parts of `pkg/guest`. Nothing
above the `machine.Driver` seam should need to change. If it does, that is a
design finding: raise it rather than work around it.

**Done means:**

1. `machinetest.Run` passes against `hcs.Driver` on a real Windows host.
2. `internal/e2e` passes with `DISCO_VM_DRIVER=hcs` and a real image.
3. `examples/windows.yaml` builds.

## Status

Implemented. On a Windows 11 Pro host (elevated, Virtual Machine Platform, no
Hyper-V role needed) with a Windows 11 Pro 25H2 guest:

- (1) passes: `DISCO_VM_HCS_BASE=<layer>\payload go test ./pkg/machine/hcs`
  runs the whole suite, fork included, in about 45 s.
- (2) passes: `DISCO_VM_DRIVER=hcs DISCO_VM_E2E_ROOT=<root>
  DISCO_VM_E2E_ISO=<iso> go test ./internal/e2e` from an elevated shell. The
  first run installs and tags `e2e/base` (about 15 min in all); later runs
  reuse it. TestWarm takes its fork branch: a template never runs out.
- (3) passes: `examples/windows.yaml` builds from the retail ISO (the 128 GiB
  fixed disk alone takes over ten minutes to allocate), its OpenSSH step
  downloads from Windows Update through the guest's NIC, and a second build
  is fully cached.

What building it found, beyond the recipes below:

- **Clones are forked by the warm shim, not the instance's shim.** A template
  can only be forked through the handle of the process that created it
  (0xC0370400 otherwise, as sandboxi found). So the warm shim serves a named
  pipe (admins and SYSTEM only); an instance shim's fork Boot sends the
  clone's files and NIC, the warm shim creates, starts, and hot-adds the NIC on
  its own handle, the instance shim opens the clone by ID, and the warm shim
  lets go. The clone then belongs to the instance shim like any other VM.
- **A clone differences over the template's disk**, whose memory it forks. The
  template's disk is frozen with it, and each clone hard-links it into its own
  directory, so a stopped clone still boots (cold) after the image is cooled.
- **Cooling a template did not end its running clones** here, contrary to
  sandboxi's note. They kept running and shut down in order afterwards.
- **`ShouldTerminateOnLastHandleClosed: true`.** Each VM lives exactly as long
  as the shim holding its handle, so a crashed shim cannot orphan a system and
  no orphan sweep is needed.
- **Every VM gets a NIC** on an HNS network of the driver's own (`disco-vm`,
  ICS, Flags 35, as sandboxi measured), since the example builds download.
  An instance asks for the same MAC on every boot.
- **Commit moves the disk, then rewrites its parent locator**
  (`SetVirtualDiskInformation`, parent path with depth), so a move across
  directories cannot break the chain. A fork clone cannot be committed; the
  builder always clones cold.
- **Install waits for Setup through the agent**, which starts during Setup:
  `ImageState` reaching `IMAGE_STATE_COMPLETE`, then `explorer.exe` for the
  automatic logon, then OOBE's completion (the `OOBECompleteTimestamp` value
  under `HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\OOBE\OOBECompleteTimestamp`),
  then a minute to settle. The desktop is not the end of OOBE: at the first
  networked logon it installs a Zero Day Patch and reboots
  (`CloudExperienceHostBroker`, event 1074). A base cut before that made every
  instance reboot itself a minute or ten after starting, which killed the
  `examples/windows.yaml` build mid-step. Windows Update policy does not stop
  it; waiting for it does.
- **Run as user is LogonUser**, with the blank password Install's unattend
  gives the account (`LimitBlankPasswordUse` is turned off in the offline
  hive), the linked elevated token, the profile loaded, and the account
  granted the agent's window station and desktop (without which anything that
  loads user32 fails with 0xC0000142).
- **Display is the VM's basic video console**, as sandboxi proved: every
  document has `VideoMonitor` (with `ConnectionOptions` naming a pipe per VM
  and the current user's SID), `Keyboard` and `Mouse`, so a template and its
  clones agree on devices. A fork clone restores fine with its own console
  pipe. With `BootOptions.GUI` the driver relays a loopback port to the pipe
  and opens `mstsc` on it, titled `BootOptions.Title` (standard RDP security
  only, so CredSSP is off in the `.rdp` file); the window closes when the VM
  stops, and closing it leaves the VM running. `InstallSpec.GUI` shows the
  install's first boot, Windows Setup included. Checked through the CLI:
  `run --gui` and `start --gui` (window in the shim; a plain `start` opens
  none), a fork clone with `--gui`, and `build --gui` with a reboot step (one
  window per boot, never two at once, none left after). `TestConsole` checks the console answers RDP on a cold VM and
  a fork clone. Not checked by a machine: the pixels after clicking through
  mstsc's warning about the unsigned `.rdp` file, which sandboxi did by hand.
- **A TTY session ignores end of input.** The client closes stdin when it has
  none, and for a terminal that closed the whole pseudo console before the
  process started (0xC0000142 again). This was a neutral server bug; on Unix it
  hung up the pty.

Add `pkg/machine/hcs/hcs_test.go`, gated on an env var naming a base layer (so
CI skips it), that calls `machinetest.Run` with `Config.Base`. That way an OS
install isn't redone on every run.

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

**Install(spec, dst):** retail ISO to a bootable VHDX in `dst.Dir`. The flow is
the one sandboxi's `build-image.ps1` proved:

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

Then boot once through Windows Setup (about 4.6 min) and commit only after an
orderly shutdown through the agent, so the base layer is past Setup. Do the
PowerShell by embedding scripts (`//go:embed`), as sandboxi does.

**Prepare(inst):** create a differencing VHDX in `inst.Dir` whose parent is
`inst.Parent().Dir`'s disk.

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
refused. Add 7301 when display lands.

`Machine.Dial(port)` is `winio.Dial` of an `HvsockAddr{VMID: <RuntimeId>,
ServiceID: VsockServiceID(port)}`. `Done` fires from an HCS system-exit
notification, not from polling.

**Commit(inst, dst):** the instance is stopped (the engine has already shut it
down through the agent). Move its differencing VHDX into `dst.Dir` within one
volume, rewriting nothing. Then Destroy. Mind the rename rule above: create the
child in a place from which the final move is a same-directory rename, or
merge/copy it.

**Destroy / DeleteLayer:** remove files. For a running or orphaned compute
system, terminate it by ID first. A crashed shim can orphan a registered
system, so do an orphan sweep in Check or at shim start.

## Fork (CloneMode "fork"), through machine.Warmer

This is the live-template model. Boot a template from the image, wait for the
agent, then call `HcsSaveComputeSystem` with `{"SaveType":"AsTemplate"}`
(which **freezes** it for forking; it does not write a restorable file). Clones
are then created with `RestoreState.TemplateSystemId`. That measured 0.44–0.86 s
create+start, about 1 s per clone end to end.

It fits the seam as `machine.Warmer` (design decision 7):

- **Warm(spec):** boot the template from `spec.Chain` at `spec.CPUs` and
  `spec.Memory`, wait for the agent, and freeze it. Return a `machine.Stage`
  that owns it. The template must stay resident in the process that created
  it, and the engine already runs Warm in a **warm shim** (`disco-vm shim
  --warm <layer>`) that holds the Stage until `warm --rm`. Write whatever a
  clone needs to find the template (its system ID) into `spec.Dir`. Ignore
  `spec.Count`: one template serves any number of clones.
- **Warmth(spec):** `{Fork, -1}` while the template system exists and is
  frozen, and zero otherwise, such as after the warm shim crashed.
- **Prepare(inst) with Mode fork:** create the clone's disk the way a cold
  Prepare does, and read the template's ID from `inst.WarmDir`. Return
  `machine.ErrNotWarm` if the template is gone. The engine then clones cold
  when the mode was auto.
- **Boot(inst) with Mode fork:** create the compute system with
  `RestoreState.TemplateSystemId`. It belongs to vmcompute, not the warm shim,
  so the instance's shim can own it like any other VM. Only the first boot
  after Prepare forks. A later start of the same instance is cold.
- **Stage.Close / Cool(spec):** terminate the template system, then remove
  what Warm wrote.
- `AsTemplate` needs commit headroom and fails with 0x800705AA otherwise.
  Surface that from Warm with the fix in the message.
- Proof: the `warm` subtest of `machinetest.Run` forks a clone of a warmed
  committed layer and checks it sees the layer's writes.

Suspend-to-disk (save a `.vmrs`, then restore) did **not** work on image VMs:
HCS cold-booted instead. Don't promise `resume` on hcs.

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
- **Display (later):** the guestfb protocol on port 7301 (Desktop Duplication
  capture plus SendInput, in the interactive session), and the host viewer.

## Known numbers (this hardware)

| | time |
|---|---|
| first boot, full Setup | 4.6 min (sandboxi); 3.9–5.5 min to a settled desktop (disco-vm) |
| boot from a captured disk | about 37 s to desktop; `run` to the agent answering in 5.8 s |
| fork a clone | about 1 s (0.44–0.86 s raw); `run` to the agent in 3.5–9 s with six VMs up |
