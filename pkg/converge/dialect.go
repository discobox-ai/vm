package converge

import (
	"fmt"
	"strings"
	"time"
)

// dialect is the scripts a run sends a guest, in the shell that guest OS has
// without anything installed: sh on Linux and macOS, PowerShell on Windows.
// It is chosen by the guest's OS, which the guest reports, and never by this
// process's: the scripts are text here and run there.
//
// Every path a dialect is given is already as a process in the guest sees it.
type dialect interface {
	// stateDir is where a guest keeps the record, the status, and the lease.
	stateDir() string
	join(elem ...string) string
	scriptName() string
	// argv runs a script file.
	argv(script string) []string
	// execShell and execPrelude are what an exec resource's script runs
	// with when it names no shell.
	execShell() []string
	execPrelude() string
	// wrap is a whole script: body, in a script that fails on the first
	// error and removes stage, the directory it was copied into, as it ends.
	wrap(stage, body string) string

	// acquire takes the lease in dir for holder, and exits leaseHeldExit
	// when another holder's has not run out. renew extends it, and exits
	// the same way once it is another's. release gives it back.
	acquire(dir, holder string, ttl time.Duration) string
	renew(dir, holder string, ttl time.Duration) string
	release(dir, holder string) string
	// read prints a file, or nothing when there is none.
	read(path string) string
	// install moves a file copied in beside the script to path, in one
	// rename.
	install(staged, path string) string

	// files and services are the bodies that put files in place and hold
	// units in a state: lines, each of which prints a marker with its index
	// when it is done. With a ledger, owners and units are written to it,
	// not applied.
	files(lines, ledger string) string
	file(n int, id, mode, owner, staged, path string) string
	link(n int, id, owner, target, path string) string
	directory(n int, id, mode, owner, path string) string
	gone(path string) string
	services(lines, ledger string) string
	service(n int, id, unit, state string) string
	// packages installs the names a provider does not have yet.
	packages(provider string, names []string, ledger string) (string, error)
}

// dialectFor is a variable so a test can run the PowerShell scripts where
// there is no Windows.
var dialectFor = func(goos string) dialect {
	if goos == "windows" {
		return powershell{}
	}
	return posix{}
}

// posix is sh, for a Linux or macOS guest.
type posix struct{}

func (posix) stateDir() string            { return "/var/lib/disco-vm/converge" }
func (posix) join(elem ...string) string  { return strings.Join(elem, "/") }
func (posix) scriptName() string          { return "run.sh" }
func (posix) argv(script string) []string { return []string{"/bin/sh", script} }
func (posix) execShell() []string         { return []string{"/bin/sh", "-c"} }
func (posix) execPrelude() string         { return "" }

// sq quotes a string for sh.
func sq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (posix) wrap(stage, body string) string {
	return "set -eu\nstage=" + sq(stage) + "\ntrap 'rm -rf \"$stage\"' EXIT\n" + body
}

// posixLease reads the lease into $holder and $expires, empty and 0 when
// there is none or it is not whole.
const posixLease = `holder=; expires=0
{ read -r holder expires < "$lease"; } 2>/dev/null || true
case "$expires" in ''|*[!0-9]*) expires=0 ;; esac
`

func (posix) acquire(dir, holder string, ttl time.Duration) string {
	// The lease is made whole beside its path and linked to it: a link to a
	// name that exists fails, so of two clients one has it. One that has
	// run out is renamed away first, which only one client can do.
	return fmt.Sprintf(`dir=%s; lease="$dir/lease"; id=%s
now=$(date +%%s)
printf '%%s %%s\n' "$id" "$((now + %d))" > "$stage/lease"
if ! ln "$stage/lease" "$lease" 2>/dev/null; then
%s  if [ "$expires" -gt "$now" ]; then
    echo "$holder is converging this guest, and holds it for another $((expires - now))s" >&2
    exit %d
  fi
  if ! mv "$lease" "$stage/stale" 2>/dev/null || ! ln "$stage/lease" "$lease" 2>/dev/null; then
    echo "another client took this guest's lease first" >&2
    exit %d
  fi
fi
for other in "$dir"/run/*; do
  [ "$other" = "$stage" ] || rm -rf "$other"
done
`, sq(dir), sq(holder), seconds(ttl), posixLease, leaseHeldExit, leaseHeldExit)
}

