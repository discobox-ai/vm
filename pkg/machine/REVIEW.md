# pkg/machine review notes

- **A libkrun stream's host side never half-closes.** libkrun 1.19 resets a
  vsock stream the host half-closes, before the guest has read what came
  before, so every host-side conn of `pkg/machine/libkrun` overrides
  `CloseWrite`: a no-op on a caller's `Dial` (the guest never reads the
  caller's EOF), a full close on a guest's forward (the guest reads EOF once
  the far end is done). A new conn type there, or a splice that half-closes
  by another path, gets one of the two, never the default. A review found a
  forward that kept its half open and hung a guest that read to EOF.
- **Every guest port a caller reaches is known at boot** (ADR 26-11-03-247):
  `Machine.Dial` takes the agent's port and the service's
  (`guest.DialablePort`), and a forward's ports are in the record. Widening
  `DialablePort`, or dialing a booted machine past the seam's checks, passes
  every test on hcs, vz, and fake, whose hypervisors connect to any port at
  any time, and fails only on libkrun, which maps ports before its VM starts
  and never after.
- **A driver's setting is a field of its configuration, never an
  environment variable it reads** (ADR 26-11-11-215). A new one is a field of
  the driver's `Config`, a `ConfigField` in its `Definition` with the
  variable the command line reads as `Env`, and a check in its `decode`. An
  `os.Getenv` in a driver passes every test that sets the variable, and
  fails for an embedder, which sets none, and for a shim, which reads its
  configuration from the VM's file. A process-wide convention every client
  honours (boxd's `ALL_PROXY`, as gRPC honours `HTTPS_PROXY`) is not a
  driver's setting.
- **A new field is placed on the privileged list or argued off it**
  (`TestPrivileged`): one that names a host path, socket, or directory, or
  an endpoint the key or a guest's data reaches, is `Privileged`. A
  validator's error names the field and never its value, and a path is
  checked with `machine.CheckFile` before anything reads it.
- **A driver sends its key only where its definition says** (ADR
  26-11-12-461): a new call that carries the key, a new header or body field
  for it, or a new host, is a `SecretSend` in the definition and a case in
  the driver's secret test, which drives the real client and compares what
  arrived with `machine.SecretSends`. A send the definition lacks passes
  every other test and fails the first time discobox's proxy refuses the
  stand-in there. A new connection to a service dials through the
  embedder's dialer where one is set (`Config.Dialer()`,
  `Config.HTTPTransport()`, or `Config.Dial` handed to gRPC), and then reads
  no proxy variable; never a bare `net.Dialer` or `http.DefaultClient`
  regardless of it. A review found a driver that dialed through it for gRPC
  and through `HTTPS_PROXY` for its token exchange.
- **A test that a dial skips the proxy reads the transport, not a proxy
  variable**: assert `Proxy == nil` on what `Config.HTTPTransport()` returns.
  A dial recorded against an `httptest` server proves the embedder's dialer
  was used, not that the proxy was skipped, and setting `HTTP_PROXY` around
  it proves nothing: Go never proxies a loopback host and reads the variables
  once per process. A review found a test whose proxy variable could fail
  nothing.
- **A local driver's `Hypervisor` is made by its `newHypervisor`**, from
  `machine.Config` and the decoded configuration, in `New` and in every
  test. A field `New` sets from the config (the registry transport) and a
  test's literal leaves unset passes CI, which skips the hardware suites,
  and fails only in `task test:libkrun` or on a Mac: go-containerregistry
  panics on a nil transport. A review found both drivers' conformance
  tests building literals without it.
- **A secret is never configuration and never read back**: no field holds
  one, and nothing printed, logged, returned, or written carries its value.
- **A field is never removed or renamed**: it is `Retired`, with
  `ReplacedBy` where it has a replacement, and stays accepted for good.
- **A driver's configuration that scopes what it lists is part of the
  reaper contract.** The pool does, and so does a region (boxd's and
  archil's `region`): a driver lists only its region's VMs, so a reaper that
  lists with another region, or none, never sees them and they are billed
  forever. A new setting that narrows what `Instances` returns updates the
  reaping paragraph of `pkg/engine/DESIGN.md` in the same change. A review
  found the region added without it.
- **A cache keyed by digest replaces only an entry that is broken.**
  `linuximage.Pull` and `PullAgent` let a boot read an entry another pull
  has just returned, so a pull that finds a good entry at its key when it is
  done keeps that one and drops its own. Removing the key unconditionally
  before the rename deletes a guest image or an agent from under a boot of
  the same digest; a review found `PullAgent` copied from `Pull` without
  that check.
- **A real-hypervisor test skips before it requires anything.** Its first
  check is the variable naming its base or image; only once one is named may
  it fail for a missing `DISCO_VM_TEST_BINARY` or agent. CI's macos job runs
  `go test ./...` with cgo, so a darwin test that fails first breaks CI, and
  `task test:vz` always sets the binary, so the author's runs never see it. A
  review found a vz test that did.
- **A binary run in a guest to check it is bounded, and so is its
  answer.** vz's agent move runs the uploaded binary before it replaces the
  agent: in a process group of its own, killed whole after a few seconds, so
  a program that never exits is refused rather than left running as root.
  An exec still running at the host's deadline is a check that did not
  finish, never an agent that was killed. A review found both missing.
