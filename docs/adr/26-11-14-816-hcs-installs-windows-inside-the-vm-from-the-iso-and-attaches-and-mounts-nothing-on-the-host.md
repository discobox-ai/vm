# 26-11-14-816 — hcs installs Windows inside the VM from the ISO, and attaches and mounts nothing on the host

- **Status**: Proposed
- **Date**: 2026-11-14

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
the Defender exclusion. The guards have not been enough:

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
  without TPM 2.0 (and checks RAM and CPU). The VM document (`newDocument`,
  `config_windows.go`) has no vTPM. Today's VMs run without one because DISM
  applies the image without asking.
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

### 2. The VM boots from the retail ISO, attached as a read-only DVD

Install's boot document, and no other boot's, adds the person's ISO as an
`Iso` attachment on the VM's SCSI controller, read-only. It is granted to
the VM (`HcsGrantVmAccess`) and revoked afterwards, as `firstBoot` does with
its paths today. disco-vm never copies, rewrites, or remasters it.

The VM's disk is a new dynamic `disk.vhdx` of `spec.DiskBytes` at LUN 0,
created with `CreateVirtualDisk` and never opened by the host again. It is
the install's target and the committed VM's disk, as now.

### 3. A medium disco-vm writes makes the first boot prompt-free and carries the answer file and the agent

disco-vm writes a second, small medium in the install's directory. It holds:

- `autounattend.xml` at its root, where Setup looks on removable media;
- the agent (`spec.Agent`), as `disco-vm.exe`;
- the specialize script (§5).

It is built in Go, in process, from bytes disco-vm has. No host tool runs,
and nothing is mounted. It is attached read-only to the install's boot and
removed with the install's working files. It is not part of the committed
VM.

This medium also makes the first boot prompt-free. It boots WinPE from the
retail ISO's own boot files, which disco-vm reads from the ISO's UDF tree
in Go. Then WinPE's Setup applies `install.wim` from the retail DVD. No key
is sent. Which shape boots is left to the implementation's spike (open
below). The candidates are:

- **(a) An ISO** whose El Torito image is the retail ISO's
  `efi\microsoft\boot\efisys_noprompt.bin`, and which carries the ISO's
  `efi\microsoft\boot\bcd`, `boot\boot.sdi`, and `sources\boot.wim`. This is
  oscdimg's own no-prompt recipe.
- **(b) A small VHD** with an ESP holding the ISO's `efi\boot\bootx64.efi`
  (Boot Manager, which never prompts; a USB stick boots the same way), the
  BCD, `boot.sdi`, and `boot.wim`. It has to enumerate after the target
  disk, or the answer file must pick the target by something other than its
  number.

Either way, the spike has to show that Setup finds `install.wim` on the
retail DVD, with `InstallFrom` pointing there if it does not find it on its
own. After Setup's first reboot, it has put Windows Boot Manager first in
the firmware's order. Even if the retail DVD comes first, its prompt times
out to the disk.

### 4. The answer file does what install.ps1 did, through Setup

disco-vm resolves the edition before the boot. It reads install.wim's (or
install.esd's) XML out of the ISO in Go and applies `install.ps1`'s rules:

- an index;
- a name, matched exactly and case-insensitively;
- with no edition given, the first whose name ends in ` Pro`, else the first.

An edition the ISO lacks is refused before anything boots, and the error
lists the ISO's editions, as now.

The answer file's passes:

- **windowsPE:**
  - `DiskConfiguration` wipes disk 0 and lays out today's partitions: a
    300 MB FAT32 ESP, a 16 MB MSR, and NTFS Windows for the rest.
  - `ImageInstall/OSImage/InstallFrom/MetaData` names `/IMAGE/INDEX`, and
    `InstallTo` names the Windows partition.
  - The EULA is accepted. No page of Setup shows.
  - `HKLM\SYSTEM\Setup\LabConfig` bypasses the TPM, Secure Boot, RAM, and
    CPU checks. This keeps parity with today, whose VMs never met those
    checks and run without a vTPM. A vTPM is deferred.
- **specialize:** today's `ComputerName` and `TimeZone`, and the specialize
  script (§5) through `Microsoft-Windows-Deployment/RunSynchronous`.
- **oobeSystem:** today's settings unchanged: the hidden OOBE pages, the
  local administrator, automatic logon, and en-US.

### 5. The agent is installed inside the guest, during specialize

The specialize script runs as SYSTEM in the new Windows. It finds the
medium by a marker file and copies `disco-vm.exe` to
`C:\disco-vm\disco-vm.exe`. Then it makes, online, what `install.ps1` made
in the offline hive, with the same values:

- the `disco-vm` service: `"C:\disco-vm\disco-vm.exe" guest`, auto-start,
  LocalSystem, and the same failure actions;
