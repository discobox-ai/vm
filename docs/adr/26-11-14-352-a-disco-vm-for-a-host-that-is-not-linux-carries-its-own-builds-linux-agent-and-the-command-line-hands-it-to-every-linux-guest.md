# 26-11-14-352 — A disco-vm for a host that is not Linux carries its own build's Linux agent, and the command line hands it to every Linux guest

- **Status**: Proposed
- **Date**: 2026-11-14
- **Supersedes in part**: 26-11-13-679 §2, its darwin and windows binaries
  built alone

## Context

A released disco-vm installed on a Mac, by `install.sh` or by
`brew install discobox-ai/tap/disco-vm` (ADR 26-11-13-679 §3), runs macOS
guests but not a Linux guest on vz, which is vz's default base (#125). It
also can't drive a Docker guest. Both need a linux disco-vm of the same
version as their `agent`:

- vz's `agent` is only what the configuration names
  (`pkg/machine/vz/linux.go`, `Config.Agent`). Every Linux boot appends it to
  the initrd (`pkg/machine/vz/linux_darwin.go`, `bootLinux`), and `checkAgent`
  refuses an empty one, telling the user to run `go tool task build`.
- docker falls back to `os.Executable()` only when `runtime.GOOS == "linux"`
  (`pkg/machine/docker/docker.go`, `newDriver`). On any other host it refuses
  a Create with no `agent`.
- libkrun runs only on Linux and treats an empty `agent` as "this binary"
  (`pkg/machine/libkrun/config.go`).
- vz's `macosAgent` is marked `DefaultExecutable`. The command line sets it
  to its own executable (`internal/cli/driverconfig.go`, `withExecutable`;
  ADR 26-11-13-318 §3), so a macOS guest needs no setup.

Neither install channel puts a linux disco-vm on the machine. The release
uploads `disco-vm-linux-arm64` beside `disco-vm-darwin-arm64`, and
`DESIGN.md` ("Releases") tells a Mac user to fetch it, check it, and set
`DISCO_VM_AGENT` by hand. A checkout works only because `go tool task build`
cross-compiles `build/linux-<arch>/disco-vm`, and the test tasks point
`DISCO_VM_AGENT` at it.

An embedder (discobox) ships no disco-vm binary. It pins the published agent
index, `ghcr.io/discobox-ai/disco-vm-agent:<module version>`, by digest and
pulls each platform's agent with `agent.Pull` (`pkg/machine/agent`). That
path is not this ADR's. The index is published by `agent.yml` on every push
to main and every `v*` tag, separately from `release.yml`.

A static linux disco-vm is about 16 MB stripped.

No `v*` tag has been cut yet, so no published release has shipped the gap.

## Decision

### 1. A release binary for a host that is not Linux carries the linux agent of its own build

`go tool task release:build` builds the static linux disco-vm first. It then
embeds that binary in the binary for each host that is not Linux:

- `disco-vm-darwin-arm64` carries linux/arm64.
- `disco-vm-windows-amd64.exe` carries linux/amd64, for a Docker guest from
  Windows.

The embedded agent is built from the same commit, with the same `LDFLAGS` and
version as the binary that carries it. So it is that binary's agent, and its
`guest.Protocol` always matches. The linux release binaries carry nothing,
because each is its own agent.

Each channel still installs one file, and that file's SHA-256 in
`checksums.txt` and in the stamped installers covers the agent too. On macOS,
the ad-hoc signature (26-11-13-679 §4) also covers it. The channels, the
formula, and the installers do not change. Nothing is fetched at runtime, so
an install runs offline.

The embedding lives in `cmd/disco-vm`, behind a build tag that the release
and `go tool task build` set. A plain `go build`, `go install`, or `go test`
compiles without the tag and carries no agent. Such a build behaves as today,
and needs `agent` configured. No package an embedder links carries the agent,
so an embedder's binary does not grow.

`go tool task build` on a Mac builds the same way as the release, so a
checkout behaves like an install. It still leaves `build/linux-<arch>/disco-vm`
for the tasks that name it.

### 2. The command line defaults every Linux agent field to a linux disco-vm for this host's architecture

A config field that names an agent declares the guest OS that agent is for.
This replaces the boolean `ConfigField.DefaultExecutable`. When the field is
absent, the command line fills it with one shared resolver, which replaces
both `withExecutable` and docker's `runtime.GOOS` fallback:

1. If this executable is a disco-vm for that OS on this host's architecture
   (`agent.Check(os.Executable(), os, runtime.GOARCH)`), it is used. That
   covers a Linux host's linux fields and a Mac's `macosAgent`, as today.
2. Otherwise, if this executable carries an agent for that OS and
   architecture (§1), the resolver writes the agent out once. It goes into an
   owner-only cache under the command line's directory, named by the agent's
   SHA-256 (the layout `agent.Pull` uses). The resolver checks the file with
   `agent.Check` and uses it.
