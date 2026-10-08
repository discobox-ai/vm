---
name: repostd
description: The discobox-ai repository standard for code layout, docs, agent config, tooling, lint, tests, CI, and release. Use when creating a discobox-ai repo, bringing one into conformance, reviewing a change that touches repo structure (new top-level directory, workflow, Taskfile target, lint config, ADR, DESIGN.md), or when `go tool repocheck` reports findings.
---

<!-- Managed by repostd (go tool repocheck sync). Do not edit here; change
     github.com/discobox-ai/repostd and sync. -->

# discobox-ai repository standard

Every `discobox-ai` repo follows this standard. Rules tagged with a rule ID
are enforced by `go tool repocheck` (run by `task check` and CI). Untagged
rules need judgment: apply them when writing and reviewing.

The standard has two **stacks**. A repo with a `go.mod` is a **Go** repo; a
repo with a `package.json` and no `go.mod` is a **node** repo. Everything
but the toolchain is the same for both. Rules marked *(Go)* or *(node)*
apply to that stack only, and repocheck skips the others.

## Workflow

This skill is self-contained. It may be read from its URL,
`https://raw.githubusercontent.com/discobox-ai/repostd/main/.agents/skills/repostd/SKILL.md`,
without being installed, by any agent. It works on the git repository that
contains the current directory, whatever state that repository is in, even
empty. `$RC` finds the repo root itself. If the current directory is not
inside a git repository, `$RC` says so; stop and tell the user.

1. **Pick the runner, `$RC`.** Needs nothing but git and a Go toolchain.
   Use the first of these that applies:
   - `go.mod` has a `tool` directive for
     `github.com/discobox-ai/repostd/cmd/repocheck`: `$RC` is
     `go tool repocheck`.
   - `go` is on `PATH`: `$RC` is
     `env GOTOOLCHAIN=auto GOPROXY=direct go run github.com/discobox-ai/repostd/cmd/repocheck@main`.
     It downloads and builds on first use, installs nothing, and never
     edits the repo's `go.mod`. `GOTOOLCHAIN=auto` fetches a newer Go if
     the installed one is too old. `GOPROXY=direct` reads `main` straight
     from GitHub, because the module proxy serves a cached `@main` for a
     while after each push.
   - `nix` is on `PATH`: `$RC` is
     `nix shell nixpkgs#go -c env GOTOOLCHAIN=auto GOPROXY=direct go run github.com/discobox-ai/repostd/cmd/repocheck@main`.
   - Otherwise stop, and tell the user to install Go.
2. **New repo?** If the directory is empty or has no commits, this is a new
   repo. For a node repo, run `pnpm init`, set `packageManager`,
   `devEngines.runtime` and the standard scripts (§4), then `$RC init`, fill
   in every TODO, and continue at step 3. For a Go repo, run, in order, and
   then report as in step 5:
   - `git init -b main` when it is not a repo yet.
   - `go mod init github.com/discobox-ai/<repo>`, asking the user if the
     module path is not obvious. `$RC init` refuses to run without a
     `go.mod`, because the starter files carry the module path.
   - `$RC init`, which writes every starter and managed file and the
     symlinks.
   - `env GOPROXY=direct go get -tool` for
     `github.com/go-task/task/v3/cmd/task`,
     `github.com/golangci/golangci-lint/v2/cmd/golangci-lint` and
     `github.com/discobox-ai/repostd/cmd/repocheck@main`.
   - `go mod tidy`, because `go get -tool` leaves `go.sum` incomplete and
     `verify` would fail on it.
   - `nix flake lock`, then write `cmd/<repo>/main.go` and
     `internal/version/version.go`.
   - Fill in every TODO the starters leave: what the repo is, in `README.md`,
     `DESIGN.md` and `AGENTS.md`'s Project Structure.
   - `$RC` until clean, then `nix develop -c go tool task ci`, then commit.
   - Creating the GitHub repo and pushing is the user's call; ask.
3. **Check.** Run `$RC`. It prints one finding per line, in the form
   `path: severity [rule-id] message`, then a summary. It exits 1 when any
   unwaived error remains. `$RC rules` lists every rule.
4. **Review.** Read the untagged rules below against the repo; they are
   what `$RC` cannot check. Examples: packages in `pkg/` that nothing
   imports, a `DESIGN.md` describing planned work, workflows with logic
   beyond task calls.