func (posix) renew(dir, holder string, ttl time.Duration) string {
	return fmt.Sprintf(`lease=%s/lease; id=%s
%sif [ "$holder" != "$id" ]; then
  echo "this guest's lease went to ${holder:-no one}" >&2
  exit %d
fi
printf '%%s %%s\n' "$id" "$(($(date +%%s) + %d))" > "$stage/lease"
mv -f "$stage/lease" "$lease"
`, sq(dir), sq(holder), posixLease, leaseHeldExit, seconds(ttl))
}

func (posix) release(dir, holder string) string {
	return fmt.Sprintf("lease=%s/lease; id=%s\n%sif [ \"$holder\" = \"$id\" ]; then rm -f \"$lease\"; fi\n", sq(dir), sq(holder), posixLease)
}

func (posix) read(path string) string {
	return fmt.Sprintf("if [ -f %s ]; then cat %s; fi\n", sq(path), sq(path))
}

func (posix) install(staged, path string) string {
	return fmt.Sprintf("mv -f \"$stage\"/%s %s\n", sq(staged), sq(path))
}

// posixFiles is the functions a files script calls. A function that step
// runs is run as a condition, where sh ignores -e, so each is one chain.
const posixFiles = `step() {
  n=$1; id=$2; shift 2
  if "$@"; then echo "::done $n"; else echo "disco-vm: $id failed" >&2; exit 1; fi
}
# own gives a path to an owner. The ledger names it by its third argument,
# for a file that is still on its way to its path.
own() {
  if [ -z "$1" ]; then return 0; fi
  if [ -n "$ledger" ]; then echo "owner ${3:-$2} $1" >> "$ledger"; else chown -h "$1" "$2"; fi
}
# A directory in the way goes only if it is empty.
clear() {
  if [ -d "$1" ] && [ ! -L "$1" ]; then rmdir "$1"; fi
}
put() {
  mkdir -p "$(dirname "$4")" && clear "$4" && chmod "$1" "$stage/$3" && own "$2" "$stage/$3" "$4" && mv -f "$stage/$3" "$4"
}
lnk() {
  mkdir -p "$(dirname "$3")" && clear "$3" && ln -sfn "$2" "$3" && own "$1" "$3"
}
dir() {
  if [ -L "$3" ] || { [ -e "$3" ] && [ ! -d "$3" ]; }; then rm -f "$3" || return 1; fi
  mkdir -p "$3" && chmod "$1" "$3" && own "$2" "$3"
}
gone() {
  if [ -d "$1" ] && [ ! -L "$1" ]; then
    rmdir "$1" 2>/dev/null || echo "left in place, with what it holds: $1" >&2
  else
    rm -f "$1"
  fi
}
`

func (posix) files(lines, ledger string) string {
	return "ledger=" + sq(ledger) + "\n" + posixFiles + lines
}

func (posix) file(n int, id, mode, owner, staged, path string) string {
	return fmt.Sprintf("step %d %s put %s %s %s %s\n", n, sq(id), sq(mode), sq(owner), sq(staged), sq(path))
}

func (posix) link(n int, id, owner, target, path string) string {
	return fmt.Sprintf("step %d %s lnk %s %s %s\n", n, sq(id), sq(owner), sq(target), sq(path))
}

func (posix) directory(n int, id, mode, owner, path string) string {
	return fmt.Sprintf("step %d %s dir %s %s %s\n", n, sq(id), sq(mode), sq(owner), sq(path))
}

func (posix) gone(path string) string { return "gone " + sq(path) + "\n" }

