# Install disco-vm on Windows.
#
#   irm https://github.com/discobox-ai/vm/releases/latest/download/install.ps1 | iex
#   $env:DISCO_VM_INSTALL_VERSION = 'v0.1.0'; irm https://github.com/discobox-ai/vm/releases/latest/download/install.ps1 | iex
#
# The copy a release uploads is stamped with that release and the SHA-256 of
# each binary it uploaded, and run with no arguments installs exactly that
# release. Asked for another version, it downloads the installer that release
# uploaded and hands over to it, so the code installing a release is always
# the code that release shipped. The copy in the source tree is not stamped,
# so it always hands over: to the newest release, or to the one named.
# install.sh is the same installer for macOS and Linux: change the two
# together.
#
#   -Version VERSION   the release to install, such as v0.1.0 or v0.2.0-rc.1
#                      (default: the release this copy came with, or the
#                      newest full release)
#   -InstallDir DIR    where to put disco-vm.exe
#                      (default: %LOCALAPPDATA%\Programs\disco-vm)
#   -NoModifyPath      leave the user's PATH alone
#
# Each can also be set in the environment as DISCO_VM_INSTALL_VERSION,
# DISCO_VM_INSTALL_DIR, or DISCO_VM_INSTALL_NO_MODIFY_PATH, which is the only
# way to pass one through `iex`. A parameter beats the environment.
#
# This runs under Windows PowerShell 5.1 as well as PowerShell 7, and stays
# ASCII because 5.1 reads a file with no byte order mark in the ANSI code
# page. Everything happens inside Install-DiscoVm, so `iex` leaves no
# preference behind in the caller's session, and nothing here calls exit,
# which would close that session.

param(
    [string]$Version,
    [string]$InstallDir,
    [switch]$NoModifyPath
)

