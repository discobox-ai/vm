# Build spec reference

A build spec is a YAML file (default `disco-vm.yaml`) that `disco-vm build`
turns into an image. Unknown keys are errors, not silently ignored typos.

As with a Dockerfile, the spec says how to build and not what to call the
result: `disco-vm build -t NAME[:TAG]` names it (repeatable; the tag defaults
to `latest`), and a build without `-t` is known by its image ID, which `run`,
`tag`, and the rest accept by prefix. A spec with `name:` or `tag:` is refused
with that advice.

```yaml
from:                          # exactly one of image or install
  image: discobox/windows:11   #   an image already in the store
  install:                     #   or an OS installed from media
    os: windows                #     windows | darwin | linux
    media: Win11.iso           #     ISO, IPSW, or "latest" (context-relative or absolute)
    edition: Windows 11 Pro    #     image inside multi-image media
    disk: 128GiB
    options: {key: value}      #     args substituted, then passed to the driver

args:                          # ${NAME} substitution; --build-arg overrides
  NODE_VERSION: "22.20.0"
env:                           # set for every run step
  CI: "1"
shell: [pwsh, -Command]        # default: see below
user: admin                    # default account for run steps
resources:                     # the build VM's size; not part of the cache key
  cpus: 4
  memory: 8GiB

layers:
  - name: toolchains           # unique; shown in the build log
    when: {os: windows}        # optional; skip the whole layer elsewhere
    steps:
      - name: node             # optional label
        when: {os: [windows]}  # optional
        run: |                 # a script for the shell
          ...
        shell: [cmd, /c]       # per-step override
        env: {KEY: value}
        workdir: C:\src
        user: admin            # run as this account instead of root or SYSTEM
        timeout: 30m           # default 1h
      - copy: {src: tools, dst: C:\tools}  # context → guest; a dir's contents land in dst
      - reboot: true           # orderly restart, for installers that need one
```

## Semantics

**Layers.** Each layer boots the previous one, runs its steps, shuts the guest
down in order, and commits the disk. Put the steps that change least in the
earliest layers. A boundary costs a full shutdown, so group steps, rather than
cutting a layer per step as a Dockerfile does.

**Cache.** A layer's ID is a hash of:

- its parent's ID
- the driver and guest OS
- each step's resolved argv, environment, workdir, and user
- each copy step's destination and a content digest of its source

Step names, timeouts, and resources are not part of it. An unchanged prefix of
layers is reused. `--no-cache` builds every layer under fresh IDs and
overwrites nothing. An install layer is keyed on the media's path, size, and
mtime, not its hash, so a 6 GB ISO is not reread on every build.

**Args.** Only `${NAME}` with braces, and only for declared args, is
substituted. `$env:PATH` in PowerShell and `$HOME` or `${HOME}` in sh reach the
guest untouched. Args are also exported to run steps as environment variables.
A `--build-arg` that the spec does not declare is an error.

**`when`.** The base decides the guest OS: `from.install.os`, or the OS
recorded on the `from.image` layer. Steps and layers whose `when` does not match
are dropped before the cache key is computed. A layer left with no steps is
skipped.

**Default shell.**

| guest | shell |
|---|---|
| Windows | `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command`, with `$ErrorActionPreference = 'Stop'` prepended |
| macOS | `/bin/zsh -l -c` (a login shell, so `path_helper` and `brew shellenv` apply) |
| Linux | `/bin/sh -c` |

A step fails on a nonzero exit. In Windows PowerShell 5.1, a native command's
failure does not stop the script. Check `$LASTEXITCODE`, or use `Start-Process
-Wait -PassThru` as the discobox base example does.

**Copy.** `src` must be inside the build context, which defaults to the spec's
directory. A directory's contents land in `dst`, and a file lands in `dst` by
its own name. `dst` is always a directory.

**Environment.** Every run step is a new process. On Windows the agent builds
its environment from the registry each time, so what an earlier step installed
is already on PATH.

**Clean commits only.** If the guest must be forced off at the end of a layer,
the build fails. A disk from a hard stop is never cached.
