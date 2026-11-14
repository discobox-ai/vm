# 26-11-14-593 — A window signs in by itself only where the password is the account's name, and disco-vm keeps no other

- **Status**: Accepted (by the owner, 2026-11-14, choosing option B on
  discobox-ai/vm#138)
- **Date**: 2026-11-14
- **Supersedes in part**: 26-11-02-274 §1, that a Remote Desktop window
  always stores the image's user with its name for a password

## Context

An hcs Windows image's install makes its account's password the account's
own name (ADR 26-11-02-274 §6), and a `gui` window stores that as mstsc's
sign-in (§1, with the per-window address of 26-11-11-746 §3). The agent's
`exec --user` logs on with the name, then a blank password. `install
--option password=...` gives the account a password of the person's own
instead, which neither knows: the window's stored sign-in is wrong, and
`exec --user` fails with a bare logon error. This was t4 of vm PR 71's
review, left out of #74 and carried to #138.

The guest does hold the password: the install's automatic logon keeps it as
the LSA secret `DefaultPassword`. A build of `disco-vm` that read it with
`LsaRetrievePrivateData` was quarantined by Windows Defender as
`Trojan:Win32/Bearfoos.A!ml` on Windows 11 Pro 26200. disco-vm is one binary
for the command line, the agent, and the shim, so that code would break
disco-vm on any Windows host running Defender.

## Decision

1. **The agent says whether the name is still the password, and nothing
   more.** `disco-vm password-is-name USER`, a hidden command the host runs
   in the guest through the agent's exec, tries `LogonUser(USER, USER)` and
   prints `name` or `other`. No password is read, kept, or sent. An account
   that does not exist also prints `other`: Windows answers it with the same
   logon failure.
2. **A window stores the name as its sign-in only on `name`.** On `other`
   it stores none, still claims its own loopback address, and mstsc asks the
   person for the password. With no answer (an agent from before the
   command, or any failure) it stores the name as before, and a wrong one
   has mstsc ask all the same.
3. **`exec --user` as such an account stays unsupported**, with an error
   that says the password is neither the name nor blank and disco-vm keeps
   no other (`machine.ErrUnsupported` in the agent).
4. **`--option password` stays.** Install writes no password file to the
   guest.

## Alternatives rejected

- **Read the guest's `DefaultPassword` LSA secret.** Defender quarantines
  the binary that reads it, and with it the command line on the host.
- **Install also writes the password to a guest file only SYSTEM can read**
  (option A). It leaves a plaintext secret at rest in the guest, for one
  convenience, and only images installed after the change would have it.
- **Remove `--option password`** (option C). An image a person will sign in
  to by hand, or expose, should be able to have a password of its own.
- **Store the name and let mstsc fail first.** mstsc does ask again after a
  failed sign-in, but as an error ("your credentials did not work") over a
  sign-in the person never gave; the check makes the plain prompt the first
  thing shown, for the same one failed logon.

## Consequences

- A `gui` window on a `--option password` image asks for its password each
  time it opens, unless the person has mstsc remember it; that sign-in is
  then the person's own, and later windows skip its address.
- Running as such an account (`exec --user`) needs its password given by
  some later change; none is planned.
- The check is a logon attempt: a wrong one counts toward the guest's
  account lockout policy, which a fresh install leaves at its default.