3. Otherwise the field stays absent, and the driver refuses as it does today.
   The error names the field and `$DISCO_VM_AGENT`.

The resolver chooses by what a file is, not by `runtime.GOOS`, so
`internal/cli` stays OS-neutral. The fields that declare a Linux agent are
vz's, docker's, and libkrun's `agent`. vz's `macosAgent` declares a macOS one.
fake's `agent` runs on the host's own OS and keeps its own default.

Like the macOS agent, the path is resolved where the configuration is read,
so the shim receives a path and resolves nothing itself (26-11-11-215).

### 3. A driver given no agent refuses; it falls back to nothing

docker drops its `os.Executable()` fallback, and libkrun drops "empty is this
binary". With no `agent`, each refuses as vz does, naming the field. Every
default now comes from one place, the command line's resolver. An embedder
already names every agent it hands a driver (discobox pulls them by digest),
so it loses nothing.

### 4. The architecture is the host's

The resolver picks the agent for `runtime.GOARCH`:

- vz boots guests of the host's architecture only.
- Docker Desktop on Apple silicon and Docker on Windows run daemons of the
  host's architecture.

A docker daemon of another architecture still needs `agent` configured, and
docker's `checkArch` refuses a mismatch at Create, as today.

## Alternatives rejected

- **The channels install the release's `disco-vm-linux-<arch>` beside the
  binary, and the drivers find it next to `os.Executable()`.** This is the
  issue's first suggestion, and it also runs offline and is checked against
  `checksums.txt`. Several things count against it:
  - Every channel changes. The installers download and check a second file,
    and the formula gains an `on_macos` resource.
  - The second file needs a place off the user's `PATH` that both channels
    reach from the executable's resolved path (`../libexec/disco-vm/`). That
    place differs between the Homebrew Cellar, `/usr/local`, `~/.local`, and
    a checkout's `build/`.
  - Two files can be separated or replaced one at a time. The agent's version
    then drifts from the binary's, and the drift shows up only as a protocol
    refusal at boot.
  - Embedding costs about 16 MB in the one binary that already ships.
    Bundling costs the same bytes in a second file, plus everything above.
- **The driver pulls the published agent of its own version from ghcr by a
  digest stamped into the darwin build.** This is the issue's second
  suggestion, and the embedder's path. It has three problems:
  - The release would have to wait for `agent.yml` to publish the index and
    pull back its digest before the Mac builds. That couples two workflows
    that today run independently on the same tag.
  - The first Linux boot would need the network and ghcr.
  - A second root of trust (the registry) would join `checksums.txt` for one
    install.
- **Pull by the version tag, with no stamped digest.** This loosens the
  digest pinning that `agent.Pull` enforces, which the embedder relies on.
  The point of that pinning is that a tag that moves is never trusted.
- **Keep the resolver in each driver** (vz and docker each looking for a
  carried agent). The bytes live in `cmd/disco-vm` and must not reach a
  package an embedder links. Two copies of the lookup is also what the triage
  warned against.
- **Embed without a build tag**, by committing the agent or generating it
  into the source tree. That puts a 16 MB binary in git, or makes every
  `go build` depend on a cross-compile first.

## Consequences

- A Mac install from either channel boots the default Linux base on vz, and
  drives Docker Desktop, with `DISCO_VM_AGENT` unset. A Windows install
  drives Docker with no extra setup.
- `disco-vm-darwin-arm64` and `disco-vm-windows-amd64.exe` grow by the size of
  the linux agent. A macOS guest's agent drive (26-11-13-318 §2), which holds
  the darwin binary, grows by the same amount. It is still built once per
  digest.
- The published `Definition` changes shape: `defaultExecutable` gives way to
  a field that names the guest OS. Anything that reads definitions, such as
  discobox's console, follows.
- `DESIGN.md` "One binary, three roles" and "Releases" describe the carried
  agent, and drop the hand-fetch instruction. The proof table gains a row for
  a Mac install that runs a Linux guest with `DISCO_VM_AGENT` unset. Only a
  Mac with a released install can prove it.
- Accepting this ADR marks 26-11-13-679 as superseded in part, in its
  status line and in the index. 26-11-13-318 §3 still holds: the command
  line sets `macosAgent` to itself, now through the shared resolver.

## Deferred

- **Open to implementation**:
  - The build tag's name, and where `cmd/disco-vm` takes the embedded file
    from (a gitignored path the release task fills).
  - Whether the agent is embedded compressed.
  - The cache directory's exact path, and whether `pkg/machine/agent` gains
    a helper to write and check an agent from bytes that `Pull` then shares.
  - The new `ConfigField` member's name and type.
- **A linux host carrying the other architecture's agent** (a linux/amd64
  disco-vm driving an arm64 Docker daemon): revisit if someone asks. Until
  then, `agent` is configured by hand, as today.
