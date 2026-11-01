# pkg/machine/local review

- **A shim client made per call keeps no connection.** `openShim` runs on
  every Attach, stop, and kill, so its `http.Transport` disables keep-alives:
  one kept idle would hold a descriptor in the caller and a goroutine in the
  shim per call, without end in an embedder that runs for days. A review
  found the leak once.
- **A peer forward cannot refuse.** The shim's listener has accepted the
  guest's connection before it can know the peer is stopped, so the guest
  connects and reads EOF. Write it that way wherever it is described.
