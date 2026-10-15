# State file reference

A state file declares what a guest is to have: packages, files, services,
and scripts that run once. `disco-vm converge` makes a running instance match
state files, from the client: they and the files they name are read on the
machine `disco-vm` runs on, and the guest is reached through exec and copy,
so nothing is installed in it. See
[ADR 0003](adr/0003-a-client-converges-a-guest-to-state-files.md).

```
disco-vm converge dev harness/claude-code/converge   # apply; exit 1 with the error
disco-vm converge dev base.yaml harness.yaml         # any number of files and directories
disco-vm converge dev                                # the last run's status, as JSON
```

A program that embeds the engine calls `Engine.Converge`. In a build,
`- converge: PATH` is a step ([build-spec.md](build-spec.md)).

## State files and runs

Each `NAME.yaml` is one state file. A run is given state files, or
directories, which are every `NAME.yaml` in them.

- **State files are applied in name order**, each one whole before the next,
  so `20-docker.yaml` can install from a repository whose tools
  `10-base.yaml` installed. Number them.
- **Runs add up.** A run applies the state files it is given and leaves what
  the guest has from others as it is. An image layer can be one state file,
  and a changed file can be converged on its own.
- **A later state file's declaration replaces an earlier one's** of the same
  resource (the same path, package, unit, or `exec` name), the way a later
  image layer's file is the file. That holds across runs: given alone, the
  earlier file does not take the resource back.
- The same resource twice in one state file is an error, and so is an unknown
  key. State files that do not load change nothing.

## A state file

```yaml
packages:
  apt: [jq, ripgrep, "nodejs=25.*"]
  npm: ["@anthropic-ai/claude-code"]
files:
  - path: /etc/claude-code/managed-settings.json
    source: ../managed-settings.json  # from this file's directory, on the client
    mode: "0644"
    owner: root:root
  - path: /etc/profile.d/harness.sh
    content: |
      export DISABLE_AUTOUPDATER=1
  - path: /usr/local/bin/discobox-hook-publish
    link: /usr/local/bin/discobox-sandbox-agent
  - path: /usr/local/libexec/discobox
    directory: true
services:
  enabled: [docker.socket]
  disabled: [docker.service]
  masked: [apt-daily.timer]
exec:
  - name: nix
    key: "3"
    shell: [/bin/bash, -o, pipefail, -c]
    run: |
      curl -fsSL https://install.determinate.systems/nix | sh -s -- install
```

**`packages`.** The key is the provider: `apt` or `npm`, on a Linux guest. A
name with no version is satisfied by whatever version is installed, so it
never asks the network on a guest that has it, and never upgrades one. Give a
version to get one: `NAME=VERSION` (apt, globs allowed) and `NAME@VERSION`
(npm) are installed as given. A provider's packages are one install; apt's is
`apt-get update`, then `install --no-install-recommends`, keeping a
configuration file that is already there. Neither leaves its download cache
behind: apt's archives are cleaned, and npm installs with a cache of its own
that is removed.

**`files`.** `path` is absolute in the guest, and each file is exactly one of
`source`, `content`, `link`, or `directory`. `mode` is octal and defaults to
0644, or 0755 for a directory. `owner` is `USER` or `USER:GROUP`, by name or
number; without one a path is root's. A managed path is replaced whatever was
there, except a directory with something in it. A file is copied into the
guest and moved to its path. Files are placed in the order they are
declared. A `link` holds its target as written, which need not exist yet; on
a Windows guest a link is to a file or to a directory as its target is when
the link is made, so a directory is declared before a link to it.

`source` is a path on the client, relative to the state file's directory,
and may be anywhere (`../../assets/brand`). One that is a directory is every
file under it, at the same place under `path`. Each is a resource of its own:
a file added to the directory is the only one copied, and one removed from it
is deleted. `mode` and `owner` apply to each file; with no `mode`, a file
whose source can be executed is 0755. A symbolic link in the directory is
made as a link. Two directories can share one `path`.

**`services`.** systemd units. `enabled` is `enable --now`, `disabled` is
`disable --now`, and `masked` is `mask`, then `stop` if the unit is running.
Disabled and masked units are applied before enabled ones, so a socket can
be enabled in the same file that disables its service. A changed file does
not restart the service that reads it; an `exec` does.

**`exec`.** A script for the guest's shell (`/bin/sh -c`, or PowerShell on
Windows), or for `shell`, an argv the script is appended to. It runs as root
or SYSTEM, with root's `HOME`. It runs once for each `key`, `shell`, and
`run` it has had, so it must be safe to run again, and changing `key` runs it
again. It does not run again because a file it reads changed.

## What a run does

The guest keeps a record of the digest each resource was applied at
(`/var/lib/disco-vm/converge/record.json`, or
`C:\ProgramData\disco-vm\converge` on Windows), and a run applies only a
resource whose digest is not the recorded one:

- Converging an instance to the state files its image was built from applies
  nothing: the image carries the record.
- A resource a user changed by hand stays changed until its declaration does.
- A file that leaves a state file is deleted when that state file is next
  given. A package or service that leaves is forgotten and left as it is. To
  drop a whole state file, give it empty.
- The first resource that fails ends the run with its error. What was applied
  before it is recorded, so the next run starts at the failure.

`status.json` beside the record is what `disco-vm converge INSTANCE` prints:
the digest of each state file the guest was converged to, and how the last
run ended.

## One run at a time

A run holds a lease on the guest, so two clients cannot converge it at once.
The lease lasts a minute (`--lease`) and the run renews it every half of
that.

- A run that finds the guest leased fails at once and names the holder. It
  does not wait; try again.
- A client that dies stops renewing, and the guest is free a minute later.
  What it had applied is in the record.
- A run that cannot reach its guest for a whole lease stops, since another
  client may have it by then.

## What each guest OS has

| | Linux | macOS | Windows |
|---|---|---|---|
| files | yes | yes | yes, without `mode` or `owner` |
| exec | yes | yes | yes |
| packages | apt, npm | no | no |
| services | systemd | no | no |

The guest needs only its own shell: sh on Linux and macOS, PowerShell on
Windows.