function Install-DiscoVm {
    param(
        [string]$Version,
        [string]$InstallDir,
        [switch]$NoModifyPath
    )

    Set-StrictMode -Version 3.0
    $ErrorActionPreference = 'Stop'
    # Invoke-WebRequest in 5.1 downloads many times slower while it draws
    # progress.
    $ProgressPreference = 'SilentlyContinue'

    # Stamped by internal/release into the copy a release uploads. Empty here
    # in the source tree, where this script can only hand over to a release's
    # own installer.
    $release = ''
    $checksums = @{}

    # Where the releases are: a release's files at <releases>/download/<tag>/,
    # and the newest full release's at <releases>/latest/download/.
    # Overridable for a test or a mirror.
    $releases = 'https://github.com/discobox-ai/vm/releases'
    if ($env:DISCO_VM_INSTALL_RELEASES) { $releases = $env:DISCO_VM_INSTALL_RELEASES }

    if ($PSVersionTable.PSVersion.Major -lt 5) { throw 'this installer needs PowerShell 5.1 or later' }
    $onWindows = ($PSVersionTable.PSEdition -ne 'Core') -or $IsWindows
    if (-not $onWindows) {
        throw "on macOS and Linux, install with install.sh instead: curl -sSfL $releases/latest/download/install.sh | sh"
    }
    if ($PSVersionTable.PSEdition -ne 'Core') {
        # Windows PowerShell 5.1 can still default to TLS 1.0, which GitHub
        # refuses.
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    }

    # The SHA-256 of a file, without Get-FileHash: a Windows PowerShell whose
    # module path has been trimmed does not have that cmdlet. The .NET class
    # is always there, in 5.1 and in 7.
    function Get-DiscoVmSha256([string]$Path) {
        $stream = [IO.File]::OpenRead($Path)
        try {
            $sha = [Security.Cryptography.SHA256]::Create()
            try {
                return [BitConverter]::ToString($sha.ComputeHash($stream)).Replace('-', '').ToLowerInvariant()
            } finally {
                $sha.Dispose()
            }
        } finally {
            $stream.Dispose()
        }
    }

    # Puts the install directory on the user's PATH, for new terminals and
    # for this one.
    function Add-DiscoVmPath([string]$Dir) {
        if ((@($env:Path -split ';') -notcontains $Dir)) { $env:Path = "$env:Path;$Dir" }
        $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
        try {
            # Read unexpanded and written back as REG_EXPAND_SZ, so the
            # entries that use %VARIABLES% keep working.
            $current = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
            $entries = @($current -split ';' | Where-Object { $_ })
            if ($entries -contains $Dir) { return }
            $key.SetValue('Path', (($entries + $Dir) -join ';'), [Microsoft.Win32.RegistryValueKind]::ExpandString)
        } finally {
            $key.Close()
        }
        # Writing the registry tells nobody. Setting a user variable through
        # [Environment] broadcasts WM_SETTINGCHANGE, which makes Explorer, and
        # every terminal it starts from now on, read PATH again; clearing one
        # that does not exist changes nothing else.
        [Environment]::SetEnvironmentVariable('DISCO_VM_INSTALL_REFRESH', $null, 'User')
        Write-Host "added $Dir to your PATH; open a new terminal to use disco-vm there"
    }

    function Install-DiscoVmRelease {
        $arch = $null
        try { $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() } catch { }
        if (-not $arch) {
            $arch = $env:PROCESSOR_ARCHITECTURE
            if ($env:PROCESSOR_ARCHITEW6432) { $arch = $env:PROCESSOR_ARCHITEW6432 }
        }
        switch -regex ($arch) {
            '^(x64|amd64)$' { $arch = 'amd64' }
            '^arm64$' { $arch = 'arm64' }
        }
        $asset = "disco-vm-windows-$arch.exe"
        if (-not $checksums.ContainsKey($asset)) { throw "disco-vm $release has no build for Windows on $arch" }

        Write-Host "downloading disco-vm $release for windows/$arch"
        $downloaded = Join-Path $tmp $asset
        $url = "$releases/download/$release/$asset"
        try {
            Invoke-WebRequest -Uri $url -OutFile $downloaded -UseBasicParsing
        } catch {
            throw "could not download $asset of $release from ${releases}: $($_.Exception.Message)"
        }
        if ((Get-DiscoVmSha256 $downloaded) -ne $checksums[$asset]) {
            throw "$url is not the file $release was released with"
        }

        $dir = $InstallDir
        if (-not $dir) { $dir = Join-Path $env:LOCALAPPDATA 'Programs\disco-vm' }
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        $dir = (Resolve-Path -LiteralPath $dir).ProviderPath
        $dest = Join-Path $dir 'disco-vm.exe'
        # Copied in beside the destination and renamed over it, so a copy
        # that fails part way never leaves half a binary where disco-vm
        # should be.
        $staged = Join-Path $dir '.disco-vm.exe.new'
        Copy-Item -LiteralPath $downloaded -Destination $staged -Force
        if (Test-Path -LiteralPath $dest) {
            # Windows will not overwrite an executable that is running, but it
            # will rename one.
            $old = "$dest.old"
            if (Test-Path -LiteralPath $old) {
                try { Remove-Item -LiteralPath $old -Force } catch { $old = "$dest.$([guid]::NewGuid().ToString('N')).old" }
            }
            Move-Item -LiteralPath $dest -Destination $old -Force
        }
        Move-Item -LiteralPath $staged -Destination $dest -Force

        try {
            $out = (& $dest --version | Out-String).Trim()
        } catch {
            throw "installed $dest, but it does not run: $($_.Exception.Message)"
        }
        if ($LASTEXITCODE -ne 0) { throw "installed $dest, but it does not run: $out" }
        if (-not $out.EndsWith(" $release")) { throw "installed $dest, but it says it is '$out' rather than $release" }
        Write-Host "installed disco-vm $release to $dest"

        if (@($env:Path -split ';') -notcontains $dir) {
            if ($NoModifyPath) {
                Write-Host "warning: $dir is not on your PATH; add it to use disco-vm by name"
            } else {
                Add-DiscoVmPath $dir
            }
        }
        $first = Get-Command disco-vm -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($first -and $first.Source -ne $dest) {
            Write-Host "warning: $($first.Source) comes before it on your PATH, so that is the one 'disco-vm' runs"
        }
    }

    if (-not $Version -and $env:DISCO_VM_INSTALL_VERSION) { $Version = $env:DISCO_VM_INSTALL_VERSION }
    if (-not $InstallDir -and $env:DISCO_VM_INSTALL_DIR) { $InstallDir = $env:DISCO_VM_INSTALL_DIR }
    if ($env:DISCO_VM_INSTALL_NO_MODIFY_PATH) { $NoModifyPath = $true }
    if ($Version) {
        if ($Version -notmatch '^v') { $Version = "v$Version" }
        if ($Version -notmatch '^v[0-9][0-9A-Za-z.-]*$') { throw "$Version is not a release version, such as v0.1.0" }
    }

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('disco-vm-install-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        if ($release -and (-not $Version -or $Version -eq $release)) {
            Install-DiscoVmRelease
            return
        }
        # A release's installer is always asked for its own release, so a
        # second hand-over means that release uploaded the wrong script. Stop
        # rather than follow it anywhere.
        if ($env:DISCO_VM_INSTALL_DELEGATED) {
            throw "the installer $($env:DISCO_VM_INSTALL_DELEGATED) uploaded is stamped for '$release'"
        }

        if ($Version) {
            $url = "$releases/download/$Version/install.ps1"
            Write-Host "installing disco-vm $Version with the installer it was released with"
        } else {
            $url = "$releases/latest/download/install.ps1"
            Write-Host 'installing the newest disco-vm with the installer it was released with'
        }
        $script = Join-Path $tmp 'install.ps1'
        try {
            Invoke-WebRequest -Uri $url -OutFile $script -UseBasicParsing
        } catch {
            throw "could not download ${url}: is there such a release? (see $releases)"
        }
        $params = @{}
        if ($Version) { $params['Version'] = $Version }
        if ($InstallDir) { $params['InstallDir'] = $InstallDir }
        if ($NoModifyPath) { $params['NoModifyPath'] = $true }
        # A script block rather than the file, because an execution policy may
        # refuse to run a downloaded .ps1 and never refuses this, which is
        # also how `iex` ran the first one.
        $env:DISCO_VM_INSTALL_DELEGATED = if ($Version) { $Version } else { 'latest' }
        try {
            & ([scriptblock]::Create((Get-Content -Raw -LiteralPath $script))) @params
        } finally {
            Remove-Item Env:\DISCO_VM_INSTALL_DELEGATED -ErrorAction SilentlyContinue
        }
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Install-DiscoVm -Version $Version -InstallDir $InstallDir -NoModifyPath:$NoModifyPath
