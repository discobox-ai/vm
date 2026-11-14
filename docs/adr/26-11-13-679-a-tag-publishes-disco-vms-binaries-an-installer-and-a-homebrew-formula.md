# 26-11-13-679 — A tag publishes disco-vm's binaries, an installer, and a Homebrew formula

- **Status**: Accepted (§2's darwin and windows binaries built alone
  superseded by
  [26-11-14-352](26-11-14-352-a-disco-vm-for-a-host-that-is-not-linux-carries-its-own-builds-linux-agent-and-the-command-line-hands-it-to-every-linux-guest.md))
- **Date**: 2026-11-13

Accepted by the owner by merging #105, 2026-11-14.

## Context

disco-vm has no release of its binaries (#102). There are no tags and no
GitHub Releases. The workflows are `ci`, `linux-guest`, `krun-runtime`, and
`linux-agent`. To get the binary, you clone the repository and run
`go tool task build`, which on macOS also ad-hoc signs it with
`cmd/disco-vm/disco-vm.entitlements`.

- Only the `linux-agent` workflow runs on `v*` tags. It publishes
  `ghcr.io/discobox-ai/disco-vm-agent:<module version>`, and embedders pin that
  by digest (`pkg/machine/linuximage/agent.go`).
- `disco-vm --version` already prints a version set at link time
  (`internal/version`, `Taskfile.yml` `LDFLAGS`).
- The darwin binary links `Code-Hex/vz` with cgo, so it can only be built on
  macOS. The other three targets are static and cross-compile from Linux.

discobox installs in two ways:

- From a Homebrew tap, `discobox-ai/homebrew-tap`
  (`brew install discobox-ai/tap/discobox`).
- From an install script that each release uploads, stamped with that
  release's tag and binary digests (discobox ADR 0110 §1).

discobox's darwin binary is ad-hoc signed with the Virtualization
entitlement. Its ADRs 0062 and 0066 note Developer ID signing and
notarization as a later need. curl and Homebrew do not quarantine what they
download, so an ad-hoc signed binary runs. A browser download is
quarantined, and Gatekeeper refuses it.

## Decision

### 1. One `v*` tag is the binary's, the module's, and the agent's version

A `vX.Y.Z` tag is a SemVer version, with prereleases `-alpha.N` or `-rc.N`
(repostd §9). The same tag is three versions at once:

- the module version embedders require;
- the version `disco-vm --version` prints, set by ldflags into
  `internal/version`;
- the tag of `disco-vm-agent`, which the `linux-agent` workflow publishes from
  the same tag.

The guest base (`linux/v*`) and the libkrun runtime (`krun/v*`) keep their own
lines, pinned in code by digest.

### 2. A tag publishes binaries, checksums, and stamped installers to a GitHub Release

`.github/workflows/release.yml` runs on `v*` tags and only calls
`go tool task release:*` targets (repostd §9; no goreleaser):

- A macOS runner builds `disco-vm-darwin-arm64` with cgo and signs it (§4).
- A Linux runner builds the static `disco-vm-linux-amd64`,
  `disco-vm-linux-arm64`, and `disco-vm-windows-amd64.exe`.
- A publish job creates the release: a GitHub prerelease for a
  `-alpha`/`-rc` tag, and a full release otherwise. It uploads the binaries,
  `checksums.txt` (SHA-256, in `sha256sum` format), `install.sh`, and
  `install.ps1`.

The binaries are uploaded bare, not archived; the macOS signature lives in
the Mach-O, so a copy keeps it. darwin/amd64 is not published, because vz's
macOS guests need Apple silicon. Release logic (checksums, stamping, the
formula) is a Go program in `internal/cmd`.

### 3. Two install channels: the installer, and Homebrew in discobox's tap

- **The installer.**
  - Each release uploads `install.sh` and `install.ps1`, stamped with its tag
    and the SHA-256 of each binary it uploaded, as discobox ADR 0110 §1 does.
  - Run with no arguments, a stamped installer installs its own release and
    checks the digest. With `--version vX.Y.Z`, it runs that release's
    installer instead.
  - To get the newest release, run
    `curl -sSfL https://github.com/discobox-ai/vm/releases/latest/download/install.sh | sh`,
    or on Windows
    `irm https://github.com/discobox-ai/vm/releases/latest/download/install.ps1 | iex`.
    GitHub's `latest` never resolves to a prerelease.
  - It covers macOS, Linux, and Windows.
- **Homebrew** (the owner chose discobox's tap).
  - The formula is `disco-vm`, in the same tap as discobox:
    `brew install discobox-ai/tap/disco-vm`.
  - It serves darwin/arm64 and linux amd64 and arm64 from the release's
    binaries, by URL and SHA-256.
  - It has no post-install step, so nothing rewrites the signed binary.
  - The publish job writes it for every full release, using a
    `HOMEBREW_TAP_TOKEN` secret in this repository that can write to
    `discobox-ai/homebrew-tap`.
  - A full release without the token fails, so the tap can't silently fall
    behind. A prerelease never reaches the tap.

There is one channel: the newest full release. A prerelease is installed only
by naming its version.

### 4. macOS is ad-hoc signed with the Virtualization entitlement

The release build signs `disco-vm-darwin-arm64` the same way
`go tool task build` does:
`codesign --force --sign - --entitlements cmd/disco-vm/disco-vm.entitlements`.
It then checks that the signature carries
`com.apple.security.virtualization` before anything is uploaded.

Neither channel quarantines what it downloads, so the binary runs a vz VM
without re-signing. Developer ID signing and notarization are deferred
(below).

## Alternatives rejected

- **discobox's blessed-stable, latest, and edge channels** (discobox ADRs
  0105 and 0110 §3), with two formulae and a promote step. That process exists
  to hold a release back from users until a human blesses it. disco-vm has
  no users to hold back from yet, and a prerelease tag already keeps a build
  off both channels.
- **A tap of its own** (`discobox-ai/homebrew-disco-vm`, which would make the
  command `brew install discobox-ai/disco-vm`). The owner chose discobox's
  tap, so one tap and one token serve both projects.
- **winget or scoop now.** discobox's own winget submission is still waiting
  on a moderator, and `install.ps1` already serves Windows.
- **Archives** (`.tar.gz`, `.zip`) around each binary. Each would hold one
  file, and both the installer and the formula can fetch a bare binary by URL
  and digest.
- **Developer ID and notarization now.** They need an Apple Developer ID
  certificate and notary credentials as CI secrets, and neither channel needs
  them. discobox ships ad-hoc too.

## Consequences

- The owner adds `HOMEBREW_TAP_TOKEN` to this repository before the first full
  release.
- `DESIGN.md` gains a section on releases. The proof table gains rows for a
  published release and for each channel's install.

## Deferred

- **Renaming the project to `discovm`**: the owner first asked for it with this
  release, then decided against it for now (2026-11-14). Revisit if the owner
  asks again. A rename after the first release costs more than one before it:
  - the formula and the release assets change name;
  - the ghcr images need new repositories and re-pinned digests;
  - users' state directories, Docker images, and the names inside their
    guests either need migrating or must be dropped.
- **Developer ID signing and notarization**: revisit when a channel delivers a
  quarantined binary (a browser download promoted as a way to install, a
  `.pkg`, or a cask), or when macOS refuses an ad-hoc signed binary installed
  by curl or Homebrew.
- **A short installer address** (a path or host on `discobox.ai`, as discobox
  ADR 0110 §4 serves its own): revisit when the release URL is the thing
  people complain about. It is a change to `discobox-ai/site`, not here.
- **winget and scoop**: revisit once discobox's winget package is accepted,
  or when Windows users ask.
- **Open to implementation**:
  - where each installer puts the binary: `/usr/local/bin` or
    `~/.local/bin`; on Windows, `%LOCALAPPDATA%\Programs\disco-vm` on the
    user's `PATH`;
  - the formula's test block.
