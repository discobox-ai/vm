#!/bin/sh
# Install disco-vm on macOS or Linux.
#
#   curl -sSfL https://github.com/discobox-ai/vm/releases/latest/download/install.sh | sh
#   curl -sSfL https://github.com/discobox-ai/vm/releases/latest/download/install.sh | sh -s -- --version v0.1.0
#
# The copy a release uploads is stamped with that release and the SHA-256 of
# each binary it uploaded, and run with no arguments installs exactly that
# release. Asked for another version, it downloads the installer that release
# uploaded and hands over to it, so the code installing a release is always
# the code that release shipped. The copy in the source tree is not stamped,
# so it always hands over: to the newest release, or to the one named.
#
# install.ps1 is the same installer for Windows: change the two together.
#
# Everything happens inside main, called on the last line, so a download cut
# short part way through runs nothing at all.
set -eu

# Stamped by internal/release into the copy a release uploads. Empty here in
# the source tree, where this script can only hand over to a release's own
# installer.
release=
checksums=

# Where the releases are: a release's files at <releases>/download/<tag>/,
# and the newest full release's at <releases>/latest/download/. Overridable
# for a test or a mirror.
releases=${DISCO_VM_INSTALL_RELEASES:-https://github.com/discobox-ai/vm/releases}

# Which of the two digest tools this machine has, decided before anything is
# downloaded.
sha_tool=

usage() {
	cat <<'USAGE'
Install disco-vm.

Usage: install.sh [--version VERSION] [--dir DIR]

  --version VERSION  the release to install, such as v0.1.0 or v0.2.0-rc.1
                     (default: the release this copy of the script came with,
                     or the newest full release)
  --dir DIR          where to put the disco-vm command
                     (default: /usr/local/bin as root, ~/.local/bin otherwise)

Each option can also be set in the environment as DISCO_VM_INSTALL_VERSION or
DISCO_VM_INSTALL_DIR. A flag beats the environment.
USAGE
}

say() {
	printf '%s\n' "$*" >&2
}

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "this installer needs $1, and it is not on your PATH"
}

# need_sha256 picks the digest tool up front. Deciding it inside sha256 would
# put the die inside the command substitution that hashes the download, where
# it ends that subshell and nothing else, and the download would be reported
# as the wrong file.
need_sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha_tool=sha256sum
	elif command -v shasum >/dev/null 2>&1; then
		sha_tool=shasum
	else
		die "this installer needs sha256sum or shasum to check what it downloads"
	fi
}

sha256() {
	case $sha_tool in
		sha256sum) sha256sum "$1" | cut -d' ' -f1 ;;
		*) shasum -a 256 "$1" | cut -d' ' -f1 ;;
	esac
}

main() {
	need curl
	need uname
	need mktemp
	need_sha256

	version=${DISCO_VM_INSTALL_VERSION:-}
	dir=${DISCO_VM_INSTALL_DIR:-}
	while [ $# -gt 0 ]; do
		case $1 in
			--version | --dir)
				[ $# -ge 2 ] || die "$1 needs a value"
				option "$1" "$2"
				shift 2
				;;
			--version=* | --dir=*)
				option "${1%%=*}" "${1#*=}"
				shift
				;;
			-h | --help)
				usage
				exit 0
				;;
			*) die "unknown option $1 (try --help)" ;;
		esac
	done

	if [ -n "$version" ]; then
		case $version in
			v*) ;;
			*) version=v$version ;;
		esac
		case $version in
			v[0-9]*) ;;
			*) die "$version is not a release version, such as v0.1.0" ;;
		esac
		case $version in
			*[!0-9A-Za-z.-]*) die "$version is not a release version, such as v0.1.0" ;;
		esac
	fi

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM

	if [ -n "$release" ] && { [ -z "$version" ] || [ "$version" = "$release" ]; }; then
		install_release
		return
	fi
	# A release's installer is always asked for its own release, so a second
	# hand-over means that release uploaded the wrong script. Stop rather than
	# follow it anywhere.
	if [ -n "${DISCO_VM_INSTALL_DELEGATED:-}" ]; then
		die "the installer $DISCO_VM_INSTALL_DELEGATED uploaded is stamped for ${release:-no release}"
	fi
	delegate
}

