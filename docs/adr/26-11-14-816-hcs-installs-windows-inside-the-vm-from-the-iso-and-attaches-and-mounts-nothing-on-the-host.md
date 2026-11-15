# 26-11-14-816 — hcs installs Windows inside the VM from the ISO, and attaches and mounts nothing on the host

- **Status**: Accepted
- **Date**: 2026-11-14
- **Amended**: 2026-11-15, by the implementation (discobox-ai/vm#127), before
  anything shipped against it: §3 is shape (a), with what it took to boot;
  §4 is the Deferred fallback, no vTPM, because HCS gives none on a host
  without the Hyper-V role; §5 bypasses the TPM check with the CPU check;
  §6 says how the scripts find the medium. See
  Deferred's last item.

## Context

hcs's `Install` (`pkg/machine/hcs/install_windows.go`) builds the Windows
disk on the host, then boots it once:

1. `createFixed` makes a fully allocated `install.vhdx` of `spec.DiskBytes`
   (64 GiB by default). `docs/drivers/hcs.md` records that a 128 GiB one
   took over ten minutes.
2. `writeDisk` attaches it to the host (`AttachVirtualDisk`, `attach` in
   `vhd_windows.go`), drops a `.attached` breadcrumb, and runs the embedded
   `install.ps1`. The script adds a Defender exclusion, mounts the ISO
   (`Mount-DiskImage`), reads the editions with `Get-WindowsImage`,
   partitions the disk (ESP, MSR, Windows), applies the image with the
   host's `dism /Apply-Image`, writes `unattend.xml`, copies the agent in,
   loads the image's SYSTEM hive (`reg load`) to register the agent's
   service, `LimitBlankPasswordUse` and Remote Desktop, and runs the host's
   `bcdboot`.
3. `createDynamicFrom` copies the fixed disk into a dynamic `disk.vhdx`,
   and the fixed one is removed.
4. `firstBoot` boots `disk.vhdx`, and `waitSetup` waits through the agent
   for Setup, the automatic logon, and OOBE. The guest is then shut down
   through the agent, and the disk is the committed VM.

Steps 1–3 are the only place disco-vm puts a disk on the host's storage
stack. The hcs brief allows it there alone, under guards from sandboxi:
a fixed disk, `Assert-OurDisk`, a free-space preflight, a breadcrumb, and
the Defender exclusion. The disk is fixed only because of the attach: a
dynamic disk expanding under the host's DISM through `vhdmp` hung sandboxi's
host. The compaction in step 3 exists only to undo the fixed allocation. The
guards have not been enough:

- On 2026-11-11 (discobox-ai/vm#74), `disco-vm install` with a 64 GiB fixed
  disk on D: froze a Windows 11 Pro host with two DRAM-less NVMe drives.
  Within two minutes of the attach, the System log shows three `vhdmp`
  event 129s, each a reset after an I/O to the attached disk timed out.
  Everything else that touched the host's disks then waited behind it: a
  `git commit` on another drive, Explorer, and later the Defender service
  (event 7011). The machine had to be restarted.
- `install.ps1`'s own header records the same failure in sandboxi.

Since then, the owner's rule for that host is that nothing mounts on it.

Every other path already hands VHDX paths to HCS and lets the guest write
them. Every VM runs on a dynamic disk the guest grows. That includes
today's own first boot, which runs Setup's specialize pass and OOBE on the
dynamic `disk.vhdx`. Only the host attach is unique to install. A Hyper-V
install is the usual way to do it instead: the VM boots the retail ISO from
a virtual DVD, and Setup partitions and applies the image inside the guest.

Moving Setup into the VM brings three things today's path never meets:

- **The boot prompt.** A retail ISO's El Torito image (`efisys.bin`,
  `cdboot.efi`) asks to "press any key to boot from CD or DVD". It falls
  through to the next device when no key comes. HCS has no call that sends
  a key. The VM worker's video console serves RDP only on some host
  editions (ADR 26-11-02-274), and hcs must work on Home.
- **Windows 11 Setup's hardware check.** Setup in WinPE refuses a machine
  without TPM 2.0 and Secure Boot (and checks RAM and CPU). The VM document
  (`newDocument`, `config_windows.go`) applies the Secure Boot template but
  has no vTPM. Today's VMs run without one because DISM applies the image
  without asking. Where a vTPM would keep its state matters too: `Boot`
  replaces an instance's guest state file (`guest.vmgs`) at every boot
  (`freshState`, `hcs_windows.go`), and `Commit` removes it.
- **Reading the ISO without mounting it.** Microsoft's ISOs are UDF, with an
  ISO 9660 tree that holds only a README. The editions (install.wim's XML)
  and the boot files can be read in Go only through UDF.

The issue (discobox-ai/vm#127) asks for this install and for both installs
timed step by step on the same machine. It expects about 15 minutes today
(the first e2e install), with first boot at 3.9–5.5 minutes, and 15–20
minutes for the in-VM install. Neither number has been measured. This
decision rests on the host's safety, not on the time.

## Decision

### 1. Install attaches nothing to the host and mounts nothing on it

`Install` makes no fixed disk, attaches no VHDX, mounts no ISO, loads no
offline hive, and runs no host `dism`, `bcdboot`, or PowerShell. These go:

- `install.ps1`, `writeDisk`, `attach`, `createFixed`, `createDynamicFrom`;
- the Defender exclusion, `Assert-OurDisk`, and the breadcrumb.

The brief's hard rule loses its exception. It becomes: never mount a disk
on the host.

### 2. The VM boots from the retail ISO, attached as a read-only DVD, onto a dynamic disk

Install's boot document, and no other boot's, adds the person's ISO as an
`Iso` attachment on the VM's SCSI controller, read-only. It is granted to
the VM (`HcsGrantVmAccess`) and revoked afterwards, as `firstBoot` does with
its paths today. disco-vm never copies, rewrites, or remasters it.

The VM's disk is a new dynamic `disk.vhdx` of `spec.DiskBytes` at LUN 0,
created with `CreateVirtualDisk` and never opened by the host again. It is
the install's target and the committed VM's disk, as now. Nothing is
preallocated. With no host attach there is nothing for a fixed disk to
guard, and allocating one is the slowest step today.

The free-space check is a basic one. Install refuses to start unless the
root's volume has at least `minDiskBytes` (32 GiB) free, the smallest disk
hcs installs to. It does not reserve the disk's full size.

### 3. A medium disco-vm writes makes the first boot prompt-free and carries the answer file and the agent

disco-vm writes a second, small medium in the install's directory. It holds:

- `autounattend.xml` at its root, where Setup looks on removable media;
- the agent (`spec.Agent`), as `disco-vm.exe`;
- the specialize script (§6), and (amended) the windowsPE script this
  section describes below.

It is built in Go, in process, from bytes disco-vm has. No host tool runs,
and nothing is mounted. It is attached read-only to the install's boot and
removed with the install's working files. It is not part of the committed
VM.

This medium also makes the first boot prompt-free. It boots WinPE from the
retail ISO's own boot files, which disco-vm reads from the ISO's UDF tree
in Go. Then WinPE's Setup applies `install.wim` from the retail DVD. No key
is sent. The implementation took shape (a) (amended; the candidates were):

- **(a) An ISO** whose El Torito image is the retail ISO's
  `efi\microsoft\boot\efisys_noprompt.bin`, and which carries the ISO's
  `efi\microsoft\boot\bcd`, `boot\boot.sdi`, and `sources\boot.wim`. This is
  oscdimg's own no-prompt recipe. **Taken.** The medium is ISO 9660 with
  Joliet (no UDF), and carries the ISO's whole `efi` tree and its root
  `bootmgr.efi` too: the El Torito image's `cdboot` loads `\bootmgr.efi`
  from the disc's root, and without it fails back to the firmware at once
  (Hyper-V-Worker 18603, status 0x00010003/0x13, for every El Torito entry).
- **(b) A small VHD** with an ESP holding the ISO's `efi\boot\bootx64.efi`
  (Boot Manager, which never prompts; a USB stick boots the same way), the
  BCD, `boot.sdi`, and `boot.wim`. It has to enumerate after the target
  disk, or the answer file must pick the target by something other than its
  number.

Either way, the spike has to show that Setup finds `install.wim` on the
retail DVD, with `InstallFrom` pointing there if it does not find it on its
own. (Amended:) it does not look there on its own, and in WinPE the DVDs'
letters follow no rule. So a windowsPE `RunSynchronous` script on the medium
finds the DVD that holds `sources\install.wim` (or `install.esd`) and none of
disco-vm's files, and `subst`s it to `Z:`. `InstallFrom` names
`Z:\sources\install.wim`. Setup runs `RunSynchronous` before it reads the
image. After Setup's first reboot, it has put Windows Boot Manager first in
the firmware's order. Even if the retail DVD comes first, its prompt times
out to the disk.

### 4. No hcs VM has a vTPM; install bypasses Setup's TPM check (amended)

As proposed, every hcs VM was to have a vTPM (`SecuritySettings.EnableTpm`)
kept in its own `guest.vmgs`. The Deferred section said what to do if HCS
gives no vTPM on a host without the Hyper-V role: drop this section and
bypass the TPM check with `LabConfig`. That is the case, so this section is
now the fallback.

- **The evidence.** On a Windows 11 Pro host with Virtual Machine Platform
  and without the Hyper-V role (no `vmms` service, no `vmsp.exe`), a
  document with `EnableTpm: true` fails to start: HRESULT 0x80070002, and
  Hyper-V-Worker event 3040 ("could not initialize"). The same document
  without it starts. `HcsCreateEmptyGuestStateFile` works elevated, so the
  state file is not what is missing; the TPM's isolated worker is.
- **What install does.** The windowsPE script sets
  `HKLM\SYSTEM\Setup\LabConfig\BypassTPMCheck` with `BypassCPUCheck` (§5).
  Secure Boot stays on, as every hcs VM has it.
- **What stays as today.** `Boot` replaces `guest.vmgs` at every boot, and
  `Commit` removes it. A VM has no TPM, so nothing on its disk can be sealed
  to one. Install still sets `PreventDeviceEncryption` to 1, so a host that
  does give a TPM later cannot have the committed disk encrypted to it.

### 5. The answer file does what install.ps1 did, through Setup

disco-vm resolves the edition before the boot. It reads install.wim's (or
install.esd's) XML out of the ISO in Go and applies `install.ps1`'s rules:

- an index;
- a name, matched exactly and case-insensitively;
- with no edition given, the first whose name ends in ` Pro`, else the first.

An edition the ISO lacks is refused before anything boots, and the error
lists the ISO's editions, as now. Memory below 4 GiB, which Setup would
refuse, is refused before the boot too.

The answer file's passes:

- **windowsPE:**
  - `DiskConfiguration` wipes disk 0 and lays out today's partitions: a
    300 MB FAT32 ESP, a 16 MB MSR, and NTFS Windows for the rest.
  - `ImageInstall/OSImage/InstallFrom/MetaData` names `/IMAGE/INDEX`, and
    `InstallTo` names the Windows partition.
  - The EULA is accepted. No page of Setup shows.
  - The VM meets the Secure Boot and RAM checks itself. The TPM check is
    bypassed (`HKLM\SYSTEM\Setup\LabConfig\BypassTPMCheck`, §4, amended),
    and so is the CPU check (`BypassCPUCheck`): a VM's processor is the
    host's, which the VM cannot change, and today's install runs on hosts
    whose CPU Windows 11 does not list. The windowsPE script on the medium
    sets both, with the DVD's letter (§3).
- **specialize:** today's `ComputerName` and `TimeZone`, and the specialize
  script (§6) through `Microsoft-Windows-Deployment/RunSynchronous`.
- **oobeSystem:** today's settings unchanged: the hidden OOBE pages, the
  local administrator, automatic logon, and en-US.

### 6. The agent is installed inside the guest, during specialize

The specialize script runs as SYSTEM in the new Windows. (Amended:) the
answer file runs each script from whichever drive holds it, under
`disco-vm\`, so the scripts are the medium's marker. It copies
`disco-vm.exe` from beside itself to
`C:\disco-vm\disco-vm.exe`. Then it makes, online, what `install.ps1` made
in the offline hive, with the same values:

- the `disco-vm` service: `"C:\disco-vm\disco-vm.exe" guest`, auto-start,
  LocalSystem, and the same failure actions;
- `LimitBlankPasswordUse` set to 0;
- Remote Desktop turned on (`fDenyTSConnections` 0) unless the guest's own
  `EditionID` starts with `Core`, its firewall left closed as now.

It also sets `PreventDeviceEncryption` (§4). It logs to a file under
`C:\disco-vm`, so a failed install can be read from a committed disk or
from the agent.

(Amended:) the log file is all the scripts report to. A COM1 on a named
pipe, which install read into its log, was tried and dropped: nothing the
scripts wrote to it in WinPE or in specialize reached the host, though
installed Windows's writes do.

The agent runs from the boot after specialize. `waitSetup` already
tolerates no agent at first, and its stages do not change: `ImageState`,
then the automatic logon, then OOBE's completion, then a minute to settle.
A guest whose agent never comes up ends at `setupTimeout`, as now.

The installed guest is the same as today's, apart from the TPM. The agent
has the same path and service name, and the account, password, unattend
settings, and Remote Desktop setting are the same. Committed VMs made by
today's install need nothing.

### 7. Install's end, its window, and its records do not change

Install still ends only after an orderly shutdown through the agent, and
the committed VM is `disk.vhdx` alone. `install --gui` and
`gui --install` still open at the first logon, from the `.install-vm`
record. A guest with no desktop before that has nothing to show, as now.

### 8. Time is measured, and does not decide

Both installs are timed on the same hardware, step by step, as
discobox-ai/vm#127 lists:

- today: fixed-disk allocation, the host's DISM, compaction, first boot;
- this design: Setup's copy inside the VM, specialize, OOBE;
- totals, and whether any `vhdmp` reset or host-wide stall occurs in either.

The numbers go into `docs/drivers/hcs.md`'s Known numbers. The switch is
made for the host's safety. A slower install that cannot freeze its host
is the better one. The implementation PR states both totals, so the owner
can see the cost before merging it.

(Amended:) the owner chose on discobox-ai/vm#127 to skip the baseline,
which attaches a disk to the host, so only this design is timed. The old
install's own numbers in `docs/drivers/hcs.md` stand in for it.

## Alternatives rejected

- **Keep the host-side install and harden it further.** Examples: throttle
  DISM, put the work area on another volume, or require DRAM-ful storage.
  The guards it already has (a fixed disk, the Defender exclusion, a
  bounded attach) did not prevent the freeze. The failure is host-wide, and
  disco-vm cannot know which storage will survive an attach. Every other
  path already lets the guest write its own disk.
- **Keep both installs, with an option to choose the host-side one.** That
  is two paths to prove. The hazardous one keeps all its code and guards.
  Its only argument, speed, is unmeasured and expected to be small (issue
  estimate: 15 minutes against 15–20).
- **Preallocate the in-VM install's disk, or reserve its full size in the
  free-space check.** The fixed disk guarded a host attach, which is gone.
  Allocating it is the slowest step today. A dynamic disk is what every VM
  already runs on, and reserving 64 GiB for an install that writes about
  20 GiB refuses hosts that could install.
- **Bypass Setup's TPM and Secure Boot checks with `LabConfig`, and give no
  vTPM.** It keeps today's VMs as they are. But guests then have no TPM for
  anything that wants one: BitLocker, Windows Hello, measured boot, or
  software that checks for one. And install depends on a registry switch
  Microsoft could stop honouring. A vTPM makes the VM what Windows 11
  expects. (Amended: taken after all, as §4 says, because HCS gives no vTPM
  without the Hyper-V role; these costs stand.)
- **A vTPM with a fresh guest state file at every boot, as today.** The
  guest would see a new TPM each boot. Anything sealed to it, such as
  BitLocker or a Hello key, would be lost at the next boot.
- **Commit the guest state file with the disk, so a clone inherits the
  TPM.** Every clone of a committed VM would share one TPM identity and its
  keys. A TPM that is never committed avoids that, and turning off device
  encryption at install keeps the committed disk free of anything sealed to
  it.
- **Send a key at boot.** HCS has no call for it. The VM worker's video
  console, through which one could be typed, does not serve on Home hosts
  (ADR 26-11-02-274), and hcs must work there.
- **Remaster the whole retail ISO with a no-prompt boot image for every
  install.** It is a 5–7 GB copy per install and needs a UDF writer for an
  `install.wim` over 4 GiB. It stays as the fallback if neither shape in §3
  boots (open below), and then it would be cached by the ISO's digest.
- **Install the agent from `SetupComplete.cmd`.** It runs after OOBE, so
  `waitSetup` would see no agent for most of the install. Windows also skips
  it on an image with an OEM product key. Specialize's `RunSynchronous`
  runs earlier and always runs.
- **Boot a WinPE of disco-vm's own and run DISM inside the VM.** Building a
  WinPE needs the ADK, which disco-vm cannot ship or require. Setup is the
  path Microsoft supports, and it carries the boot files from the person's
  own ISO.

## Consequences

- Install no longer touches the host's disks. `Get-Disk` and
  `Get-DiskImage` show nothing of disco-vm's during or after an install.
  The fixed-disk allocation and the compaction go, and the free-space check
  asks for 32 GiB instead of the disk's size plus 30 GiB.
- (Amended) hcs VMs still have no TPM, and `guest.vmgs` is still scratch.
  Install depends on `LabConfig`'s TPM bypass, a switch Microsoft could stop
  honouring; the rejected alternative above names that cost.
- Install now depends on Setup inside the guest: the answer file and the
  boot medium. A change in a future Windows release to how Setup
  finds its answer file or checks hardware breaks install, where DISM did
  not care.
- disco-vm gains, in Go and tested on every OS in CI:
  - a UDF reader for the retail ISO;
  - a reader for install.wim's XML;
  - an ISO 9660 writer (shared with ADR 26-11-13-318's agent drive if that
    lands);
  - for shape (b), a VHD, GPT, and FAT writer.
  Whether any of these belongs in `github.com/discobox-ai/x` is the
  implementation's call.
- The proof this needs, on a real Windows host:
  - (amended) a Windows 11 Pro install from a retail ISO completes with the
    TPM and CPU checks bypassed and Secure Boot on, and `Get-Disk` and
    `Get-DiskImage` are checked during and after;
  - the OS volume of the committed VM is not encrypted;
  - its committed VM passes `machinetest` and `internal/e2e`;
  - `install --gui` shows the first logon;
  - an edition by name, by index, and a missing one;
  - this install timed per §8 (amended: the baseline is skipped).
  The baseline run attaches a disk to its host. It runs only on a host
  whose owner has agreed to that, never on the host that froze.
- `docs/drivers/hcs.md` (Hard rules, Install, Boot, Commit, Known numbers),
  the proof table's install row in `DESIGN.md`, `pkg/machine/hcs/doc.go`,
  and the `InstallSpec.Agent` comment change with the implementation.

## Deferred

- **The agent on a drive attached at every boot, as ADR 26-11-13-318 does
  for macOS.** Windows guests still keep the agent install gave them.
  Revisit as that ADR's Deferred says. The specialize script would then
  install a stub instead of the agent.
- **A vTPM**, where the host gives one (a host with the Hyper-V role), with
  the lifetime the proposed §4 gave it. Revisit when hcs can tell such a
  host from one without, at `Check`.
- **Left to implementation**, none of it decided here:
  - whether Commit refuses a VM whose OS volume BitLocker protects.
- **Settled by the implementation** (discobox-ai/vm#127, amended
  2026-11-15), of what was left to it:
  - §3 is shape (a), with the retail ISO's root `bootmgr.efi` on the medium,
    and the retail DVD `subst`ed to `Z:` in WinPE for `InstallFrom`.
  - §4 is the Deferred fallback: no vTPM, and `LabConfig` bypasses the TPM
    check, because HCS gives no vTPM without the Hyper-V role.
  - Setup needs no installation key for the editions tried (Pro and
    Education).
  - The scripts are the medium's marker: the answer file runs each from
    whichever drive holds it under `disco-vm\`.
  - `sweepAttached` goes with the attach, with nothing to sweep: the old
    attach was never permanent, so it ended with the process that held it.
  - The UDF and WIM readers and the ISO 9660 writer are `internal/udf`,
    `internal/wim`, and `internal/iso9660`, in this repository and not in
    `github.com/discobox-ai/x`: each reads or writes only what a retail
    Windows ISO and this medium need (one type 1 partition, 2048-byte
    blocks, no Rock Ridge), so none is yet the general library `x` holds.
    They move there when something outside disco-vm needs them.
  - vz's agent drive keeps its own writer (`pkg/machine/vz/iso.go`, ADR
    26-11-13-318) for now: a flat Rock Ridge image whose bytes are its cache
    key, proven on a Mac. Moving it onto `internal/iso9660` means giving that
    Rock Ridge and a fixed epoch and proving the drive again on a Mac, so it
    is deferred until the next change to either writer.
