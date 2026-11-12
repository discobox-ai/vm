# pkg/machine/served review notes

- **The server hands its driver only what it found.** A `machine.Instance`
  from a client carries an ID a local driver reads as a directory; the
  wire carries names, and the server finds each in its driver's pool
  (`server.find`) before any call. A route that decodes an instance, or an
  ID, from a request reaches another pool.
- **A new route or body field is refused until it is listed.** Every route
  goes through `op` or `vm` with an `Op`; every create field is a `Field`
  checked in `createRequest.set`, and the body is decoded with unknown
  fields refused. A field added to `machine.InstanceSpec` is not served
  until it is added to both, and to `capabilities` where a capability
  reports it.
- **The version is checked before routing.** `versioned` wraps the whole
  mux, so a client of another version learns nothing else, an unknown path
  included.
- **Every check the engine makes is made again on the server's side.** The
  engine runs on the client, which is the caller's, so a check that lives
  only in `pkg/engine` is skipped by a client that calls the protocol
  itself. The served driver refuses it (as `local` refuses a malformed
  forward), or the server does (a forward to a host socket). A forward to a
  host socket went to the shim unchecked, which dials it with the server's
  privileges.
- **Machine sentinel codes are shared with the guest relay.** Add them in
  `machine.ErrorCode` / `ErrorSentinel`, so JSON refusals and guest
  operations preserve the same `errors.Is` identity. Served-only refusals
  stay in this package. Exec refuses before upgrading; errors after headers
  or frames start cannot acquire a response header.
- **A seam type the protocol decodes whole is a field a pool sends.**
  `createRequest.Forwards` is `[]machine.Forward`, so a field added to
  `machine.Forward` reaches the driver unless `create` checks it.
  `Forward.Pool` reached the owner's driver, which takes another pool,
  until `create` refused it; the same holds for any type the requests
  embed.
