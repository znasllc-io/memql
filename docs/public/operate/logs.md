---
title: Logs
audience: public
status: stable
area: operate
sinceVersion: 0.21.0
owner: znas
---

# Logs

One place to read what every node wrote, instead of a browser console, then a
pod, then a database. Every node type persists its log lines to the cluster's
database beside the observability rows, keeps them for thirty days, and
archives each day to blob storage before it goes. The MemQL OS front end
writes its own errors into the same store. The Logs app in MemQL OS shows all
of it with real filtering, and every app in the OS carries a Logs section
showing its own slice. Design record:
[the Logs design](../../superpowers/specs/2026-09-03-logs-design.md), Epic 2
of [the Deployables program](../../superpowers/specs/2026-09-02-deployables-program-design.md).

## What is persisted, from where

| Source | How it gets there | What it carries |
|---|---|---|
| Every node type (identity, bff, cognition, agent, planner, voice, workbench, mcp, edge) | An `slog.Handler` installed beside the console handler by `core/logger`, forwarding to a bounded, non-blocking sink in `component/logstore` that batches into the `log_line` hypertable | time, node type, node id, level, component, message, every other attribute (already through the console redactor), and `subject` |
| The MemQL OS front end | The shell's capture module: window errors, unhandled rejections, `console.error` and `console.warn`, batched and sent over the connection the OS already holds through `logsRecordClient` | the app id and section of the focused window, the page, a per-tab session id, and the user, stamped server-side |
| The pipelines that name a subject | `logger.Subject(concept, id)` on the line | `subject` and `subjectConcept`: the deployment, site, plan or user the line is about |

Two things are deliberately NOT persisted:

- **The portal.** It is deprecated and will be removed. It is not
  instrumented, so it has no component name and appears in no facet.
- **A hosted site's browser console.** MemQL has no anonymous write, the
  edge's `apiProxy` carries no site id, and a new HTTP route needs the
  owner's explicit approval under the gRPC-first policy. The recommended
  shape is recorded in the design record's section G as an owner decision;
  until it is taken, a deployable's own console stays in the visitor's
  browser.

The row is `v1:observability:logLine`, declared in
`dsl/observability/concepts.memql`. It is a declaration of the table's shape,
the way `invocation` declares `code_invocation`: rows never enter the graph,
never broadcast, and are read only through the builtins below.

## Levels, the cap, and what a drop looks like

- `info` and above persist by default on every node type. `debug` is opt-in
  per node with `MEMQL_LOGS_LEVEL=debug`; `off` disables the store on that
  node. The console handler keeps printing exactly what it printed before.
- Each node has a per-second cap, `MEMQL_LOGS_MAX_LINES_PER_SECOND`
  (default 2000), and a queue of 4096 lines. A caller never waits on the
  database: when the queue is full, the cap is hit, or the insert fails,
  the line is dropped and counted on `memql_logs_dropped_total{reason}`
  (`queue`, `rate`, `level`, `db`). `memql_logs_written_total` counts what
  landed.
- **A gap announces itself.** When a node dropped lines in the last minute
  it writes ONE warning line saying how many and why, and that line is
  stored, so the Logs app shows its own holes. The store's own failures go
  to the console only (component `logs.store`, which the sink never stores),
  which is the one place a broken store can still be read.
- Attributes reach the store after `core/logger`'s redactor, so a `token`,
  `secret` or `authorization` attribute is `<redacted>` in the row exactly
  as on the console. `payload` and `body` are redacted whatever they hold.

## Reading

Every read is a builtin declared in `dsl/observability/builtins.memql`, admin
and above (owner, developer, admin under the one ladder), enforced in the
handler:

| Builtin | What it answers |
|---|---|
| `logsSearch` | lines inside `[windowStart, windowEnd)`, newest first, narrowed by node type, node, component, app, level, subject, session, user and text; keyset-paged with `beforeAt` + `beforeId` |
| `logsTail` | the live tail: lines newer than `afterAt` + `afterId`, oldest first; with no cursor the newest `limit` lines, the baseline a stream starts from |
| `logsSources` | what logged in a window: one row per component, per node and per OS app with counts, which is where the facet lists come from |
| `logsStatus` | what this cluster keeps: retention, archive, level, cap, written and dropped counters, oldest and newest line |
| `logsArchiveList` | the archived days |

`apps` and `subjectConcepts` form ONE scope predicate, ORed: an app's slice is
"lines tagged with me, or about the things I own". Every other facet ANDs.

From any client that executes MemQL calls, the same reads are one line:

```memql
builtin logsSearch(windowStart: "2026-09-02T00:00:00Z", windowEnd: "2026-09-03T00:00:00Z", levels: ["error"], components: ["packages.pipeline"])
```

### In MemQL OS

- **The Logs app** (admin and above): **Stream** follows the last fifteen
  minutes with level, source, node, app, subject and text facets; **Search**
  takes a window from fifteen minutes to thirty days or a custom range and
  pages older; **Settings** holds the reader's preferences, what the cluster
  keeps, and the archived days. The stream polls `logsTail` every two seconds
  while its window is visible; nothing broadcasts, and no arrival cue rings,
  because on a stream every line is news.
- **Every app's Logs section** (admin and above, absent from the nav for
  everyone else) shows that app's slice through the same tail: lines
  tagged with the app id, plus lines whose subject is one of the concepts
  the app owns. Deployables' site and deployment pages carry a "Logs"
  action that opens the Logs app already narrowed to that subject.

## Retention and the archive

The `logsRetentionSweep` automation runs nightly at 03:20 UTC on the cron
leader (a Postgres advisory lock, so exactly one replica runs it) and calls
`builtin logsSweep`. For every UTC day older than `MEMQL_LOGS_RETENTION_DAYS`
(default 30), at most sixty per run:

1. each node type's rows for the day are written as one object,
   `logs/<YYYY-MM-DD>/<nodeType>.ndjson.gz`, into
   `MEMQL_LOGS_ARCHIVE_CONTAINER` (default: the cluster's
   `MEMQL_AZURE_BLOB_CONTAINER`) -- one JSON object per line, the concept's
   field names, gzip;
2. only once every node type of that day is uploaded, that day's rows are
   deleted.

**No archive, no delete.** A cluster with no object storage configured keeps
its lines, and the sweep says so in its own log line every night (search for
component `logs`, level `warn`). Set a container, and the next run catches
up. The run's one INFO line carries days archived, rows archived and rows
deleted; `memql_logs_archived_total` and `memql_logs_deleted_total` carry the
same figures for alerting. An owner may run the sweep by hand with
`builtin logsSweep()`; a second sweep while one runs answers `skipped`.

### Bringing a day back

Owner only. From the Logs app: Settings, Archived days, "Bring back" beside
the day. From any client:

```memql
builtin logsArchiveRestore(day: "2026-08-01")
```

with an optional `nodeType` to restore one node type only. The restore
downloads the day's objects, gunzips them, and inserts the rows with `ON
CONFLICT DO NOTHING` on `(occurredAt, id)`, so running it twice restores
nothing twice; the reply is `{ restored, skipped, objects }`. A restored day
is older than the retention boundary by definition and is swept again at the
next nightly run: read it now.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `MEMQL_LOGS_LEVEL` | `info` | The store's floor on this node: `debug`, `info`, `warn`, `error`, or `off`. The console level is untouched. |
| `MEMQL_LOGS_MAX_LINES_PER_SECOND` | `2000` | Per-node cap (10..100000); beyond it lines are dropped and counted as `rate`. |
| `MEMQL_LOGS_RETENTION_DAYS` | `30` | Days kept before the nightly sweep archives and deletes (1..365). |
| `MEMQL_LOGS_ARCHIVE_CONTAINER` | `MEMQL_AZURE_BLOB_CONTAINER` | The blob container the archive lands in. Empty means no archive, and no delete. |

All four are registered in `scripts/secrets/manifest.yaml` under
`component: observability`; the full registry story is in
[Environment Variables](env-vars.md).

## Multi-node

Every replica writes its own lines under its own `node`; nothing crosses a
node boundary. An OS line lands on whichever bff holds the browser's stream
and is stamped there. The sweep is leader-only. The per-session bucket
behind `logsRecordClient` is per replica: a browser that reconnects to a
sibling starts a fresh bucket, which is why the cap is a courtesy against a
storm rather than an authority.

## When something looks wrong

- **A node's lines are missing.** Check its `MEMQL_LOGS_LEVEL` is not `off`,
  then its boot log: the sink falls back to a no-op with a warning when the
  database was not ready after ten tries, and the Logs app shows that node
  from its next restart.
- **`memql_logs_dropped_total{reason="queue"}` climbs.** The database is
  slower than the node is loud. Raise nothing; find what is logging at that
  rate (`logsSources` over the last few minutes names it).
- **The sweep refuses every night.** It has no container. Set
  `MEMQL_LOGS_ARCHIVE_CONTAINER` or the cluster's blob container; the
  refusal line names both.
- **A restored day vanished.** It was swept again the next night, as
  designed; restore it when you are ready to read it.
