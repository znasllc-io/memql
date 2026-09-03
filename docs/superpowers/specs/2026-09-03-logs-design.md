# Logs -- Design

- **Date:** 2026-09-03
- **Status:** approved for build. The owner asked for the epic end to end in
  one pass ("get it done, let me know when it is completely done"), so this
  round ran as a recommendation pass rather than a question-by-question
  brainstorm: every fork below records the choice, the reason, and what was
  rejected. Two forks are the owner's alone under standing policy and are
  marked **OWNER DECISION**; both are recorded with the recommended shape so
  each is a one-line approval rather than a design round.
- **Scope:** `dsl/observability/` (the concept, the builtins, the sweep
  automation), `core/logger` (the fan-out handler and the `subject`
  helper), a new `component/logstore` (the batching sink, the reader, the
  sweep, the restore, the client write), `component/metrics` (four
  counters), `component/database` (one migration), `component/packages`
  (the first `subject` stamps), `clients/os/` (capture, the per-window error
  boundary, the `logsSection` convention, `AppLogsSection`, the Logs app),
  the operator docs, the env registry.
- **The wave this belongs to:** Epic 2 of
  [the Deployables program](2026-09-02-deployables-program-design.md)
  (P8, P9). Build writes to this store from day one; Run's traffic and
  health ride it.

## Why

Following one fault today means a browser console, then a pod's stdout,
then a database. The program record fixes what this epic must deliver: every
node's log lines persisted beside the observability rows, thirty days of
retention with an archive before the sweep, one app over everything with
real filtering, a Logs section on every app for admins and above, the OS
front end's own errors always, a hosted site's console where possible, and
the portal skipped. This record settles what that record left open and is
the authority for the remaining tasks of the epic.

## What was verified before any of this was decided

- **The "code_invocation tradition" exists on the write side only.**
  `component/observe.TimescaleSink` batches into its own hypertable through
  bun, non-blocking and drop-on-full; but `query codeMetric
  codeMetricsInWindow` has no Go provider behind it and nothing maps a
  concept to a table, so a DSL `query` over a dedicated hypertable returns
  nothing. The Go-served read that does exist is the `@sdk` builtin
  (`dataOrigins`, `modelCatalog`, the fleet catalog): a builtin whose
  executor answers from Go and returns synthetic nodes. That is the read
  shape here.
- **`concept log` is taken.** `dsl/data/concepts.memql` declares `log`, the
  validation audit trail for `v1:data:record`. Construct names are unique
  across the whole tree.
- **`core/logger` has no fan-out.** The chain is `redactingHandler ->
  slog.JSONHandler -> stdout`. Node id and node type never reach the logger;
  `slog.SetDefault` is never called, so every `slog.Default()` fallback in
  the tree bypasses `logger.New`.
- **The `payload` and `body` attribute keys are always redacted** by
  `core/logger/redact.go`, whatever they hold.
- **No engine rate limiter exists for a client-callable construct**, and
  `@rateLimit` is a load-time rejection on a query or mutation. The only
  working token bucket is `component/identity/abuse.IPRateLimiter`, in
  memory and per replica.
- **`AccessContext` carries no session id** and the DSL actor envelope is a
  closed set. The OS reads `sessionId` off the wire and drops it.
- **Scheduled automations run on the cron leader only**
  (`component/automations.CronLeader`, a Postgres advisory lock), so a
  scheduled sweep is single-replica by construction.