// posixServices holds a systemd unit in a state. Not `mask --now`, which
// fails for what cannot be stopped: a template, which has only instances,
// and a unit this guest does not have, which is masked so it never does.
const posixServices = `step() {
  n=$1; id=$2; shift 2
  if "$@"; then echo "::done $n"; else echo "disco-vm: $id failed" >&2; exit 1; fi
}
unit() {
  if [ -n "$ledger" ]; then echo "service $2 $1" >> "$ledger"; return 0; fi
  case "$1" in
    enabled) systemctl unmask "$2" && systemctl enable --now "$2" ;;
    disabled) systemctl unmask "$2" && systemctl disable --now "$2" ;;
    masked)
      systemctl mask "$2" || return 1
      case "$2" in *@.*) return 0 ;; esac
      if systemctl is-active --quiet "$2"; then systemctl stop "$2"; fi ;;
    *) echo "no service state $1" >&2; return 1 ;;
  esac
}
if [ -z "$ledger" ] && ! command -v systemctl >/dev/null 2>&1; then
  echo "this guest has no systemctl" >&2
  exit 1
fi
`

func (posix) services(lines, ledger string) string {
	return "ledger=" + sq(ledger) + "\n" + posixServices + lines
}

func (posix) service(n int, id, unit, state string) string {
	return fmt.Sprintf("step %d %s unit %s %s\n", n, sq(id), sq(state), sq(unit))
}

// A name with no version is satisfied by whatever is installed, so a guest
// that has it never asks the network. Neither provider leaves its download
// cache behind: apt's archives are cleaned, and npm gets a cache of its own
// to throw away, for the question too, which writes its logs there.
const (
	posixApt = `if ! command -v apt-get >/dev/null 2>&1; then
  echo "this guest has no apt-get" >&2
  exit 1
fi
missing=
for name; do
  case "$name" in
    *=*) missing="$missing $name" ;;
    *) dpkg-query -W -f '${Status}' "$name" 2>/dev/null | grep -q 'install ok installed' || missing="$missing $name" ;;
  esac
done
if [ -z "$missing" ]; then exit 0; fi
export DEBIAN_FRONTEND=noninteractive
apt-get update
# A configuration file a state file already placed is kept over the package's.
# shellcheck disable=SC2086 # package names, one word each
apt-get install -y --no-install-recommends -o Dpkg::Options::=--force-confold $missing
apt-get clean
`
	posixNpm = `if ! command -v npm >/dev/null 2>&1; then
  echo "this guest has no npm" >&2
  exit 1
fi
cache=$(mktemp -d)
trap 'rm -rf "$stage" "$cache"' EXIT
export npm_config_cache="$cache" npm_config_update_notifier=false
missing=
for name; do
  case "$name" in
    ?*@*) missing="$missing $name" ;;
    *) npm ls -g --depth=0 "$name" >/dev/null 2>&1 || missing="$missing $name" ;;
  esac
done
if [ -z "$missing" ]; then exit 0; fi
# shellcheck disable=SC2086 # package names, one word each
npm install -g $missing
`
)

func (posix) packages(provider string, names []string, ledger string) (string, error) {
	quoted := make([]string, len(names))
	for i, name := range names {
		if strings.ContainsAny(name, " \t\n") {
			return "", fmt.Errorf("package name %q has a space in it", name)
		}
		quoted[i] = sq(name)
	}
	// -f: a version pin can hold a glob (nodejs=25.*), which is apt's.
	body := "set -f\nset -- " + strings.Join(quoted, " ") + "\n"
	if ledger != "" {
		return body + fmt.Sprintf("for name; do echo \"package %s $name\" >> %s; done\n", provider, sq(ledger)), nil
	}
	switch provider {
	case "apt":
		return body + posixApt, nil
	case "npm":
		return body + posixNpm, nil
	default:
		return "", fmt.Errorf("no package provider %q for this guest (apt, npm)", provider)
	}
}

// powershell is Windows PowerShell 5.1, which every Windows guest has.
type powershell struct{}