5. **Report.** Group the findings by section (layout, docs, ADRs, agents,
   env, lint, tests, CI, release, git). For each group, say what is wrong
   and whether the fix is mechanical (step 6) or needs a decision. Stop
   here unless the user asked to fix or conform.
6. **Conform**, when asked:
   - With neither a `go.mod` nor a `package.json`, ask the user which
     stack, and for a Go repo the module path (normally
     `github.com/discobox-ai/<repo>`), and run `go mod init <path>` or
     `pnpm init`. `$RC init` refuses to run without one, because the
     starter files carry the module path or package name.
   - In a Go repo, pin the tool:
     `env GOPROXY=direct go get -tool github.com/discobox-ai/repostd/cmd/repocheck@main`.
     From then on `$RC` is `go tool repocheck`. A node repo keeps running
     it through `go run`; its CI pins the version (§8).
   - Run `$RC init`. It writes the starter files that are missing, the
     managed files, and the symlinks, and never overwrites an existing
     file. Where the repo already has its own version of a starter file,
     `$RC show <path>` prints the starter filled in for this repo (for
     example `$RC show Taskfile.yml`). Merge what it adds into the repo's
     file by hand, such as Taskfile targets, flake packages or AGENTS.md
     sections, and fill in every TODO.
   - Run `$RC sync` whenever managed files or ADRs change.
   - Fix everything else by hand, following the rule text below. Run
     `nix flake lock` if the repo has a new flake.
   - Re-run `$RC` until it is clean, then run `go tool task ci`.
   - Anything that needs a decision goes to the user; don't guess. Moving
     packages, renumbering ADRs and deleting files all need a decision.

- **Waivers.** Add one to `.repocheck.yaml` only when conforming is wrong for
  this repo, not merely inconvenient. A waiver is a `rule`, an optional
  `path` glob, and a required `reason`. Tell the user about every new
  waiver.
- **Managed files** come from repostd: `.github/actionlint.yaml`,
  `docs/adr/template.md` and this skill in every repo; `.golangci.yml` and
  `.github/actions/task/action.yml` in a Go repo; `oxlint.repostd.json`,
  `oxfmt.config.ts`, `.github/actions/pnpm/action.yml` and
  `.github/actions/repocheck/action.yml` in a node repo. Change them
  upstream, never locally. The one exception is inside the `repostd:local`
  blocks of `.golangci.yml` and `oxfmt.config.ts`; a node repo's own lint
  additions go in its `.oxlintrc.json`, which extends
  `oxlint.repostd.json`.

## 1. Code layout

- *(Go)* Root directories are only `cmd`, `pkg`, `internal`, `docs`, `test`
  and `scripts`, plus dot-directories. No Go packages at the root.
  `layout.root-dirs`, `layout.root-go`
- *(node)* A UI is a Remix 3 app in Remix's documented layout: `server.ts`
  at the root; under `app/` the route map `routes.ts`, the router
  `router.ts`, `middleware/`, controllers and route actions in `actions/`,
  shared views in `ui/`, and `assets.ts`; browser source in `public/`
  directories beside its narrowest owner. Learn it from Remix's "Project
  tour" (`node_modules/remix/guides/01-start-here.md`), and let
  `remix doctor --strict` check it (§4). Code outside the request flow
  (a data layer, a mock backend) gets a directory of its own under `app/`
  or beside it, named for what it is.
- Binaries go in `cmd/<binary>`. Repo-only tools (release, codegen) go in
  `internal/cmd/<tool>`.
- *(Go)* Use one Go module. `go.work` needs a waiver, and every task then
  covers every module. `layout.go-work`
- `pkg/` holds only packages other programs import. Default to `internal/`.
- Generic helpers useful beyond one repo belong in
  `github.com/discobox-ai/x`.
- Generated code is committed as `*_gen.go` or under `gen/`, and is produced
  by `//go:generate` directives that `task generate` runs.
- No Makefile. *(Go)* No `tools.go`, no goreleaser. `layout.no-makefile`,
  `layout.no-tools-go`, `layout.no-goreleaser`

## 2. Docs

- `AGENTS.md` is the real file. `CLAUDE.md` is a symlink to `AGENTS.md`
  wherever either exists. `docs.agents-symlink`
- The root `AGENTS.md` has these `##` sections: Project Structure, Shared
  Libraries, Git Workflow, Commands, Implementation Quality, Package Design
  Docs, Architecture Decision Records. `docs.agents-sections`
