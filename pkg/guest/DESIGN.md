# pkg/guest

The guest agent (`disco-vm guest`), its client, and the shim's relay. Where
the agent listens is in [pkg/machine](../machine/DESIGN.md#ports-not-guids).

## HTTP between host and agent

Where a driver has an agent, the agent protocol is HTTP/1.1
(`pkg/guest/proto.go`), as every discobox guest service already is.
`guest.Client` is that driver's `machine.Guest`, and the shim serves the same
protocol from its machine (`guest.Relay`) to other processes:

| endpoint | purpose |
|---|---|
| `GET /v1/health`, `GET /v1/info` | readiness, OS/arch/hostname |
| `POST /v1/exec` | upgrades to a framed stream: stdin, resize, stdout, stderr, exit |
| `GET`/`PUT /v1/files` | tar out of and into the guest |
| `POST /v1/shutdown` | orderly power-off or reboot |

The guest, not the hypervisor, powers itself off: the agent where there is
one, systemd on boxd. A vz stop request reaches macOS as a power-button press,
and macOS answers it by sleeping. HCS has no graceful path for a stock Windows
guest at all. So Kill is only the fallback.

The agent is a boot-start service (SYSTEM on Windows, root launchd daemon on
macOS). It is up before any user logs on, and a command can drop to a user
with `exec -u`. On Windows it builds each process's environment from the registry
(`CreateEnvironmentBlock`), so a toolchain one `exec` installs is on
PATH for the next.
