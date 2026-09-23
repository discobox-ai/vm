# disco-vm hcs driver: lay Windows from a retail ISO onto a VHDX that the
# driver has created (fixed size) and attached to this host as $DiskNumber.
#
# Embedded in disco-vm and run by Install, the only code in the driver that
# touches a disk on the host. Its guards come from sandboxi's build-image.ps1,
# where each one was earned:
#   * Assert-OurDisk gates every destructive cmdlet: the disk must be backed by
#     OUR vhdx, RAW, and not disk 0 or a boot/system disk.
#   * The disk is fixed size (the driver made it so): a dynamic disk expanding
#     under DISM tripped vhdmp bus resets and hung the machine.
#   * The attach belongs to the driver and dies with its handle; this script
#     never mounts or dismounts the VHDX. It does mount the ISO, and dismounts
#     it on every exit path.
#   * bcdboot is the HOST's (the image's own exits 0xC0E90002 silently, leaving
#     an empty ESP), its exit code is checked, and the BCD is asserted to exist.
#   * A Defender exclusion covers the work area for the run and is removed.
#
# ASCII only: the driver writes this with a BOM, but Windows PowerShell 5.1
# still fails to parse a stray non-ASCII byte inside a double-quoted string.
param(
  [Parameter(Mandatory)][string]$Iso,
  [Parameter(Mandatory)][string]$Vhdx,
  [Parameter(Mandatory)][int]$DiskNumber,
  [Parameter(Mandatory)][string]$Agent,
  [string]$Edition = '',
  [string]$User = 'disco',
  [string]$Password = ''
)
$ErrorActionPreference = 'Stop'
$workDir = Split-Path -Parent $Vhdx
$script:mountedIso = $false
$script:excluded = $false

function Dismount-Iso {
  if ($script:mountedIso) {
    try { Dismount-DiskImage -ImagePath $Iso -ErrorAction Stop | Out-Null } catch {}
    $script:mountedIso = $false
  }
}

function Invoke-Cleanup {
  Dismount-Iso
  if ($script:excluded) {
    try { Remove-MpPreference -ExclusionPath $workDir -ErrorAction SilentlyContinue } catch {}
    $script:excluded = $false
  }
}
trap { Invoke-Cleanup; break }

# Prove the disk about to be partitioned is our fresh VHDX and nothing else.
# Call it as a statement: a function returns everything it writes.
function Assert-OurDisk([int]$n, [string]$path) {
  $d = Get-Disk -Number $n -ErrorAction Stop
  if ($n -eq 0) { throw "safety: refusing to touch disk 0" }
  if ($d.IsBoot -or $d.IsSystem) { throw "safety: disk $n is a boot or system disk; refusing" }
  if ($d.Location -ine $path) { throw "safety: disk $n is '$($d.Location)', not our vhdx '$path'; refusing" }
  if ($d.PartitionStyle -ne 'RAW') { throw "safety: disk $n is $($d.PartitionStyle), not RAW; refusing to repartition a disk that holds data" }
  Write-Output "safety: disk $n is our vhdx: RAW, not disk 0, not boot or system"
}

function Escape-Xml([string]$s) { [System.Security.SecurityElement]::Escape($s) }