- `LimitBlankPasswordUse` set to 0;
- Remote Desktop turned on (`fDenyTSConnections` 0) unless the guest's own
  `EditionID` starts with `Core`, its firewall left closed as now.

It logs to a file under `C:\disco-vm`, so a failed install can be read
from a committed disk or from the agent.

The agent runs from the boot after specialize. `waitSetup` already
tolerates no agent at first, and its stages do not change: `ImageState`,
then the automatic logon, then OOBE's completion, then a minute to settle.
A guest whose agent never comes up ends at `setupTimeout`, as now.

The installed guest is the same as today's. The agent has the same path
and service name, and the account, password, unattend settings, and
Remote Desktop setting are the same. Committed VMs made by today's install
need nothing.

### 6. Install's end, its window, and its records do not change

Install still ends only after an orderly shutdown through the agent, and
the committed VM is `disk.vhdx` alone. `install --gui` and
`gui --install` still open at the first logon, from the `.install-vm`
record. A guest with no desktop before that has nothing to show, as now.

`needSpace` asks for what the dynamic disk and Setup can grow to, which is
less than today's fixed disk plus its copy. The number is the
implementation's, from the measurement.

### 7. Time is measured, and does not decide

Both installs are timed on the same hardware, step by step, as
discobox-ai/vm#127 lists:

- today: fixed-disk allocation, the host's DISM, compaction, first boot;
- this design: Setup's copy inside the VM, specialize, OOBE;
- totals, and whether any `vhdmp` reset or host-wide stall occurs in either.

The numbers go into `docs/drivers/hcs.md`'s Known numbers. The switch is
made for the host's safety. A slower install that cannot freeze its host
is the better one. The implementation PR states both totals, so the owner
can see the cost before merging it.

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
- **Add a vTPM to the VM instead of bypassing the check.** HCS's
  `SecuritySettings.EnableTpm` is unproven here on a Home host without the
  Hyper-V role. It would change every VM's document and guest state, not
  just install's. Today's VMs run without one. Deferred below.

## Consequences

- Install no longer touches the host's disks. `Get-Disk` and
  `Get-DiskImage` show nothing of disco-vm's during or after an install.
  The fixed-disk allocation and the compaction go, and with them the
  largest free-space demand.
- Install now depends on Setup inside the guest: the answer file, the boot
  medium, and `LabConfig`. A change in a future Windows release to how
  Setup finds its answer file or checks hardware breaks install, where
  DISM did not care.
- disco-vm gains, in Go and tested on every OS in CI:
  - a UDF reader for the retail ISO;
  - a reader for install.wim's XML;
  - an ISO 9660 writer (shared with ADR 26-11-13-318's agent drive if that
    lands);
  - for shape (b), a VHD, GPT, and FAT writer.
  Whether any of these belongs in `github.com/discobox-ai/x` is the
  implementation's call.
- The proof this needs, on a real Windows host:
  - a Windows 11 Pro install from a retail ISO completes on a Home host and
    on a Pro host, with `Get-Disk` and `Get-DiskImage` checked during and
    after;
  - its committed VM passes `machinetest` and `internal/e2e`;
  - `install --gui` shows the first logon;
  - an edition by name, by index, and a missing one;
  - both installs timed per §7.
  The baseline run attaches a disk to its host. It runs only on a host
  whose owner has agreed to that, never on the host that froze.
- `docs/drivers/hcs.md` (Hard rules, Install, Known numbers), the proof
  table's install row in `DESIGN.md`, `pkg/machine/hcs/doc.go`, and the
  `InstallSpec.Agent` comment change with the implementation.

## Deferred

- **A vTPM** (`SecuritySettings.EnableTpm`), which would let Setup's
  checks pass as they are and give guests BitLocker and Windows Hello.
  Revisit when a guest needs a TPM, or when Setup stops honouring
  `LabConfig`.
- **The agent on a drive attached at every boot, as ADR 26-11-13-318 does
  for macOS.** Windows guests still keep the agent install gave them.
  Revisit as that ADR's Deferred says. The specialize script would then
  install a stub instead of the agent.
- **Left to implementation**, none of it decided here:
  - which shape in §3 boots without a prompt, (a) or (b), or failing both,
    the cached remaster;
  - whether Setup needs a generic installation key for the chosen edition
    (Microsoft publishes them; they do not activate), and which option
    carries one;
  - how the specialize script finds the medium (a marker file, a volume
    label);
  - whether `sweepAttached` stays for one release, for an install a
    previous binary interrupted;
  - the medium's file names and volume label;
  - `needSpace`'s figure;
  - the UDF, WIM, and ISO 9660 libraries.
