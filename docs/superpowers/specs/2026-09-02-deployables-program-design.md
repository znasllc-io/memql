# The Deployables program -- Design

- **Date:** 2026-09-02
- **Status:** approved (in-session Q&A with the owner; every fork below
  records the choice that was made and why)
- **Scope:** a PROGRAM record for four epics over the MemQL OS Deployables
  app (`clients/os/src/apps/deployables/`), the packages pipeline
  (`component/packages/`, `dsl/platform/`), the edge (`component/edge/`),
  the logging and observability runtime (`core/logger`, `component/observe/`),
  the workbench binding (`integrations/workbench/`) and the Fleet dispatch
  seam (`integrations/agent/worker/`). Each epic gets its own design record;
  the first, Compose, is
  [2026-09-02-deployables-compose-design.md](2026-09-02-deployables-compose-design.md).
- **Order:** Compose, then Logs, then Build, then Run (P8 below).

## Why

Owner's brief, condensed. Deployables needs a lot of attention. A private
repository could not be configured from the app. Creating a deployable sits
under a section called Actions, which names nothing a person is trying to do.
To create a deployable, give it an address, bind a domain and tie it to a
client, a person jumps between Packages, Actions and Sites. The map is good
and stays. The stage rail that shows how far a deploy has got is good and
stays. Composing a deployable needs one flow, and the flow depends on what
kind of deployable it is: the kinds supported today, and iOS, Android and
macOS later. It has to work end to end, report the deployable's status, and
carry everything needed to manage and maintain a deployable after it is
live. Most repositories will be private.

A second brief arrived mid-session: a Logs app. Every app gains a Logs
section, visible to owners, developers and admins, showing that app's own
logs. The Logs app itself shows everything: every OS app, every integration,
the engine, every component, the observability runtime, the deployments,
and the MemQL OS front end itself, so an error on either side of the wire is
persisted and findable in one place. The portal is skipped: it is deprecated
and will be removed, though not yet. Log lines persist to the database, are
kept for thirty days, and are backed up before they are swept. Where
possible, the browser console of a hosted deployable is captured too, so
nobody has to open a browser, then a pod, then a database to follow one
fault.

## What was verified before any of this was decided