- **The edge cannot inject a script.** It serves bundle bytes verbatim with
  no HTML rewrite stage and a static `script-src 'self'` with no nonce; a
  gate refuses the literal `portal` anywhere in its serving path. The
  `apiProxy` mount reaches `/memql/query` and `/memql/ws` on the bff but
  attaches no site id and no credential, and **MemQL refuses every
  anonymous write** before the concept is resolved (memql#4541, D4).
- **Nothing in the OS is virtualized**, no OS surface has a time facet, and
  the OS calls `console.*` nowhere. A high-volume concept is exactly the
  shape `clients/os/README.md` excludes from broadcast.
- **The Compose rail has not landed.** The Deployables app on `main` has
  `SiteDetail` and `PackageDetail`; the Build and Live stops are a design
  record. The deep link attaches to what exists and moves onto the stops
  when Compose lands.

## Locked decisions

| # | Decision | Choice |
|---|---|---|
| L1 | The concept | **`v1:observability:logLine`**, a dedicated `log_line` hypertable, never a graph row. Fields in A. `log` was taken; `logLine` says what one row is |
| L2 | Reads and the write | **`@sdk` builtins served from Go** (`logsSearch`, `logsTail`, `logsSources`, `logsStatus`, `logsArchiveList`, `logsArchiveRestore`, `logsRecordClient`), not `query` / `mutate` constructs: a DSL query cannot reach a dedicated table, and a bare mutation has no seam for a cap or a rate limit. Rejected: rows in `MemoryNodes` (every DSL feature for free, but it contradicts the fixed point and puts the loudest table in the graph) |
| L3 | Access | **Admin and above on every read** (owner, developer, admin under the one ladder), **owner** on `logsSweep` and `logsArchiveRestore`, enforced IN THE GO HANDLER -- the `integrationConfigure` precedent -- because a builtin's annotation set is closed and carries no `@requiresRank`. `logsRecordClient` admits any signed-in principal and nothing anonymous. The concept declares no row tier because no row of it ever passes graph admission. Follow-up worth its own small change: admitting `@requiresRank` on a builtin, so the surface half is declared where the OS manifest can mirror it |
| L4 | Levels | **`info` and above persist by default on every node type**; `debug` is opt-in per node through `MEMQL_LOGS_LEVEL`. The console keeps everything it kept. Rejected: `codeProfile` for debug (it is per-FQN for the observe runtime, a different question) |
| L5 | The handler's bounds | Queue **4096** lines, batch **256**, flush **1s**, a per-node token bucket of **`MEMQL_LOGS_MAX_LINES_PER_SECOND`** (default 2000). Overflow is dropped and counted by reason; a caller never waits on the database |
| L6 | Broadcast | **No.** Rows never enter the graph, so no `graph.node.*` event exists to route. The Stream polls `logsTail` incrementally every two seconds while its window is visible; the arrival cue is not used |
| L7 | Retention and the archive | **Thirty days by `MEMQL_LOGS_RETENTION_DAYS`**, a nightly automation (`0 20 3 * * *` UTC) on the cron leader: archive each expired UTC day per node type as **`logs/<YYYY-MM-DD>/<nodeType>.ndjson.gz`** in `MEMQL_LOGS_ARCHIVE_CONTAINER` (default: the cluster's blob container), then delete that day. **No archive, no delete**: a cluster without object storage keeps its lines and the sweep says so in its own log line, every night |
| L8 | Restore | **`logsArchiveRestore(day, nodeType?)`**, owner only, idempotent on `(occurred_at, id)`, offered from the Logs app's Settings and callable from the cockpit. A restored day is swept again at the next run, and the response says so |
| L9 | The OS write | **`logsRecordClient(session, lines[])`**: per call at most 50 lines, 4 KiB per message, 8 KiB of attributes per line, refused whole with a sentence; a per-(user, session) bucket of 120 lines with a refill of two per second, refused with `rate_limited`. `userId` is server-stamped from the actor; `session` is a client-minted per-tab id, a correlation key and never an authority |
| L10 | Hosted-site ingest | **None in v1 -- OWNER DECISION recorded, not taken.** The apiProxy route cannot carry a signed-out visitor's lines (no anonymous write, no site id), a new HTTP route needs the owner's explicit approval under the gRPC-first policy, and the edge cannot inject a collector today. The recommended shape is in G so approval is one line |
| L11 | The per-app section | **`logsSection` on `OsAppManifest`, required like `settingsSection`**, naming a section gated `{ min: "admin" }`; `AppLogsSection` reads the app's slice: lines tagged with the app id **or** whose subject concept is one the app owns |
| L12 | The subject seam | **Two columns, `subject` (bare id) and `subjectConcept`**, stamped by `logger.Subject(concept, id)`. An app's slice is a set of concept ids, which the client already holds as generated constants; nothing composes or parses a canonical id |
| L13 | The Logs app | **Stream, Search, Settings.** Sources are a facet; the portal is absent by construction (it is not instrumented and has no component name). A windowed list carries ten thousand lines |
| L14 | The portal | **Not instrumented, not captured, not a facet.** Its deprecation is the reason and nothing in this epic names it |

## A. The store

### The concept (`dsl/observability/concepts.memql`)

```memql
@displayCard(primary="message", secondary="component", tertiary="node", status="level")
concept logLine {
  occurredAt      datetime!   // the hypertable time column
  nodeType        enum("identity","bff","cognition","agent","planner","voice","workbench","mcp","edge","os")!
  node            string      // MEMQL_NODE_ID; blank for an OS line
  level           enum("debug","info","warn","error")!
  component       string!     // the logger's component attribute; os.<app> for an OS line
  app             string      // the OS app id; blank for an engine line
  message         string!
  attributes      object      // every other attribute, already redacted
  subject         string      // bare id of what the line is about
  subjectConcept  string      // its concept id
  session         string      // the OS tab session; blank for an engine line
  userId          string      // server-stamped for an OS line; blank otherwise
}
```

`v1:observability:logLine` is the row's concept id on the wire, the id is a
short id minted at write, and `createdAt` is `occurredAt`. The concept is a
declaration of the table's shape (the `invocation` precedent); nothing maps
a field to a column.

### The table (`component/database/memory-nodes/migrations/20260903000000_log_line_hypertable`)

`log_line(occurred_at timestamptz, id text, node_type text, node text,
level text, component text, app text, message text, attributes jsonb,
subject text, subject_concept text, session text, user_id text)`, primary
key `(occurred_at, id)`. A hypertable on `occurred_at` with one-day chunks
where TimescaleDB is present (the same `DO $$ IF EXISTS timescaledb` guard
as `code_invocation`, so a plain Postgres box still migrates); compressed
after one day, segmented by `node_type, component`. **No Timescale retention
policy**: the sweep owns retention, because the archive must come first.
Indexes: `(subject, occurred_at DESC)` where `subject <> ''`, `(component,
occurred_at DESC)`, `(app, occurred_at DESC)` where `app <> ''`, `(level,
occurred_at DESC)` where `level in ('warn','error')`. Text search is
`ILIKE` over `message` inside the window; the window bounds the scan.

## B. The handler (`core/logger`)

- `logger.New` builds `redactingHandler -> fanout(JSONHandler, storeHandler)`.
  The store handler sits INSIDE the redactor, so every stored attribute is
  what the console would have printed.
- The store handler forwards a `logger.Line` to the process's one
  registered `logger.Sink`. Before boot registers a sink, lines go to a ring
  of 2048; `SetSink` drains it, so boot lines are kept. A binary that never
  registers a sink drops the ring's overflow and nothing else changes.
- `app/` calls `slog.SetDefault` on the app logger, so the tree's
  `slog.Default()` fallbacks reach the store too.
- Node identity is resolved once by the sink from `MEMQL_NODE_ID` (hostname
  fallback) and `envregistry.ResolveNodeType()`; the logger stays ignorant
  of it.
- The sink (`component/logstore.Sink`) is installed in `app/database.go`
  beside the observe sink on every node type; it is a bun batch insert with
  the bounds in L5; `Write` is a non-blocking channel send. Drops are
  counted on `memql_logs_dropped_total{reason=queue|rate|level|db}`; writes
  on `memql_logs_written_total`. When drops occurred in the last minute the
  sink logs ONE warn line saying how many and why -- which is itself
  stored, so the Logs app shows its own gaps.
- **Recursion guard:** a line whose `component` starts with `logs.store` is
  never stored. The store's own failures go to the console only, which is
  where a broken store can still be read.
- `logger.Subject(concept, id string) slog.Attr` writes `subject` and
  `subjectConcept`. It lives in `core/logger` rather than `component/observe`
  because the handler that reads it is in `core/logger` and `observe` is a
  leaf module the core cannot import.
- The first consumer is `component/packages/pipeline.go`: every line of a
  run carries `logger.Subject("v1:platform:packageDeployment",
  deploymentId)`, and the two lines that stamped nothing now do.

## C. Levels, sampling and the metric

`MEMQL_LOGS_LEVEL` (default `info`; `off` disables the store on that node)
is the store's floor; the console handler's level is untouched. Above the
floor there is no sampling: a line that reaches the sink is stored unless the
bucket in L5 is empty, and then it is counted. The bucket is per node and
per second, so one chatty replica cannot starve the others.

## D. Reads

All `@sdk`, executor `integration.logs.<name>`, admin-floored in the
handler (L3), `PreserveOrder`, rows returned as `v1:observability:logLine`
nodes.

- **`logsSearch`** -- `windowStart!`, `windowEnd!`, the facet set
  (`nodeTypes[]`, `nodes[]`, `components[]`, `apps[]`, `levels[]`,
  `subject`, `subjectConcept`, `subjectConcepts[]`, `session`, `userId`,
  `text`), `limit` (default 200, cap 500), keyset `beforeAt` + `beforeId`.
  Newest first. `apps[]` and `subjectConcepts[]` together form ONE scope
  predicate ORed (an app's slice is "tagged with me or about my things");
  every other facet ANDs.
- **`logsTail`** -- the same facets, keyset `afterAt` + `afterId`, `limit`
  (default 200, cap 500). Oldest first so a client appends. With no cursor
  it answers the newest `limit` lines in ascending order: the baseline.
- **`logsSources`** -- `windowStart!`, `windowEnd!`; one row per distinct
  `component`, per `(nodeType, node)` and per `app` seen in the window with
  a count. The facet lists come from here, so a value that never logged is
  never offered.
- **`logsStatus`** -- what this cluster keeps: retention days, the store
  level and rate on the answering node, whether an archive is configured
  and where, the written and dropped counters, the oldest and newest
  `occurredAt`, the row count estimate.
- **`logsArchiveList`** -- the archived days and node types with sizes.
- **`logsArchiveRestore(day!, nodeType?)`** -- owner only (L8).

## E. The OS write

`logsRecordClient(session!, lines![])` where a line is `{ at, level!,
app, component, message!, attributes, subject, subjectConcept }`. The
handler refuses an anonymous or connector actor, validates `session`
against `^[A-Za-z0-9_-]{4,64}$` and `app` against `^[a-z][a-z0-9-]{0,39}$`,
takes `component` as given or derives `os.<app>` (or `os.shell`), keeps a
client `at` within five minutes of the node clock and replaces one outside
it, stamps `nodeType=os`, `node=""`, `userId=actor.userId`, and writes
through the same sink as the engine's lines. The caps and the bucket are
L9. The reply is `{ accepted, dropped, reason }`.

## F. Retention and the archive

- The automation `logsRetentionSweep` fires nightly on the cron leader and
  calls `builtin logsSweep ()`. A manual `logsSweep` from an owner is the
  same code; a `pg_try_advisory_lock` keeps two sweeps from overlapping and
  the second answers `skipped`.
- The boundary is UTC midnight `retentionDays` ago. Days are taken from the
  oldest row up to the boundary, at most sixty per run. For each day, for
  each node type present: stream the rows in `(occurred_at, id)` order into
  gzip NDJSON (one row per line, the concept's field names, `attributes`
  inline), upload, and only when every node type of that day is uploaded
  delete that day in batches of 5000.
- No configured container means no delete (L7). The run's one INFO line
  carries days archived, rows archived, rows deleted, or the refusal.
- Metrics: `memql_logs_archived_total`, `memql_logs_deleted_total`; both
  zeroed at init so a flat series reads as zero rather than absent.
- Env: `MEMQL_LOGS_RETENTION_DAYS` (30, clamped 1..365),
  `MEMQL_LOGS_ARCHIVE_CONTAINER` (defaults to `MEMQL_AZURE_BLOB_CONTAINER`),
  `MEMQL_LOGS_LEVEL`, `MEMQL_LOGS_MAX_LINES_PER_SECOND` (2000, clamped
  10..100000). All registered `component: observability`, `optional: true`.
- Restore is E of L8: list `logs/<day>/`, download, gunzip, insert with
  `ON CONFLICT DO NOTHING` in batches, answer `{ restored, skipped }`.

## G. Hosted-site ingest -- OWNER DECISION, recorded

**v1 ships no hosted-site console capture.** Three facts decide it:

1. There is no anonymous write in MemQL by design (memql#4541, D4), and the
   `apiProxy` mount carries neither a credential nor a site id, so a
   signed-out visitor's console cannot reach the store that way.
2. A new HTTP route is an owner approval under the gRPC-first policy, and
   this round could not obtain one.
3. The edge serves bundle bytes verbatim; an injected collector needs an
   HTML rewrite stage and a CSP source that do not exist.

**The recommended shape, for a one-line approval:** `POST /__memql/console`
on the site's OWN origin, served by the edge (the `runtime-config.json`
precedent for a path carved out beneath `/`), so `connect-src 'self'` admits
it with no CSP change; the site id comes from the edge's own Host
resolution and never from the page; the edge writes the lines through its
sink under `nodeType=edge`, `component=site.console`, `subject=<siteId>`,
`subjectConcept=v1:platform:site`, with a per-site bucket and the L9 caps;
the collector is a bundle-served `/__memql/console.js` the edge injects
before `</head>` of `index.html` only (ETag recomputed over the rewritten
bytes); `systemOwned` rows are never injected, expressed as a row field
rather than a literal. It would be the fourth self-authenticated write in
the repo and needs the `*Paths()` declaration, the front-door
classification and the CLAUDE.md row citing the approval.

## H. The OS

### Capture (`clients/os/src/logs/capture.ts`)

`window.onerror`, `unhandledrejection`, `console.error` and `console.warn`
(info is not captured: it is noise at the rate the shell would produce it).
A module-level queue, never React state: flush every two seconds or at
twenty lines, at most fifty per call, queue cap two hundred with the oldest
dropped and counted; identical lines within one flush collapse into one
with `attributes.repeat`. Sent through the connection the OS already holds
as an unawaited `logsRecordClient` whose rejection is swallowed, so the
capture path can never re-enter itself; a `pagehide` flush. Each line
carries the focused window's app id and section and the page path, read
through a context the Shell installs. The session id is `os-<shortId>`,
minted once per tab and kept in `sessionStorage`.

**A per-window error boundary** (`chrome/WindowErrorBoundary`) wraps every
app body: a render error in one app no longer blanks the desk; the window
shows a `Notice` with the error's sentence and a "Reload app" action, and the
boundary reports the line with the app id and section exactly, which is what
makes "the right app id" a property rather than a hope.

### The convention

`OsAppManifest.logsSection: string`, required. Every app's `*_SECTIONS`
carries `{ id: "logs", name: "Logs", roles: { min: "admin" } }` before its
settings section; `sectionsForRole` hides it below admin and the nav never
shows it. The contract test mirrors `settingsContract`: the named section
exists and is admin-floored (the Logs app names its Stream, floored by the
app). `AppLogsSection` lives in `src/logs/` and is imported by every app,
the way `apps/accounts/tie.tsx` is.

### `AppLogsSection`

Head("Logs", meta = the window and the count), Refine with level, window and
text, chips for each; the windowed list; "Following" with a "N new lines --
jump to latest" affordance when the reader has scrolled up. Reads `logsTail`
scoped to `apps: [id]` plus the app's `subjectConcepts`. Empty state:
"Nothing recorded for this app in the last hour." Filtered-to-empty: "No
lines match." A subject in a row narrows on click; a `subject` intent lands
narrowed.

### The Logs app (`clients/os/src/apps/logs/`)

- **Stream**: the last fifteen minutes and following. Facets: level floor,
  source (component), node, app, subject text, text.
- **Search**: a window (presets from fifteen minutes to thirty days plus
  From / To) and every facet, paged older by keyset. A `{ subject,
  subjectConcept }` intent lands here narrowed with a copyable chip.
- **Settings**: open on (Stream / Search), density, the level floor shown by
  default, the stream window; a "This cluster" panel from `logsStatus`
  (retention, archive, drops); "Archived days" from `logsArchiveList` with
  "Bring back" for an owner, and a sentence for everyone else on why the
  action is absent.
- Rows: a two-pixel left rule and the level word for warn and error (colour
  is never the only carrier), elapsed time with the instant on `title`,
  the component, the message in the mono voice, attributes inline as
  `key=value`; a selected row opens a Panel below the list with the full
  line and a `CopyValue` for the subject.
- The windowed list (`src/logs/WindowedList.tsx`) renders only the rows in
  view plus an overscan, at a fixed row height per density, and is the
  first use; it is promoted to `kit/` on the second.
- Density reuses `.os-app-stack[data-density]`; level colours are
  `--os-error` and `--os-warn` only, never the accent.

### Deep links

Deployables' `SiteDetail` and `PackageDetail` carry a quiet "Logs" action
that opens the Logs app on Search with `{ subject, subjectConcept }`. When
the Compose rail lands, the Build and Live stops carry the same action.

## I. Multi-node

Every replica writes its own lines with its own `node`; nothing crosses a
node. The OS write lands on whichever bff holds the stream and is stamped
there. The sweep runs on the cron leader and takes an advisory lock. The
client bucket is per replica, so a browser reconnecting to a sibling starts
a fresh bucket; the cap is a courtesy against a storm, not an authority.

## J. Testing

- db-gated: write through the handler, read back through `logsSearch` with
  each facet, `logsTail` from a cursor, `logsSources`; two sinks with two
  node ids in one process, both present in a read; the sweep archives before
  it deletes against an in-memory archiver and is a no-op on an empty store;
  the restore reads an archive back and is idempotent; `logsRecordClient`
  refuses an oversized payload and an over-rate session, stamps the user,
  and refuses an anonymous actor; a viewer calling `logsSearch` is refused.
- pure: the handler never blocks against a stalled insert (the caller
  returns and the drop counter moves); the ring keeps boot lines; the
  recursion guard; the level floor; the redaction of a `token` attribute
  before storage; `logger.Subject` lands in both columns.
- OS: capture batches and never throws; the boundary reports with the right
  app id; the manifest contract; the role pins (admin, developer, owner see
  the section; writer and reader do not); each facet narrows; a subject
  intent lands narrowed; a ten-thousand-line fixture renders a bounded
  number of rows; the portal never imports the capture module.
- Gates that move: `shippedAutomationCount` (46 -> 47), the embed count for
  `component/database` and `dsl`, `make sdk-gen`, `make env-registry-sync`,
  `make arch-model`.

## K. Out of scope

Alerting, log-based metrics beyond `logsSources`' counts, shipping to an
external system, any portal capture, retention past a year, per-app
retention, the hosted-site collector (G), and a session id on the actor
envelope.
