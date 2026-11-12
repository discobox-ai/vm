# pkg/machine/served

A driver served over a stream, and the driver that is its client (discobox
ADR 26-11-11-963 §3). An embedder that holds a driver serves it to a caller
it cannot hand the driver to; the caller opens an engine on the client as on
any driver. discobox's server serves a held driver (vz, libkrun) this way to
one pool agent, on that pool's tagged channel.

## What the embedder decides

`Handler(driver, Options)` serves one driver, made for one pool. The
embedder authenticates the channel; the handler authenticates no one.

- **Scope.** A client names VMs, never IDs or paths. The server finds every
  name, a create's `From` and an upgrade's `from` included, in its driver's
  pool, and hands the driver only what it found.
- **Allow-list.** `Options.Allow` lists operations (`Op`), each a group of
  driver calls in the engine's terms; `Options.Fields` lists what a create
  may set past its name (`Field`). Anything else is refused with
  `ErrRefused`, a body field the protocol does not know included.
  `GET /v1/driver` reports the driver's capabilities narrowed to the list,
  so an engine on the client refuses up front most of what the server
  would. A capability that covers two listed things is reported only where
  the first is allowed, and the server refuses the second: `Ports` is
  dial and forwards, reported where `OpDial` is, and a forward is refused
  unless `FieldForwards` is listed too.
- **What the engine checks, the server's side checks again.** The engine
  runs on the client, which is the caller's. A forward to a host socket is
  refused by the server whatever the list says: only the embedder's hook
  sets one. A malformed peer forward is refused by the driver, as the
  engine refuses it (`local`'s `checkForwards`).
- **A hook on every create** (`Options.Create`), after the fields are
  checked and `From` found, before the driver: the embedder forces a fence,
  a size, or a forward of its own there, or refuses.
- **Version.** Every request carries the client's (`Disco-Vm-Version`);
  another than `Options.Version` is refused with a `*VersionError` naming
  both, before the request is routed.

Never served, whatever the list says: `Install`, `Boot` in the serving
process, a window, host directories, caches, and keys. The client's `Boot`
is its `Start`, as boxd's is: the machine is the server's, held as its
driver holds it, and outlives the client.

## The protocol

HTTP/1.1, a new stream per call from the client's dialer, nothing kept
between calls, as the agent's protocol is (`pkg/guest`). Errors carry a
code that names a sentinel (`machine.ErrNotFound`, `ErrRefused`, ...), so
`errors.Is` answers the same on both sides; a sentinel the seam gains gets a
code and a status with it. `machine.ErrStale`'s is `stale`: a VM whose shim
is of another protocol refuses `dial` with it, and its guest operations
with its message alone, as every guest operation's error crosses. A guest operation's refusal
travels in the agent's protocol, which carries no code, so the client
refuses one the list does not allow before it asks, with `ErrRefused`, and
the server refuses it whatever the client does. A VM's guest operations upgrade
their connection to the agent's protocol, answered by `guest.Relay` over the
attached machine, with each request checked against the list (exec, copy,
info, shutdown); `dial` upgrades to the guest port's stream, the agent's or
the service's alone. That is the shim's control API's shape
(`pkg/machine/local`), and the splice and buffered connection are shared
from `internal/netutil`.

A client's machine watches for its stop only once `Done` or `Err` is called,
by asking the server whether the VM runs; a server that stops answering for
30 s gives the machine up with an error, as a remote driver does.
