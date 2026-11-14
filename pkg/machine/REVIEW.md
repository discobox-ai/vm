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
- **A window in a process that attached the machine reaches the display
  through the shim's `/display` alone** (ADR 26-11-11-746). Only a process
  holding the machine (the shim, an install), or reaching an install's VM
  by its runtime ID (`gui --install`, which has no shim), dials
  `guest.DisplayPort` itself, and nothing adds it to `DialablePort`, which would hand a Windows
  desktop and its input to every `disco-vm dial` and peer forward.
- **A check-then-write against the credential manager is not a claim.**
  CredRead then CredWrite lets two hcs windows take one loopback address,
  and one guest then gets the other's sign-in. A window claims its address
  by binding a fixed port there first (`listenRDP`); only then is its
  sign-in stored.
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
  and through `HTTPS_PROXY` for its token exchange. Its TLS trusts
  `Config.RootCAs`: through `Config.HTTPTransport()` or
  `Config.TLSConfig()`, never a `tls.Config` built in the driver, which
  would trust only the system's roots and fail behind an embedder's proxy
  that terminates TLS.
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
  `linuximage.Pull` and `agent.Pull` let a boot read an entry another pull
  has just returned, so a pull that finds a good entry at its key when it is
  done keeps that one and drops its own. Removing the key unconditionally
  before the rename deletes a guest image or an agent from under a boot of
  the same digest; a review found the agent's pull copied from `Pull` without
  that check.
- **A real-hypervisor test skips before it requires anything.** Its first
  check is the variable naming its base or image; only once one is named may
  it fail for a missing `DISCO_VM_TEST_BINARY` or agent. CI's macos job runs
  `go test ./...` with cgo, so a darwin test that fails first breaks CI, and
  `task test:vz` always sets the binary, so the author's runs never see it. A
  review found a vz test that did.
- **A URL's reach that depends on the rest of the spec is refused in the
  driver's Create, and documented as conditional.** `Capabilities.URLs` is
  one value a scope, which `URLRequests.Check` reads alone; where a driver can
  meet a strict request only with some networks or another URL's mode (boxd's
  private URL is its public one on no networks), its Create refuses the rest,
  and the capability table and the driver's DESIGN.md say when. Why: the
  engine passes such a request, so a reach stated as unconditional would
  promise what the driver then refuses.
- **A test that checks a file's owner-only mode skips the check on
  Windows**, which reports no owner-only mode: 0666 for a writable file,
  0444 for a read-only one (`agent` TestPull,
  `local` TestShimSocket, `vz` TestAgentDrive). A test in a file with no
  build tag runs in the `windows` CI job, and a review found one that would
  fail there on every run.
- **A string vz writes into a launchd plist escapes `&`, `<` and `>` and
  nothing else** (`plistEscape`). launchd's reader ends a string at a
  character reference, so `encoding/xml`'s `&#xA;` for a newline ran the
  macOS stub as its first line alone and no install's agent ever started.
  `plutil`, `plistlib` and `encoding/xml` all read the references, so only
  a real guest showed it; `TestDaemonPlist` now refuses any `&#`.
- **An in-machine endpoint is declared whether the driver uses it or not**
  (`Definition.InMachine`). The endpoint is the provider's (boxd's metadata
  adapter answers any process on a non-isolated boxd machine), so whether
  the driver's configuration turns it on never decides whether it is listed:
  an embedder blocks every endpoint listed, whatever the configuration. Its
  field defaults to off, as every way a driver acts as its host does. A
  review found a first draft that listed the adapter only when the driver
  used it, and so told an embedder holding a key there was nothing to block.