func (powershell) stateDir() string           { return `C:\ProgramData\disco-vm\converge` }
func (powershell) join(elem ...string) string { return strings.Join(elem, `\`) }
func (powershell) scriptName() string         { return "run.ps1" }

func (powershell) argv(script string) []string {
	return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script}
}

func (powershell) execShell() []string {
	return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command"}
}

func (powershell) execPrelude() string {
	return "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue'; "
}

// pq quotes a string for PowerShell.
func pq(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (powershell) wrap(stage, body string) string {
	return "$ErrorActionPreference = 'Stop'\n$ProgressPreference = 'SilentlyContinue'\n$stage = " + pq(stage) + `
function Finish($code) {
  Set-Location ([IO.Path]::GetTempPath())
  Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue
  exit $code
}
try {
` + body + `} catch {
  [Console]::Error.WriteLine("disco-vm: $_")
  Finish 1
}
Finish 0
`
}

// powershellLease reads the lease into $holder and $expires.
const powershellLease = `$holder = ''; $expires = [long]0
try {
  $parts = [IO.File]::ReadAllText($lease).Trim().Split(' ')
  $holder = $parts[0]; $expires = [long]$parts[1]
} catch { }
`

func (powershell) acquire(dir, holder string, ttl time.Duration) string {
	// As in sh: the lease is made whole and hard-linked to its path, which
	// fails when the path exists, and one that has run out is moved away
	// first, which only one client can do.
	return fmt.Sprintf(`$dir = %s; $lease = Join-Path $dir 'lease'; $id = %s
$now = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$mine = Join-Path $stage 'lease'
[IO.File]::WriteAllText($mine, "$id $($now + %d)")
function Take { New-Item -ItemType HardLink -Path $lease -Target $mine | Out-Null }
$taken = $true
try { Take } catch { $taken = $false }
if (-not $taken) {
%s  if ($expires -gt $now) {
    [Console]::Error.WriteLine("$holder is converging this guest, and holds it for another $($expires - $now)s")
    Finish %d
  }
  try {
    [IO.File]::Move($lease, (Join-Path $stage 'stale'))
    Take
  } catch {
    [Console]::Error.WriteLine("another client took this guest's lease first")
    Finish %d
  }
}
$self = Split-Path -Leaf $stage
Get-ChildItem -LiteralPath (Join-Path $dir 'run') -Directory | Where-Object { $_.Name -ne $self } |
  Remove-Item -Recurse -Force -ErrorAction SilentlyContinue
`, pq(dir), pq(holder), seconds(ttl), powershellLease, leaseHeldExit, leaseHeldExit)
}

func (powershell) renew(dir, holder string, ttl time.Duration) string {
	return fmt.Sprintf(`$lease = Join-Path %s 'lease'; $id = %s
%sif ($holder -ne $id) {
  [Console]::Error.WriteLine("this guest's lease went to $holder")
  Finish %d
}
$mine = Join-Path $stage 'lease'
[IO.File]::WriteAllText($mine, "$id $([DateTimeOffset]::UtcNow.ToUnixTimeSeconds() + %d)")
Move-Item -LiteralPath $mine -Destination $lease -Force
`, pq(dir), pq(holder), powershellLease, leaseHeldExit, seconds(ttl))
}

func (powershell) release(dir, holder string) string {
	return fmt.Sprintf("$lease = Join-Path %s 'lease'; $id = %s\n%sif ($holder -eq $id) { Remove-Item -LiteralPath $lease -Force }\n", pq(dir), pq(holder), powershellLease)
}

func (powershell) read(path string) string {
	return fmt.Sprintf(`if (Test-Path -LiteralPath %s -PathType Leaf) {
  $bytes = [IO.File]::ReadAllBytes(%s)
  $out = [Console]::OpenStandardOutput()
  $out.Write($bytes, 0, $bytes.Length)
  $out.Flush()
}
`, pq(path), pq(path))
}

func (powershell) install(staged, path string) string {
	return fmt.Sprintf("Move-Item -LiteralPath (Join-Path $stage %s) -Destination %s -Force\n", pq(staged), pq(path))
}

// powershellFiles is the functions a files script calls. A mode means
// nothing to a Windows guest, and an owner is not given to one yet.
const powershellFiles = `function Step($n, $id, [scriptblock]$do) {
  try { & $do } catch {
    [Console]::Error.WriteLine("disco-vm: $id failed: $_")
    Finish 1
  }
  [Console]::Out.WriteLine("::done $n")
}
function Own($owner, $path) {
  if (-not $owner) { return }
  if ($ledger) { Add-Content -LiteralPath $ledger -Value "owner $path $owner" }
  else { throw 'a file owner is not given on a Windows guest yet' }
}
function IsLink($path) {
  $item = Get-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
  return ($null -ne $item) -and (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0)
}
function Parent($path) {
  New-Item -ItemType Directory -Force -Path (Split-Path -Parent $path) | Out-Null
}
# A directory in the way goes only if it is empty.
function Clear-Way($path) {
  if ((Test-Path -LiteralPath $path -PathType Container) -and -not (IsLink $path)) { [IO.Directory]::Delete($path) }
}
function Place-File($owner, $staged, $path) {
  Parent $path; Clear-Way $path; Own $owner $path
  Move-Item -LiteralPath (Join-Path $stage $staged) -Destination $path -Force
}
function Place-Link($owner, $target, $path) {
  Parent $path; Clear-Way $path
  if (IsLink $path) { (Get-Item -LiteralPath $path -Force).Delete() }
  elseif (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Force }
  New-Item -ItemType SymbolicLink -Path $path -Target $target | Out-Null
  Own $owner $path
}
function Place-Directory($owner, $path) {
  if (IsLink $path) { (Get-Item -LiteralPath $path -Force).Delete() }
  elseif ((Test-Path -LiteralPath $path) -and -not (Test-Path -LiteralPath $path -PathType Container)) { Remove-Item -LiteralPath $path -Force }
  New-Item -ItemType Directory -Force -Path $path | Out-Null
  Own $owner $path
}
function Remove-Gone($path) {
  if (IsLink $path) { (Get-Item -LiteralPath $path -Force).Delete() }
  elseif (Test-Path -LiteralPath $path -PathType Container) {
    try { [IO.Directory]::Delete($path) } catch { [Console]::Error.WriteLine("left in place, with what it holds: $path") }
  } elseif (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Force }
}
`

func (powershell) files(lines, ledger string) string {
	return "$ledger = " + pq(ledger) + "\n" + powershellFiles + lines
}

func (powershell) file(n int, id, _, owner, staged, path string) string {
	return fmt.Sprintf("Step %d %s { Place-File %s %s %s }\n", n, pq(id), pq(owner), pq(staged), pq(path))
}

func (powershell) link(n int, id, owner, target, path string) string {
	return fmt.Sprintf("Step %d %s { Place-Link %s %s %s }\n", n, pq(id), pq(owner), pq(target), pq(path))
}

func (powershell) directory(n int, id, _, owner, path string) string {
	return fmt.Sprintf("Step %d %s { Place-Directory %s %s }\n", n, pq(id), pq(owner), pq(path))
}

func (powershell) gone(path string) string { return "Remove-Gone " + pq(path) + "\n" }

func (powershell) services(lines, ledger string) string {
	if ledger == "" {
		return "throw 'services are not held on a Windows guest yet'\n"
	}
	return "$ledger = " + pq(ledger) + "\n" + lines
}

func (powershell) service(n int, _, unit, state string) string {
	return fmt.Sprintf("Add-Content -LiteralPath $ledger -Value %s; [Console]::Out.WriteLine('::done %d')\n", pq("service "+unit+" "+state), n)
}

func (powershell) packages(provider string, names []string, ledger string) (string, error) {
	if ledger == "" {
		return "", fmt.Errorf("no package provider %q for a Windows guest yet", provider)
	}
	var body strings.Builder
	for _, name := range names {
		fmt.Fprintf(&body, "Add-Content -LiteralPath %s -Value %s\n", pq(ledger), pq("package "+provider+" "+name))
	}
	return body.String(), nil
}

func seconds(d time.Duration) int64 {
	return max(int64(d/time.Second), 1)
}
