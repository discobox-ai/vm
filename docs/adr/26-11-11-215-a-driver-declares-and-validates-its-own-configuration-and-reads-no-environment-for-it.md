# 26-11-11-215 — A driver declares and validates its own configuration, and reads no environment for it

- **Status**: Accepted (supersedes in part 26-10-31-990 §2, its agent
  `$DISCO_VM_AGENT` read by the driver; 26-11-03-530, its runtime
  `$DISCO_VM_KRUN_RUNTIME` read by the driver; and 26-11-04-412 §3, native
  Docker's environment-based constructor)
- **Date**: 2026-11-11

Accepted by the owner by merging vm#72, 2026-11-11.

## Context

`machine.Config` holds what the seam reads for every driver: its pool, base,
directory, shim command, and key. Everything one driver alone needs is an
environment variable the driver reads itself, wherever it needs it:

- docker: `DOCKER_HOST`, `DISCO_VM_DOCKER_ROOT`, and `DISCO_VM_AGENT`, at
  construction (26-11-04-412 §3 keeps that constructor);
- archil: `ARCHIL_REGION` and `ARCHIL_BASE_URL`; boxd: `BOXD_GRPC_ADDR` and
  `BOXD_TOKEN_URL`;
- libkrun: `DISCO_VM_KRUN_RUNTIME` at construction, and `DISCO_VM_AGENT` at
  every boot, in the shim; vz: `DISCO_VM_AGENT` at every boot, in the shim;
- fake: `DISCO_VM_AGENT` and `DISCO_VM_FAKE_NO_COMMIT`.

So a program that embeds the engine (discobox's vm provider) can set none of
them per engine, only for its whole process; a shim, the process that holds
a started VM, sees only what it inherits; nothing lists what a driver takes;
and a misspelled variable, or a value a driver does not take
(`DOCKER_HOST=ssh://`), is found at the first call that uses it, or never.

A region is the next such setting (vm#68): boxd serves each region at an API
endpoint of its own, and Archil's region picks its control plane. The route
first proposed for it (vm PR 69) adds `machine.Config.Region` to every
driver, `Capabilities.Region` to say which take one, and `--region`.

These settings differ in kind from driver to driver, and will keep
differing. discobox has the shape for its own sandbox providers: a
`sandbox.ProviderDefinition` per provider, whose `ConfigFields` describe a
JSON configuration the provider decodes into its own type, checked by its
registered validator, and a CLI that makes each field a flag and shows them
with `--help=PROVIDER`.

Several of these settings decide where the driver's key is sent (boxd's
`addr` and `tokenUrl`, archil's `baseUrl`) or what runs as root in a guest
(every agent path, libkrun's runtime). Today only whoever controls the
process's environment can set them.

## Decision

1. **A driver's configuration is a JSON object of its own,**
   `machine.Config.DriverConfig`. No field is shared between drivers because
   two happen to have one. `Config.Base` stays on `Config`: the seam's
   contract is stated in its terms (a fresh VM is of the base, and a driver
   with none, hcs or docker unconfigured, refuses one; `machinetest`'s
   a-fresh-vm), and an embedder sets it for any driver. A driver's
   definition describes its base as it does a field (`Definition.Base`, a
   `ConfigField`: what it takes, its default, `DISCO_VM_BASE`), so
   `--help=DRIVER` shows it and decision 7 marks it. It is not part of the
   driver's configuration: the command line reads it from `DISCO_VM_BASE`
   in both of decision 4's ways, and no command takes it as a flag (ADR
   26-11-03-606). A shim needs none, since only Create and Upgrade make a VM
   from it (`Prepare`), and a shim boots.
2. **A driver registers its definition and validator with its factory**,
   `machine.Register(name, machine.Registration{New, Definition, Validate,
   KeyEnv})`, all required. `machine.Definition` and `machine.ConfigField`
   have the shape of discobox's `ProviderDefinition` and
   `ProviderConfigField` (key, label, type, description, placeholder,
   required, advanced), and add four: `Default`, the value used when the
   field is absent (empty where the driver works it out, which the
   description then says); `Env`, the variable the command line reads it
   from; `Privileged` (decision 7); and `Retired` (decision 3).
3. **A driver decodes its own type, strictly.** `machine.DecodeConfig[T]`
   refuses a key T does not have. A driver's factory and validator decode
   through one function that applies its defaults and checks its values.
   `machine.New` validates before it constructs: a key the definition does
   not list, a required field missing, then the driver's validator. A key
   is never taken away: a field renamed or removed stays in the definition,
   `Retired` and out of `--help`, and is accepted for good, decoded into its
   replacement or, with none, ignored with a warning. A configuration an
   embedder stored keeps working in every later release. A retired key is
   `Privileged` if its replacement is, and one given beside its replacement
   is refused. It keeps its environment variable, and loses its flag.
4. **A driver reads no environment for its configuration.** The command line
   builds it, in one of two ways that do not mix:
   - `--driver-config JSON|@FILE`: the whole configuration, and nothing else.
     No environment variable is read, and a field's flag beside it is
     refused. A variable a field reads that is set, and is not what the
     configuration says, is named in a warning on stderr, never its value.
   - Otherwise each field from its flag, `--DRIVER-FIELD` (`--docker-host`),
     or else its environment variable. A flag of another driver's field is
     refused.

   `@FILE` is read by the command line, at most 1 MiB, and only its parsed
   JSON goes on; no error echoes a value or the file. The variables keep
   their names. `--help=DRIVER` lists a driver's fields with their keys,
   variables and defaults; `disco-vm drivers` describes each driver; `info`
   prints the configuration in effect. A process-wide convention every
   client honours (`HTTPS_PROXY`, boxd's `ALL_PROXY`) is not a driver's
   setting, and stays where it is read.
5. **A region is a field of each driver that has one**, `region` in boxd's
   configuration and in archil's, and nothing on `machine.Config` or
   `Capabilities`. Its meaning is the driver's: boxd's region picks the
   endpoint its home endpoint (`addr`) names for it, for the calls that are
   the region's; archil's picks the control plane, unless `baseUrl` names
   one, which wins. archil's variable stays `ARCHIL_REGION`; boxd's is
   `BOXD_REGION`. A region given to a driver without one is an unknown
   field, refused like any other.
6. **A shim is given its configuration in a file, never on its command
   line.** A local driver's `Start` writes the configuration to the VM's
   directory, readable by its owner alone, and a shim of this executable is
   started with `--driver-config @` that file. A shim an embedder starts
   (`Config.ShimCommand`) makes its driver as it makes Base and Dir, from
   the same configuration; `local.ShimMain` refuses a driver whose
   configuration is not the file's. What a boot reads (vz's and libkrun's
   agent) is so the starting command's, not the shim's environment's.
7. **A configuration is trusted as the key is.** A field that names a host
   resource (a socket, a file, a directory), or an endpoint the key or a
   guest's data is sent to, is `Privileged`: an embedder lets no one set it
   who could not be handed the key and the host. Today that is archil's
   `baseUrl`; boxd's `addr` and `tokenUrl`; docker's `host`, `root` and
   `agent`; libkrun's `runtime` and `agent`; vz's and fake's `agent`; and
   the base of vz and libkrun, which may be a host directory whose kernel
   and root are booted. A region is not: it names a provider's own
   endpoint, from its list; nor is a base that is only an image reference.
   A test holds the list, retired keys among it. The validators hold the rest:
   - A URL a key is sent to is `https`, or `http` to a loopback address for
     a test; boxd's gRPC connection is TLS whatever `addr` says. A URL with
     userinfo or a query is refused.
   - An agent or a runtime is an absolute path to a regular file (a
     directory, for a runtime), checked before it is read.
   - docker's `host` is `unix://` or `tcp://`, the latter without TLS as
     today: the docker driver sends no key, and a daemon on plain TCP is
     its operator's choice, which `Privileged` keeps theirs.
   - No field holds a secret: a credential is `Config.Keys`, and a
     definition has no type for one (discobox's `secret` type fails
     `machine.Register`).

## Alternatives rejected

- **A `map[string]string` of options with a schema of names.** It is what a
  flag gives, not what an embedder has: discobox's provider configuration is
  typed JSON, and a boolean or a number would be text every driver parses
  again.
- **A definition derived from the driver's `Config` struct by reflection,**
  from tags. It cannot drift from the decoder, but discobox's definitions are
  written by hand with labels, placeholders and advanced flags no tag spells
  well, and the point is the same shape. A test decodes every field each
  definition lists instead.
- **One variable per field named by a rule,** `DISCO_VM_<DRIVER>_<FIELD>`.
  `DOCKER_HOST` is what every Docker client reads, and `ARCHIL_REGION` and
  `BOXD_GRPC_ADDR` what those services' tools read. A field names its
  variable.
- **Layering flag over variable over `--driver-config`,** as vm#70 first
  put it. A configuration a shim is given whole would then still take any
  field it leaves out from the shim's own environment, and an inherited
  `BOXD_TOKEN_URL` would send the key elsewhere beneath a configuration
  that never named it. Whole or field by field, with a warning where they
  disagree, keeps what is in effect what is written.
- **Unprefixed flags for the selected driver only,** found by reading
  `--driver` before parsing, as discobox reads `--type`. docker's `root`
  would be `--root`, which is already the state directory, and a flag's
  meaning would change with `--driver`. The driver's name in front costs
  length and avoids both.
- **A region common to every driver** (`machine.Config.Region`,
  `Capabilities.Region`, `--region`; vm PR 69). It is a second way of
  carrying one driver's setting, for a setting two drivers share in name
  and give different meanings, and the capability repeats what a
  definition already says. Every next provider setting would ask for the
  same promotion.
- **The configuration on the shim's command line, or inherited in its
  environment.** A command line is readable by every local user, and an
  environment reaches only a shim this executable starts with the
  command line's own environment, not an embedder's.
- **Ignoring unknown keys, as discobox's `DecodeConfig` does.** A
  misspelling that does nothing is the failure this exists to stop; a
  stored configuration is kept working by decision 3 instead.

## Consequences

- An embedder sets each engine's driver configuration, validated, and can
  list a driver's fields as its own provider's (`machine.DefinitionOf`),
  minding `Privileged`. The mark is per field, so an embedder that lets a
  user pick vz's or libkrun's base by image reference must refuse a
  directory itself.
- A driver's new setting is a field, a check, and an `Env`; `REVIEW.md`
  refuses an `os.Getenv` in a driver for one.
- vm PR 69's region is carried as boxd's and archil's `region` field, its
  endpoint resolution unchanged, rebased onto this rather than adding
  `Config.Region`. vm PR 67's docker daemon is given its host by the
  configuration rather than by `$DOCKER_HOST`.
- `docker.New`, `boxd.New`, and `archil.New` return an error, since a
  configuration can be refused, and `machine.Register` takes a
  `Registration`.
- A test that boots in its own process (libkrun's and vz's conformance) sets
  the agent on the hypervisor; one that reaches a service builds its
  configuration from the environment, as the command line does.
