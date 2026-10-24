# MemQL Cockpit: slim + rename — design

**Date:** 2026-08-25
**Status:** approved (owner brainstorming session)
**Scope:** sub-project A of the Cockpit revival roadmap
**Repos:** `znasllc-io/memql-cockpit` (nearly all of it), `znasllc-io/memql` (docs sweep only)

## Context

memql-cockpit began as a terminal-native IDE and operations console: a tcell
TUI plus worker modes. The TUI is unused; the portal and the VS Code extension
now own every interactive surface it carried. What the platform needs from
this repo is the machine-side half of the fleet: the worker runtime (headless
and computer-use), app sessions, and the small CLI that enrolls and operates a
machine.

This design removes the TUI outright and renames the installed command to
`memql`. It is sub-project A of a five-part roadmap agreed 2026-08-25:

- **A. Slim + rename** (this document)
- **B. Connection security + recovery hardening**
- **C. Fleet management polish**
- **D. Distribution** — per-OS installers + a portal download section
- **E. macOS menu bar app**

B–E each get their own design before any implementation.

## Decisions

- **D1 — the installed command is `memql`.** The product name stays
  **MemQL Cockpit**; the repo and module path stay
  `github.com/znasllc-io/memql-cockpit`. The binary builds from `cmd/memql/`,
  so `go install .../cmd/memql@latest` produces the right name naturally.
  The engine repo also builds a `bin/memql`, but it ships only inside
  container images and runs in pods — different distribution channels, no
  PATH overlap on user machines. A short note goes in both repos so nobody
  "fixes" the collision later.
- **D2 — the TUI is removed, not deprecated.** It is unused. No shims, no
  redirect stubs, no compatibility window (pre-1.0).
- **D3 — subcommand keep/kill** (owner call, 2026-08-25): keep `worker`,
  `cluster`, `login`, `logout`, `creds`, `lint`, `setup project`; kill the
  bare TUI launch, `deploy`, `run`, `cut`, `rollback`, `harness`, and the
  `genesis` / `authorize` redirect stubs.
- **D4 — one installed command, two build variants.** Headless (default,
  CGO off) and computeruse (`-tags computeruse`, CGO + RobotGo) both install
  as `memql`; the variant is an install-time choice. The separate
  `memql-cockpit-computeruse` command name is retired. `memql --version`
  prints the variant; worker registration already reports capability per
  build. Dev cross-builds may still emit suffixed artifacts in `bin/`.
- **D5 — execution: rebuild the keeper skeleton.** New `cmd/memql` +
  `internal/` layout populated by `git mv`; delete `cli/` and the deploy
  family wholesale. Hidden TUI dependencies surface as compile errors
  instead of surviving unnoticed.
- **D6 — the wire contract is untouched.** Old and new binaries are
  wire-identical to the cluster. The engine needs no deploy; the fleet
  upgrades machine-by-machine by re-running the installer.
- **D7 — lint's engine dependency stays.** `lint` needs only
  `component/memql/dslimports` (import resolution), which is light.

## Command surface

```
memql                          usage + exit 2
memql --version | version      version + variant (headless / computeruse)
memql cluster add <url|name>   NEW — discovery via /.well-known/memql-config.json + OAuth login
memql cluster list | remove
memql login <cluster>          re-authenticate (absorbs the TUI 'L' authorize flow)
memql logout <cluster>
memql creds <subcommand>
memql worker pair|run|setup|config|consent
memql lint [path]
memql setup project [flags]
```

Unknown commands: error + usage, exit 2. Nothing else.

## Repo layout

Populated by `git mv` so history survives:

```
cmd/memql/main.go        thin dispatch only
internal/auth/           <- cli/auth
internal/config/         <- cli/config      (cluster registry, ~/.memql, clusters.yaml)
internal/crash/          <- cli/crash
internal/creds/          <- cmd/memql-cockpit/creds.go + store
internal/worker/         <- cmd/memql-cockpit/internal/worker
internal/lint/           <- cmd/memql-cockpit/internal/lint
internal/setupproject/   <- cmd/memql-cockpit/internal/setupproject
```

**The move rule is mechanical: a package moves iff the surviving command
graph imports it; everything else dies.** Deleted wholesale: the rest of
`cli/` (ui, canvas, editor, dsledit, wizard, splash, settings, concepts,
cluster, healing, dockerprobe, pool, app.go, and their tests),
`cmd/memql-cockpit/internal/{deploy,harness}`, and the deploy/cut/rollback
handling in main.go.