- Required files: `README.md` (user-facing), `DESIGN.md`, `LICENSE`
  (Apache-2.0), `NOTICE` and `SECURITY.md`. A design doc is always named
  `DESIGN.md`: move a `docs/design.md` to the root rather than keeping two.
  `docs.required-files`, `docs.license`
- `DESIGN.md` and `REVIEW.md`:
  - They sit next to the code and are read from the root down; closer files
    override their parents.
  - A `REVIEW.md` needs a `DESIGN.md` beside it. `docs.review-design`
  - A `DESIGN.md` over 300 lines is a warning and over 600 lines is an
    error: split it into child packages' docs. `docs.design-length`
  - `DESIGN.md` describes current state only, never planned work. Parents
    link to children instead of repeating them. Prefer Mermaid for
    structure and flows. Keep them short, directive and scannable.
  - `REVIEW.md` is a bullet list of review rules and pitfalls.
- Every Mermaid block starts with a known diagram type. `docs.mermaid`
- No plan or task documents in the repo: plans belong in the branch or task.
  `docs.no-plans`

### ADRs (`docs/adr`)

- Filenames are `NNNN-slug-stating-the-decision.md`: four digits, unique,
  with a lowercase hyphenated slug. `adr.unique`
- The header is exactly:

  ```markdown
  # NNNN — Title
  - **Status**: Proposed | Accepted | Rejected | Superseded by NNNN
  - **Date**: YYYY-MM-DD
  - **Supersedes**: NNNN        (optional)
  ```

- "Superseded by B" in A and "Supersedes: A" in B always appear together.
  `adr.supersede`
- The sections are `## Context`, `## Decision`, `## Alternatives rejected`
  and `## Consequences`, plus `## Deferred` when something was deferred.
  `adr.format` checks the filename, header and sections.
- `docs/adr/README.md` holds the index between
  `<!-- repostd:adr-index -->` markers. `repocheck sync` generates it.
  `adr.files`, `adr.index`
- Write an ADR only when a plausible alternative was rejected for a
  non-obvious reason, or something was deferred with a condition for
  revisiting it. Otherwise update `DESIGN.md`.
- Land the ADR as `Proposed` before implementation; `Accepted` is the
  go-ahead. Amend it while nothing has shipped against it, and supersede it
  after.

## 3. Agent config

- Skills live in `.agents/skills/`. `.claude/skills` is a symlink to
  `../.agents/skills`. `agents.skills-symlink`
- `.claude/settings.local.json` is never committed.
  `agents.no-settings-local`
- `.discobox/hooks/NN-<name>.sh` run on change inside a discobox. Each
  declares itself in a `#---` frontmatter block (`name`, `type: file|session`,
  `pattern`, optional `notify_llm` and `phase: review`) and is a thin trigger
  for `go tool task <target>`, or a node repo's `pnpm run <script>`. A hook
  never runs go, gofmt, golangci-lint, oxlint, oxfmt, tsc or another tool
  directly: the rule lives in the Taskfile or package.json so the hook, a
  terminal and CI cannot drift. The baseline is fmt, tidy (Go), test and
  check. `discobox.hooks`
- `.discobox/services/NN-<name>.sh` are the long-running processes a
  discobox starts. They declare themselves the same way (`name`,
  `description`, `ports:` when dockerd publishes them) and set the
  environment a developer would. A service for this repo's own program
  `exec`s a Taskfile target; an auxiliary one, such as a metrics dashboard,
  may run a container. `discobox.services`
- A repo with a `dev` target or script has a service that runs
  `go tool task dev` or `pnpm run dev`, so the dev loop is running as soon
  as the sandbox is up. `discobox.dev-service`

## 4. Environment and tools

### Go

- `flake.nix`, `flake.lock` and `.envrc` (`use flake`) are required. The
  flake's default devShell provides Go and every non-Go tool CI uses:
  shellcheck, actionlint, node, and so on. CI gets its environment only from
  the flake. `env.flake`, `env.envrc`
- Go-program tools are pinned by `tool` directives in `go.mod` and never also
  in the flake, because two pins drift. The minimum set is task,
  golangci-lint and repocheck. `env.go-tools`, `env.flake-go-tools`