try {
  # DISM's churn through vhdmp multiplied by real-time scanning is what pushed
  # the storage adapter over its timeout.
  try { Add-MpPreference -ExclusionPath $workDir -ErrorAction Stop; $script:excluded = $true } catch {}

  try { Dismount-DiskImage -ImagePath $Iso -ErrorAction SilentlyContinue | Out-Null } catch {}
  $img = Mount-DiskImage -ImagePath $Iso -PassThru
  $script:mountedIso = $true
  $isoDrive = ($img | Get-Volume | Where-Object DriveLetter | Select-Object -First 1).DriveLetter
  if (-not $isoDrive) { throw "the ISO mounted without a drive letter" }
  $wim = "${isoDrive}:\sources\install.wim"
  if (-not (Test-Path $wim)) { $wim = "${isoDrive}:\sources\install.esd" }
  if (-not (Test-Path $wim)) { throw "no sources\install.wim or install.esd on $Iso" }

  $images = @(Get-WindowsImage -ImagePath $wim)
  $index = $null
  if ($Edition -match '^\d+$') {
    $index = [int]$Edition
  } elseif ($Edition) {
    $match = $images | Where-Object { $_.ImageName -ieq $Edition } | Select-Object -First 1
    if (-not $match) { throw "no edition '$Edition' in $wim; it has: $(($images | ForEach-Object ImageName) -join ', ')" }
    $index = $match.ImageIndex
  } else {
    $match = $images | Where-Object { $_.ImageName -match ' Pro$' } | Select-Object -First 1
    if (-not $match) { $match = $images | Select-Object -First 1 }
    $index = $match.ImageIndex
  }
  $name = ($images | Where-Object ImageIndex -eq $index).ImageName
  Write-Output "image: $wim index $index ($name)"

  Assert-OurDisk $DiskNumber $Vhdx
  Initialize-Disk -Number $DiskNumber -PartitionStyle GPT | Out-Null
  # Initialize-Disk makes an MSR of its own; clear it so the disk ends up
  # exactly ESP, MSR, Windows.
  Get-Partition -DiskNumber $DiskNumber -ErrorAction SilentlyContinue | Remove-Partition -Confirm:$false -ErrorAction SilentlyContinue
  $esp = New-Partition -DiskNumber $DiskNumber -Size 300MB -GptType '{c12a7328-f81f-11d2-ba4b-00a0c93ec93b}' -AssignDriveLetter
  $s = $esp.DriveLetter
  Format-Volume -DriveLetter $s -FileSystem FAT32 -NewFileSystemLabel System -Confirm:$false | Out-Null
  New-Partition -DiskNumber $DiskNumber -Size 16MB -GptType '{e3c9e316-0b5c-4db8-817d-f92df00215ae}' | Out-Null
  $win = New-Partition -DiskNumber $DiskNumber -UseMaximumSize -GptType '{ebd0a0a2-b9e5-4433-87c0-68b6b72699c7}' -AssignDriveLetter
  $w = $win.DriveLetter
  Format-Volume -DriveLetter $w -FileSystem NTFS -NewFileSystemLabel Windows -Confirm:$false | Out-Null
  if (-not $s -or -not $w) { throw "the new partitions have no drive letters" }

  # The host's dism.exe rather than Expand-WindowsImage: same operation, but
  # its progress bar is on stdout, where the driver's log can see it.
  Write-Output "applying the image (the slow part)..."
  & "$env:SystemRoot\System32\dism.exe" /Apply-Image /ImageFile:"$wim" /Index:$index /ApplyDir:"${w}:\"
  if ($LASTEXITCODE -ne 0) { throw "dism /Apply-Image failed (rc=$LASTEXITCODE)" }
  if (-not (Test-Path "${w}:\Windows\System32\ntoskrnl.exe")) { throw "dism reported success but applied no Windows" }

  # Unattended Setup: a local administrator that logs on automatically, no
  # OOBE, UTC.
  $u = Escape-Xml $User
  $p = Escape-Xml $Password
  New-Item -ItemType Directory -Force "${w}:\Windows\Panther" | Out-Null
  $unattend = @"
<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend">
  <settings pass="specialize">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <ComputerName>*</ComputerName>
      <TimeZone>UTC</TimeZone>
    </component>
  </settings>
  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <OOBE><HideEULAPage>true</HideEULAPage><HideLocalAccountScreen>true</HideLocalAccountScreen><HideOnlineAccountScreens>true</HideOnlineAccountScreens><HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE><ProtectYourPC>3</ProtectYourPC></OOBE>
      <UserAccounts><LocalAccounts><LocalAccount wcm:action="add" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State"><Name>$u</Name><DisplayName>$u</DisplayName><Group>Administrators</Group><Password><Value>$p</Value><PlainText>true</PlainText></Password></LocalAccount></LocalAccounts></UserAccounts>
      <AutoLogon><Enabled>true</Enabled><Username>$u</Username><LogonCount>2147483647</LogonCount><Password><Value>$p</Value><PlainText>true</PlainText></Password></AutoLogon>
    </component>
    <component name="Microsoft-Windows-International-Core" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <InputLocale>en-US</InputLocale><SystemLocale>en-US</SystemLocale><UILanguage>en-US</UILanguage><UserLocale>en-US</UserLocale>
    </component>
  </settings>
</unattend>
"@
  Set-Content -Path "${w}:\Windows\Panther\unattend.xml" -Value $unattend -Encoding UTF8

  # The agent: disco-vm itself, as a boot-start SYSTEM service, so it is up
  # before anyone logs on (during Setup too). Registered through the offline
  # SYSTEM hive, which has ControlSet001 and no CurrentControlSet link.
  New-Item -ItemType Directory -Force "${w}:\disco-vm" | Out-Null
  Copy-Item $Agent "${w}:\disco-vm\disco-vm.exe" -Force
  reg load 'HKLM\DISCOVMSYS' "${w}:\Windows\System32\config\SYSTEM" | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "could not load the offline SYSTEM hive (rc=$LASTEXITCODE)" }
  $svc = 'HKLM\DISCOVMSYS\ControlSet001\Services\disco-vm'
  reg add $svc /v ImagePath /t REG_EXPAND_SZ /d '"C:\disco-vm\disco-vm.exe" guest' /f | Out-Null
  reg add $svc /v DisplayName /t REG_SZ /d 'disco-vm guest agent' /f | Out-Null
  reg add $svc /v Type /t REG_DWORD /d 16 /f | Out-Null
  reg add $svc /v Start /t REG_DWORD /d 2 /f | Out-Null
  reg add $svc /v ErrorControl /t REG_DWORD /d 1 /f | Out-Null
  reg add $svc /v ObjectName /t REG_SZ /d 'LocalSystem' /f | Out-Null
  # Restart the agent 5 s after any crash, three times a day.
  reg add $svc /v FailureActions /t REG_BINARY /d 8051010000000000000000000300000014000000010000008813000001000000881300000100000088130000 /f | Out-Null
  # Steps that run as the image's user log on with LogonUser, which refuses a
  # blank password unless this is off.
  reg add 'HKLM\DISCOVMSYS\ControlSet001\Control\Lsa' /v LimitBlankPasswordUse /t REG_DWORD /d 0 /f | Out-Null
  [gc]::Collect()
  reg unload 'HKLM\DISCOVMSYS' | Out-Null
  if ($LASTEXITCODE -ne 0) { Start-Sleep -Seconds 1; [gc]::Collect(); reg unload 'HKLM\DISCOVMSYS' | Out-Null }
  if ($LASTEXITCODE -ne 0) { throw "the offline SYSTEM hive stayed loaded; the image would boot with it held" }
  Write-Output "agent: C:\disco-vm\disco-vm.exe, service disco-vm (auto-start, LocalSystem)"

  & "$env:SystemRoot\System32\bcdboot.exe" "${w}:\Windows" /s "${s}:" /f UEFI
  if ($LASTEXITCODE -ne 0) { throw "bcdboot failed (rc=$LASTEXITCODE)" }
  if (-not (Test-Path "${s}:\EFI\Microsoft\Boot\BCD")) { throw "bcdboot returned 0 but wrote no BCD to the ESP" }

  Write-VolumeCache -DriveLetter $w -ErrorAction SilentlyContinue
  Write-VolumeCache -DriveLetter $s -ErrorAction SilentlyContinue
}
finally {
  Invoke-Cleanup
}
Write-Output "disk ready: $Vhdx"