## Dependency diet

The per-module `replace` mechanism against the pinned memql sibling
(memql-cockpit#328 / #330) stays — it is how this repo consumes the engine's
nested modules. What changes is the set it carries. The keeper graph needs
the wire tier: `component/grpc/gen`, `component/node/gen`, the SDK packages,
`component/identity/workerpairing`, plus `component/memql/dslimports` for
lint. The embedded-runtime tier drops: `component/genesis`,
`component/secret`, `component/architecture`, and the engine module's heavy
internals. With the TUI gone, `tcell` and the TUI's `x/image` leave go.mod.
The require + replace lists shrink to what the survivors import — `go mod
tidy` decides, the CI pin check verifies.

## Headless replacements for the TUI-only flows

- **`memql cluster add <discovery-url>`** — fetch
  `/.well-known/memql-config.json`, register the cluster, and run the OAuth
  login in one motion (what the TUI `A` keypress did). `memql login
  <cluster>` re-authenticates (the `L` flow). Both reuse the OAuth machinery
  moving from `cli/auth`: browser + localhost-callback flow when a desktop
  is present, RFC 8628 device-code flow as the SSH/headless fallback. The
  identity service already serves `POST /device/code`; whether the client
  half exists in `cli/auth` is verified in the plan — if absent it is added,
  since enrolling a fleet machine over SSH is a mainline case, not an edge.
- **`memql worker setup`** (computeruse build) — the tcell wizard becomes
  sequential stdin/stdout prompts; same TCC/X11 pre-flight underneath, plus
  `--non-interactive` for scripted installs: reports what is missing with
  honest exit codes, never prompts.
- **Genesis first-run** — deleted, not replaced. It sealed an env for the
  embedded engine runtime, which no longer ships. The `ApplyLocalOverride`
  startup call dies with it; worker configuration remains `~/.memql/` +
  `policy.yaml` + flags.

Nothing else is orphaned: DSL authoring lives in the VS Code extension and
the portal, concepts browsing and observability in the portal, and
`cluster list` keeps synthesizing the always-present `local` row.

## Installer + service migration

- `install.sh` keeps its raw-URL location (repo name unchanged, existing
  links keep working) but installs `memql`, and gains a cleanup step: stop
  and remove the old `com.znasllc.memql-cockpit-worker` LaunchAgent (and
  the systemd equivalent on Linux), remove old `memql-cockpit*` binaries,
  register `com.znasllc.memql-worker`.
- `~/.memql/` is unchanged — config and file-backend tokens survive.
  Whether Keychain entries are keyed by a name that needs migrating is read
  from the keyring code during planning; if so, `memql creds migrate`
  learns the rename, otherwise one `memql login` re-auth is acceptable at
  alpha.
- CI/release workflows rename artifacts (`memql-darwin-arm64`, ...) in
  lockstep with what `install.sh` downloads.
- VERSION bumps a minor (pre-1.0 breaking change).

## Engine-repo sweep (docs only)

`grep -rn memql-cockpit` over `znasllc-io/memql`: CLAUDE.md's worker and
cockpit sections, `docs/public/operate/workers-runbook.md`,
`docs/public/operate/local-apps.md`, and the portal's
`clients/portal/src/fleet/install.ts` one-liner composer. Lands after (or in
lockstep with) the cockpit release that ships the rename.

## Testing

- Surviving suites move with their packages; TUI tests die with `cli/`.
- Added: dispatch-level usage/exit-code tests; `cluster add` / `login`
  against a mock discovery + OAuth server; the non-TTY `worker setup` path;
  a plist/service-label golden test; installer cleanup coverage in the
  repo's existing script-test pattern.
- Gate: `go build ./...` + `go test ./...` (single module — no workspace
  trap here) + the CI pin check.
- Live verification: install on the owner's Mac, `memql worker run` against
  the production cluster, then the portal fleet one-liner end-to-end.

## Risks

1. Hidden tcell entanglement inside keepers — surfaced as compile errors by
   the skeleton rebuild; resolved by rewriting as plain prompts.
2. The pinned-sibling mechanism means dependency changes ride the pin-bump
   discipline (memql-cockpit#328); shrinking the set must follow it, not
   fight it.
3. Device-code client work is contingent on what `cli/auth` already has;
   scoped during planning, not discovered mid-implementation.

## Out of scope

Protocol changes of any kind, portal changes beyond the docs one-liner,
Windows support, installers/DMG (sub-project D), the menu bar app (E),
connection hardening (B), fleet routing polish (C).