- Always run task as `go tool task`. `Taskfile.yml` defines these targets
  (`env.taskfile-targets`):

  | Target | What it does |
  |---|---|
  | `build` | Builds into `build/`. |
  | `run` | Runs the program, where there is one. |
  | `dev` | Hot-reloads it through watchnbuild, where there is one. `env.dev-watch` |
  | `test` | `go test -race`. `env.test-race` |
  | `check` | golangci-lint, `repocheck`, shellcheck, actionlint. |
  | `fmt`, `tidy`, `generate` | Format, tidy go.mod, regenerate code. |
  | `verify` | fmt, tidy, generate and `repocheck sync` leave the tree unchanged. |
  | `ci` | `verify` + `check` + `test`. |

- A repo with a long-running program (a server, a daemon) has a `dev`
  target: hot reload through `go tool watchnbuild`, configured by
  `.wnb.yaml` at the root, with `.wnb.<name>.yaml` for a second loop such as
  a CLI built beside the server. Pin
  `github.com/discobox-ai/watchnbuild` as a tool. Every config is used by a
  dev target, and every dev target drives watchnbuild. `env.dev-watch`
- Logic beyond a few lines goes in a Go program under `internal/cmd`, not in
  Taskfile shell or workflow YAML. Shared programs go in repostd or `x`.

### node

A node repo uses node and pnpm and nothing else: no `go.mod`, Taskfile or
flake (ADR 0002).

- `package.json` pins pnpm exactly in `packageManager` (`pnpm@X.Y.Z`) and
  node in `devEngines.runtime`
  (`{"name": "node", "version": "X.Y.Z", "onFail": "download"}`), the
  fields pnpm itself reads to install both. `pnpm-lock.yaml` is committed,
  and no other package manager's lockfile is. `env.node-pins`
- The targets are `package.json` scripts, run as `pnpm run <script>`, with
  the Taskfile targets' names and meanings (`env.node-scripts`):

  | Script | What it does |
  |---|---|
  | `build` | Builds into `build/`, where there is a build. |
  | `dev` | Runs the program with hot reloading, where there is one. |
  | `test` | Runs the tests. |
  | `check` | `oxlint`, `oxfmt --check`, `tsc --noEmit`, and in a Remix app `remix doctor --strict`. |
  | `fmt`, `generate` | `oxfmt --write`; regenerate code. |
  | `verify` | fmt and generate leave the tree unchanged. |
  | `ci` | `verify` + `check` + `test`. |

  A script may call others (`pnpm run check:lint`); the rule follows the
  calls.
