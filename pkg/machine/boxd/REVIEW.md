# pkg/machine/boxd review notes

- **An empty `org` on an API key or a machine's token is the org it is
  fenced to, never the personal scope.** boxd's credential fence resolves
  "" to the fenced org, so a call made with no org to reach "the caller's
  own" scope reaches the org's again. The fake resolves "" the same way.
  (A `Check` that listed personal secrets with no org read the org's
  `shared` ones as personal and refused the key.)
- **A boxd call documented for a login or an API key is not one a machine's
  token is known to make.** `inMachine` drivers use such a token; a call
  boxd does not document for it either degrades (no cap) or is listed as
  unverified, and the fake refuses it where a test says so. (A `Check` that
  failed on a refused `GetOrgMachineLimit` would have broken every
  in-machine driver.)
