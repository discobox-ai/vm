# Working on disco-vm

Read [DESIGN.md](DESIGN.md) first. If you are implementing a driver, your
brief is [docs/drivers/hcs.md](docs/drivers/hcs.md),
[docs/drivers/vz.md](docs/drivers/vz.md), or
[docs/drivers/boxd.md](docs/drivers/boxd.md).

## Rules

- **Platform code lives below the seam.** Hypervisor code belongs only in
  `pkg/machine/<driver>`. Guest-OS code belongs only in `pkg/guest/*_windows.go`
  or `*_unix.go`. Nothing in `pkg/engine`, `pkg/build`, `pkg/image`, or
  `internal/cli` may be build-tagged or check `runtime.GOOS` to pick a
  hypervisor behavior. If a driver seems to need an engine change, make it
  OS-neutral, express it through `machine.Capabilities`, and update
  `DESIGN.md`.
- **The fake driver stays green on every OS.** `go tool task test` must pass on
  Windows, macOS, and Linux, and CI runs all three. A change to the seam or the
  protocol must keep fake, `machinetest`, and `internal/e2e` passing.
- **One binary.** The CLI, the guest agent, and the shim are one `disco-vm`.
  Don't add a second binary.
- **Report what was verified, and how.** A claim about guest behavior needs a
  run in a real guest. Say which layers of the proof table in `DESIGN.md` a
  change moves, and update the table.

## Project Structure

- Root module `github.com/discobox-ai/vm`: disco-vm, which builds OS images and
  runs them as VMs on Host Compute Service (hcs), Virtualization.framework
  (vz), or boxd.
- The root holds only top-level containers (`cmd`, `pkg`, `internal`, `docs`,
  `test`, `scripts`), never packages.
- `cmd/disco-vm`: the `disco-vm` binary: CLI, guest agent, and shim.
- `pkg/engine`, `pkg/build`, `pkg/image`: the OS-neutral core above the seam.
- `pkg/machine`: the driver seam; `pkg/machine/<driver>` holds each
  hypervisor, and `machinetest` its conformance suite.
- `pkg/guest`: the guest agent.
- `internal/cli`: the command line. `internal/e2e`: the CLI lifecycle test.
- `scripts/codesign-exec`: the `go test -exec` wrapper vz tests need.
- `docs`: driver briefs (`docs/drivers`), the build spec, example specs
  (`docs/examples`), and ADRs (`docs/adr`).
- `DESIGN.md` / `REVIEW.md`: package-local design and review notes.

## Shared Libraries

Reuse `github.com/discobox-ai/x` packages when applicable instead of writing
local equivalents (`go get github.com/discobox-ai/x@main`). If a generic
helper would be useful beyond disco-vm, it belongs in `x`, not here.

## Git Workflow

Work directly on whatever branch is already checked out. Do not create new
branches or worktrees unless explicitly told to. Commit messages follow
Conventional Commits in discobox's style, `type(scope): a sentence`, for
example `feat(hcs): boot a prepared instance`.

## Commands

Use Taskfile targets through the Go tool-managed `task` binary:

```bash
go tool task --list
go tool task build      # compile into build/, signed on macOS
go tool task test       # run tests (-race)
go tool task check      # vet (every OS), golangci-lint, repocheck, shellcheck, actionlint
go tool task generate   # regenerate generated files
go tool task verify     # fmt, go.mod, generated and managed files are current
go tool task ci         # everything CI runs: verify, check, test
go tool task test:vz    # vz conformance on a Mac (DISCO_VM_VZ_BASE)
go tool task test:e2e:vz
```

At the end of a code-changing task, run `go tool task ci`. Add Taskfile
targets instead of documenting ad hoc commands here.

A test that needs a real hypervisor is gated on an environment variable naming
an installed base layer, so CI skips it and a developer machine runs it.

## Implementation Quality

Prefer proper structural changes over compatibility shims or narrow patches.

- Do not introduce optional interfaces for behavior the system requires. Add
  required methods to the core interface and update all implementations.
- Do not add wrapper types, adapter layers, or helper wrappers just to avoid
  touching callers. Change the call sites.
- Treat existing databases and persisted state as durable. Design schema
  changes with a safe upgrade path.
- When the correct fix crosses package boundaries, update the model,
  interfaces, implementations, tests, and call sites together.
- Keep abstractions justified by durable ownership or meaningful complexity
  reduction.

## Package Design Docs

`DESIGN.md` explains the design of a package and its subdirectories;
`REVIEW.md` lists its review rules and pitfalls. Read them from the root down
to the package you are changing; closer files override parents. Keep them
short and directive, describe current state only, and update them in the same
change as the code. See the `repostd` skill for the rules.

## Architecture Decision Records

`docs/adr` records decisions and the alternatives rejected. Write an ADR only
when a plausible alternative was rejected for a non-obvious reason, or
something was deferred with a condition for revisiting it. Land it as
`Proposed`; `Accepted` is the go-ahead. ADRs are immutable once shipped
against: supersede, never edit. See `docs/adr/README.md`.