option() {
	case $1 in
		--version) version=$2 ;;
		--dir) dir=$2 ;;
	esac
}

# delegate runs the installer of the release asked for, or of the newest full
# release, which installs its own.
delegate() {
	if [ -n "$version" ]; then
		url=$releases/download/$version/install.sh
		say "installing disco-vm $version with the installer it was released with"
	else
		url=$releases/latest/download/install.sh
		say "installing the newest disco-vm with the installer it was released with"
	fi
	curl -fsSL --retry 2 -o "$tmp/install.sh" "$url" ||
		die "could not download $url: is there such a release? (see $releases)"
	set --
	if [ -n "$version" ]; then
		set -- --version "$version"
	fi
	if [ -n "$dir" ]; then
		set -- "$@" --dir "$dir"
	fi
	DISCO_VM_INSTALL_DELEGATED=${version:-latest} sh "$tmp/install.sh" "$@" </dev/null
}

# platform sets os and arch to the release's names for this machine, refusing
# one no release runs on.
platform() {
	os=$(uname -s)
	arch=$(uname -m)
	case $os in
		Linux) os=linux ;;
		Darwin) os=darwin ;;
		MINGW* | MSYS* | CYGWIN*) die "on Windows, install from PowerShell instead: irm $releases/latest/download/install.ps1 | iex" ;;
		*) die "disco-vm is not released for $os" ;;
	esac
	case $arch in
		x86_64 | amd64) arch=amd64 ;;
		aarch64 | arm64) arch=arm64 ;;
		*) die "disco-vm is not released for $os on $arch" ;;
	esac
	if [ "$os" = darwin ] && [ "$arch" = amd64 ]; then
		# A shell running under Rosetta reports x86_64 on Apple silicon.
		if [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
			arch=arm64
		else
			die "disco-vm needs an Apple silicon Mac; it is not released for Intel Macs"
		fi
	fi
}

install_release() {
	platform
	asset=disco-vm-$os-$arch
	expected=$(printf '%s\n' "$checksums" | awk -v asset="$asset" '$2 == asset { print $1 }')
	[ -n "$expected" ] || die "disco-vm $release has no build for $os on $arch"

	say "downloading disco-vm $release for $os/$arch"
	curl -fsSL --retry 2 -o "$tmp/disco-vm" "$releases/download/$release/$asset" ||
		die "could not download $asset of $release from $releases"
	[ "$(sha256 "$tmp/disco-vm")" = "$expected" ] ||
		die "$releases/download/$release/$asset is not the file $release was released with"
	chmod 755 "$tmp/disco-vm"

	if [ -z "$dir" ]; then
		if [ "$(id -u)" = 0 ]; then dir=/usr/local/bin; else dir=${HOME:?}/.local/bin; fi
	fi
	mkdir -p "$dir" 2>/dev/null || die "cannot create $dir; choose another directory with --dir"
	# Copied in beside the destination and renamed over it, so the swap is
	# atomic and a disco-vm that is running keeps the file it started from.
	# A copy keeps the macOS signature, which is in the file.
	staged=$dir/.disco-vm.$$
	cp "$tmp/disco-vm" "$staged" 2>/dev/null || die "cannot write to $dir; choose another directory with --dir"
	mv -f "$staged" "$dir/disco-vm"

	out=$("$dir/disco-vm" --version </dev/null 2>&1) || die "installed $dir/disco-vm, but it does not run: $out"
	case $out in
		*" $release") ;;
		*) die "installed $dir/disco-vm, but it says it is '$out' rather than $release" ;;
	esac
	say "installed disco-vm $release to $dir/disco-vm"

	case ":${PATH:-}:" in
		*":$dir:"*)
			found=$(command -v disco-vm 2>/dev/null || true)
			if [ -n "$found" ] && [ "$found" != "$dir/disco-vm" ]; then
				say "warning: $found comes before it on your PATH, so that is the one 'disco-vm' runs"
			fi
			;;
		*)
			say "warning: $dir is not on your PATH; add it with:"
			say "  export PATH=\"$dir:\$PATH\""
			;;
	esac
}

main "$@"
