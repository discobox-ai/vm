# Working on disco-vm

Read [docs/design.md](docs/design.md) first. If you are implementing a driver,
your brief is [docs/drivers/hcs.md](docs/drivers/hcs.md) or
[docs/drivers/vz.md](docs/drivers/vz.md).

## Rules

- **Platform code lives below the seam.** Hypervisor code belongs only in
  `pkg/machine/<driver>`. Guest-OS code belongs only in `pkg/guest/*_windows.go`
  or `*_unix.go`. Nothing in `pkg/engine`, `pkg/build`, `pkg/image`, or
  `internal/cli` may be build-tagged or check `runtime.GOOS` to pick a
  hypervisor behavior. If a driver seems to need an engine change, make it
  OS-neutral, express it through `machine.Capabilities`, and update
  `docs/design.md`.
- **The fake driver stays green on every OS.** `go test ./...` must pass on
  Windows, macOS, and Linux, and CI runs all three. A change to the seam or the
  protocol must keep fake, `machinetest`, and `internal/e2e` passing.
- **One binary.** The CLI, the guest agent, and the shim are one `disco-vm`.
  Don't add a second binary.
- **Report what was verified, and how.** A claim about guest behavior needs a
  run in a real guest. Say which layers of the proof table in `docs/design.md`
  a change moves, and update the table.
- Commit messages follow discobox's style, `type(scope): a sentence`, for
  example `feat(hcs): boot a prepared instance`.

## Test

```sh
go vet ./... && GOOS=darwin go vet ./... && GOOS=linux go vet ./...
go test ./...
```

A test that needs a real hypervisor is gated on an environment variable naming
an installed base layer, so CI skips it and a developer machine runs it.