- **Builds do not run.** `component/packages/builder.go` records that the
  build seam is nil in production: a package whose built output is not
  committed is refused at the build stage with a typed message. Only a tree
  carrying `dist/index.html` deploys. The design put builds on the workbench
  (memql#4794, D4); the binding was filed rather than built because the
  workbench keys every workspace on a Plan id (memql#4354).
- **A private repository cannot be connected from the OS.** The form takes
  the NAME of a `v1:platform:globalSecret`. Settings writes only the slots
  the secrets manifest declares (`integrationConfigure`), and there is no
  repository slot, so no OS surface can create the secret. The operator doc
  says "or the Settings surface"; that surface does not exist. The only path
  is the cockpit CLI.
- **Any package owner may name any cluster secret.** `createPackage` accepts
  any `repoTokenRef` string and the fetcher resolves it cluster-wide. A
  guessed name from the public manifest would make the engine fetch under an
  owner's token.
- **GitHub only.** The fetcher speaks the GitHub tarball API and refuses
  every other host, naming the zip upload as the way around it.
- **iOS, Android and macOS have no schema**, deliberately (memql#4794, D5):
  the site row is hostname-resolved and those are store-distributed. They
  are drawn from a client-side list as "coming soon".
- **A deploy is one call on one node.** The pipeline runs the whole sequence
  in a single call; a node lost mid-run leaves the row at a non-terminal
  status with no recovery but a new attempt.
- **The edge emits no per-site metrics.** Traffic and health is new work,
  not a surfacing of something measured already.
- **The workbench keys on `planId`**; a build workspace needs one narrow
  addition, an owner that is a user and a key that is the deployment.
- **A Mac build is dispatchable today** through the Fleet's host dispatch
  with an `os=darwin` requirement, since the cockpit reports that label.

## Locked decisions (program level)

| # | Decision | Choice (owner-approved) |
|---|---|---|
| P1 | How a deployable is made | **One flow, source first.** Every deployable starts at New deployable; the first stop is where it comes from: a repository, a zip in Files, or the person's own CI pushing bundles through the existing service-account route. The hand-made site becomes the "pushed by my CI" source rather than a separate path. Actions disappears. Rejected: two paths under one door (two mental models survive); source only (orphans the two paths that work today) |
| P2 | Where a deployable is built | **The workbench, with the Fleet for what needs a Mac.** Web builds run in-cluster on the sandboxed workbench; the build workspace is owned by the package's owner instead of a Plan. Kinds that need macOS tooling route to a Mac in the person's Fleet. Mirrors the product's own doctrine: workbench first, your machine for what a workbench cannot do. Rejected: workbench only (no mobile story); Fleet only (a deploy waits on a laptop, and an agent-driven deploy has no in-cluster path); the repo's own CI (per-repo config and a cluster credential, rejected once already in D4 of memql#4794) |
| P3 | How a private repository is connected | **Personal credentials, pasted once in the flow.** A new concept owned by the person, sealed server-side; the package points at a credential the caller owns and the fetcher refuses any other. A GitHub App can populate the same rows later. Rejected: GitHub App first (its own epic, and it needs this row underneath); a Secrets page over cluster-wide names (keeps the borrowing hole and the two-place dance) |
| P4 | iOS, Android, macOS in this round | **Define the target model, ship web only.** A target states its address stop, its build surface, its live states and its row; the OS renders every stop from the registry and web is the only registered target. A manifest declaring an unoffered kind is refused with a sentence that says so, scoped to that app. No fields without writers. Rejected: a first mobile slice now (doubles the epic); full mobile (its own program) |
| P5 | The client's own domain and the client tie | **In the flow's address stop, optional.** The deploy never waits on DNS: the app goes live at its cluster address and the domain stays "waiting on your DNS records" on the page until both check out. Shown only to a cluster owner, who is who may bind a domain today |
| P6 | The composition surface | **The rail is the form.** One vertical rail whose stops expand in place; the SAME rail later shows deploy progress and then the deployable's standing status. Rejected: a stepper (a second device beside the rail, and a refusal at step four pages you back to step one); a flowchart canvas (a node editor over a form that is a straight line, and a second canvas beside the map) |
| P7 | Management scope | **All four**: auto-deploy when the source moves, abandoned-run detection and retry, runtime settings per deployable, traffic and health on the Live stop. The first two belong to Build, the last two to Run |
| P8 | Order | **Compose, Logs, Build, Run.** Compose fixes what the owner hit and needs nothing from Logs. Logs lands the store, the app and the per-app sections before Build, so build output is written to the store from day one and Run's traffic and health ride the edge's request log. Rejected: Logs first (the disjointed surface survives for the length of that epic); Build before Logs (the bounded log tail would be re-pointed afterwards, a second pass over the same stage) |
| P9 | The Logs requirements | Persist every node's log lines to the database beside the observability rows; thirty-day retention with an archive to blob storage before the sweep; consolidate engine, integrations, components, observability, deployments and the OS front end; skip the portal; capture hosted sites' browser console output where possible and the OS's own front-end errors always; a Logs app with filtering by app, component, integration, node, level, time and subject; a Logs section on every app, admin and above (owner, developer, admin under the one ladder) |

## The four epics

### Epic 1 -- Compose

The rail-as-form flow, the Map / Deployables / Settings restructure, the
target registry with web, personal source credentials, the CI-pushed
source, the address stop carrying the client and the client's own domain.
Design record:
[2026-09-02-deployables-compose-design.md](2026-09-02-deployables-compose-design.md).

Done when: a private GitHub repository whose build output is committed
deploys from New deployable to a live hostname in the OS, with its token
pasted once and never shown again; a package with two apps lands as two
rows sharing a source; a manifest declaring `kind: ios` deploys its web
apps and says why the iOS one did not; the deployable's page carries every
manage-time act the portal and the three retired sections offered.

### Epic 2 -- Logs

The store, the app, the per-app sections, and capture on both sides of the
wire. Its design round decides the following, and this record states only
what is fixed.

- **A log concept in the observability namespace**, a hypertable in the
  `code_invocation` tradition, written by an `slog.Handler` installed beside
  the console handler on every node type. Batched and bounded: a log store
  that can wedge a node when the database is slow is worse than no store.
  Levels and sampling are decisions for the round; the shape carries at
  least time, node, level, component, message, attributes, and `subject`.
- **`subject` is the seam with Deployables** (and with every other app): a
  line names what it is about, such as a deployment id, a site id, a plan
  id or a user id. The Build and Live stops of a deployable open the Logs
  section already filtered to that subject.
- **Retention is a scheduled automation**: thirty days, archive to blob
  storage first, hard-delete second, the `authActivity` precedent
  (`MEMQL_IDENTITY_AUTH_ACTIVITY_RETENTION_DAYS`). The archive format and
  its restore path are decided in the round.
- **The OS front end writes its own errors, unhandled rejections and
  console output** into the same concept over the gRPC bridge it already
  holds, tagged with the app id and the session. No new route.
- **Hosted sites are third-party bundles**, so capturing their console
  needs a collector the edge injects and an ingest path. Whether that path
  is the site's `apiProxy` mount, a new HTTP exception, or nothing in v1 is
  an explicit decision for the round, under the gRPC-first policy. The
  portal is excluded from capture and from the app's filters.
- **The per-app section is a shell convention**, the way `settingsSection`
  is: a manifest field naming the Logs section, gated `{ min: "admin" }`,
  rendering the app's slice of the same concept. The Logs app is the whole
  concept with the full filter set.

### Epic 3 -- Build

- **Workbench binding**: a build workspace keyed on the deployment id and
  owned by the package's owner, reached through an internal-origin path the
  agent tool loop never sees. The fast path (committed output) stays. The
  build stop reports which surface built it.
- **Fleet routing** for apps whose target needs macOS tooling: the same
  environment-hint and reroute the agent tool loop uses (`needs:
  macos_tooling` to `os=darwin`), through the existing host dispatch. In
  this round that is the mechanism with no registered target that uses it;
  the first mobile target is what exercises it.
- **Abandoned-run detection**: a heartbeat on the deployment row while a run
  is live, a terminal `abandoned` status stamped by a sweep when it stops,
  and Retry starting a new run from the stored snapshot without refetching.
- **Auto-deploy when the source moves**: a per-package switch. The feeds
  already notice a push; when the new analysis plans exactly what the last
  confirmed run planned, the run confirms itself; a changed plan parks at
  the confirm stop as today.
- **Build output goes to the log store** with the deployment as its
  subject; the row keeps a pointer and a bounded tail for the list.

### Epic 4 -- Run

- **Runtime settings per deployable**: a settings object on the site row,
  merged into the runtime-config document the edge already serves, so one
  bundle can serve two deployables against different endpoints. Secrets
  stay named, never valued, exactly as the storefront binding does.
- **Traffic and health on the Live stop**: the edge writes a request log
  into the log store with the site as its subject; a continuous aggregate
  folds it into requests, errors and last-served-at; the Live stop reads
  the aggregate. The aggregate is the `code_invocation_1m` pattern, so the
  raw log and the figure cannot disagree.

## Seams every epic must honour

- **`placements` on `packageDeploy`** is the one write that places an app:
  hostname, client, own domain. Build and Run add nothing to it.
- **A credential is resolved under the package owner's actor**, never the
  caller's and never cluster-wide. Build's Fleet route inherits this: a
  machine that fetches does so under the same rule.
- **The Rail renders from the target.** Build changes what the Build stop
  says, not where it is; Run changes what the Live stop says. Neither adds a
  stop; a stop is a target's decision.
- **`subject` on a log line** is the only way a deployable and its logs are
  joined. No epic invents a second join.
- **Multi-node is the default.** Every cross-node hop in Build carries an
  in-process hop test in the memql#4352 pattern; a live-cluster gate that
  skips by default guards nothing.

## Out of scope for the whole program

A GitHub App installation, non-GitHub hosts, any mobile schema or store
submission, a bundle upload from the browser, any portal work, and archive
purge for deployables. Each is recorded here so it is a decision rather than
a drift.
