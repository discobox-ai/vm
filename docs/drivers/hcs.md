# hcs driver (Windows host, Windows guest): implementation brief

**Scope:** `pkg/machine/hcs` and the Windows-only parts of `pkg/guest`. Nothing
above the `machine.Driver` seam should need to change. If it does, that is a
design finding: raise it rather than work around it.

**Done means:**

1. `machinetest.Run` passes against `hcs.Driver` on a real Windows host.
2. `internal/e2e` passes with `DISCO_VM_DRIVER=hcs` and a real image.
3. `examples/windows.yaml` builds.

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

## Fork (CloneMode "fork")

This is the live-template model. Boot a template from the image, wait for the
agent, then call `HcsSaveComputeSystem` with `{"SaveType":"AsTemplate"}`
(which **freezes** it for forking; it does not write a restorable file). Clones
are then created with `RestoreState.TemplateSystemId`. That measured 0.44–0.86 s
create+start, about 1 s per clone end to end.

- The template must stay resident in the process that created it. In disco-vm
  that is a **template shim**: an instance whose shim holds the frozen template
  and forks clones on request.
- Suggested shape: `disco-vm run --mode fork IMAGE` finds or starts the
  image's template shim and asks it over its control API.
- Keep the design question open. If it needs an engine change, propose one.
- `AsTemplate` needs commit headroom and fails with 0x800705AA otherwise.

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
| first boot, full Setup | 4.6 min |
| boot from a captured disk | about 37 s to desktop |
| fork a clone | about 1 s (0.44–0.86 s raw) |