- Lint and format are oxlint and oxfmt, the tools Remix 3 itself uses
  (§6). `package.json` depends on `oxlint`, `oxlint-tsgolint` (its
  type-aware rules, on TypeScript 7's compiler), `oxfmt` and `typescript`,
  and `.oxlintrc.json` extends the managed `oxlint.repostd.json`.
  `env.node-tools`
- Committed generated files go in `oxfmt.config.ts`'s local block, and in
  `.oxlintrc.json`'s `ignorePatterns`. The managed part already ignores
  what pnpm and repocheck write.
- Logic beyond a few lines goes in a script file the `package.json` script
  runs, not in a long one-line script or workflow YAML.

## 5. Server configuration

*(Go)* A repo whose server binary is `cmd/<name>-server` follows this. A
library or CLI repo has no server and skips it.

- The server is configured by **one file plus environment overrides, and no
  configuration flags**. It accepts only `--config` (where the file is),
  `--check-config` (validate and exit, for deploy pipelines) and
  `--version`. `config.no-flags`
- The **`Config` struct is the source of truth**. The JSON schema and the
  commented example config are generated from it by `task generate` and
  committed, so `verify` fails when they drift. `config.schema`
- Use the shared loader `github.com/discobox-ai/x/config`; nothing about
  loading, env binding or schema generation is written per repo.
- **Precedence** is defaults, then file, then environment, applied in one
  direction and never overridden later. Whatever wins, always wins.
- **An unknown key in the file fails startup**, naming the key.
- **Environment names are derived** from the key path:
  `<REPO>_<KEY_PATH>` upper-snaked, e.g. `iroh.logLevel` →
  `<REPO>_IROH_LOG_LEVEL`. A variable matching that prefix but no setting
  fails startup, so a misspelled override is never silently the default. An
  explicit name is the escape hatch for a spelling an external spec fixes,
  such as the `OTEL_*` variables.
- **Secrets** are settable by file path (`file:/run/secrets/key`), because
  Kubernetes, systemd and Docker all deliver secrets as files.
- A `.env` file is a development convenience, named for the program
  (`.<binary>.env`), never plain `.env`, and never replaces a variable that
  is already set.

## 6. Lint

- `.golangci.yml` is managed. Repo additions go only inside the
  `# repostd:local` blocks. When the standard drops a block that still has
  lines, `sync` leaves the file alone and fails: move the lines into a
  remaining block. `managed.files`
- It bans testify with depguard, and `nolintlint` requires every `nolint` to
  be specific and explained.
- *(node)* `oxlint.repostd.json` and `oxfmt.config.ts` are managed the
  same way, and both follow Remix 3's own configs. oxlint runs only the
  rules the standard names (type-only imports and exports, `.ts` import
  extensions, `no-var`, concise arrow bodies), type-aware. A repo's
  `.oxlintrc.json` extends `oxlint.repostd.json` and owns its
  `ignorePatterns` and any rules it adds. oxfmt is Remix's style: 100
  columns, no semicolons, single quotes; its additions go only inside
  `oxfmt.config.ts`'s `// repostd:local` block.

## 7. Tests

- *(Go)* Use the standard `testing` package. testify is banned; go-cmp is
  allowed. `tests.no-testify`
- *(Go)* Integration and e2e tests are named `*_integration_test.go` or
  `*_e2e_test.go` and call `t.Skip` unless `<REPO>_INTEGRATION=1` or
  `<REPO>_E2E=1` is set. Build tags are only for platforms.
  `tests.no-integration-tags`
- *(Go)* Every `*_INTEGRATION` or `*_E2E` variable the Taskfile sets is read
  by a test. `tests.env-read`
- *(node)* Tests run on node's own test runner, or `remix test` in a Remix
  app, not a second framework. Integration and e2e tests skip unless their
  `<REPO>_INTEGRATION` or `<REPO>_E2E` variable is set, as in Go.
- Test-helper packages are named `<pkg>test`. Test names read as sentences.

## 8. CI (GitHub Actions on Depot runners)

- `.github/workflows/ci.yml` runs on `pull_request` and on `push` to `main`,
  with jobs `verify`, `check` and `test`. Each job runs `actions/checkout`
  and then `./.github/actions/task` with its target, or in a node repo
  `./.github/actions/pnpm` with its script. A node repo also has a
  `repocheck` job running `./.github/actions/repocheck` at a pinned
  version: repocheck and actionlint are Go programs, and that job is the
  only one that uses Go. `ci.workflow`
- Every job runs on a Depot runner (`depot-ubuntu-*`, `depot-macos-*`,
  `depot-windows-*`). `ci.runners`
- Workflows install no tools: no `actions/setup-*`. Nix does not run on
  Windows, so a Go repo's `depot-windows-*` job may use `actions/setup-go`
  with `go-version-file: go.mod` and nothing else. A node repo's toolchain
  is installed by the managed pnpm and repocheck actions only.
  `ci.no-setup`
- Workflows contain no build logic. Every `run:` is one
  `[nix develop -c] go tool task <target>`, or a node repo's
  `pnpm run <script>`. `ci.run-steps`
- Every `uses:` is pinned to a 40-character commit hash with a `# vX.Y.Z`
  comment. `ci.pinned`
- Top-level `permissions: contents: read`; jobs widen only what they need.
  `actions/checkout` sets `persist-credentials: false`. `ci.permissions`,
  `ci.checkout-credentials`
- Keep Windows CI where the repo ships for Windows.
- `renovate.json` extends `github>discobox-ai/repostd`, which covers gomod
  (including `tool` directives), npm, action digests and `flake.lock`.
  `ci.renovate`

## 9. Release

- No goreleaser. `.github/workflows/release.yml` runs on `v*` tags and calls
  only `go tool task release:*` targets, or a node repo's `release:*`
  scripts. `ci.release`
- *(Go)* Release logic (archives, checksums, manifests, images, the
  Homebrew tap) is written as Go programs: `internal/cmd` when it is
  specific to the repo, and a repostd `go tool` when it is shared.
- Tags are SemVer `vX.Y.Z`, with prereleases as `-alpha.N` or `-rc.N`. The
  version is set by ldflags into `internal/version` and falls back to the
  VCS revision.

## 10. Git

- `.gitattributes` starts with `* text=auto eol=lf`. `git.attributes`
- `.gitignore` covers `build/`, `.direnv/`, `.env` and `result`, or in a
  node repo `node_modules/` and `.env`. `git.ignore`
- Commit messages are Conventional Commits: `feat`, `fix`, `refactor`,
  `docs`, `test`, `chore`, `perf` or `style`.
