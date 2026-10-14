---
title: Operational record retention
audience: internal
status: stable
area: ops
sinceVersion: 0.23.2
owner: znas
---

# Operational record retention

`workJournalRetentionSweep` runs at 03:40 UTC under the cron leader and the
compiled maintenance principal. It performs verified archival and physical
retirement for the policies documented in [environment variables](../../public/operate/env-vars.md#operational-record-retention).
The earlier audit/safety jobs remain count observations; this central job is now
the actual archive/delete path. Worker soft deletion remains a UI lifecycle step;
physical retention measures from the latest version, including that tombstone.

## Declared concept retention

`@retention(ttl="30d", keepLatest=true, archive="required")` opts a graph
concept into the shared version-retirement sweep. The required archive uses
`MEMQL_AZURE_BLOB_CONTAINER`; `archive="none"` explicitly makes the selected
history disposable. Unmarked concepts keep their current policies.

The hourly platform automation invokes `integration.retention.sweep` under
cluster-owner authority. Each page scans at most 100 versions, has a 20-second
deadline and a 32 MiB archive budget, and compares downloaded bytes before an
exact-key delete. Each deletion also compares the archived row's contents and
rechecks keep-latest. A row changed since selection stays. Oversized individual
versions stay and are counted; smaller rows continue. Public and secret row
tables both use the policy, and physical deletions invalidate cached reads across
the cluster. Row admission failures preserve the batch.

`concept_retention_progress` stores the cursor and a generation-fenced, expiring
claim. A replacement never needs the previous process's memory. A complete walk
resets its cursor so previously protected or backdated rows get another pass;
a policy change has its own cursor. Dry runs change neither rows nor progress.
Archive objects under `retention/concepts/` contain gzip NDJSON with the source
`table` and the complete stored row, including schema and provenance.

The existing log sweep reads logLine's retention declaration and verifies each
archive before dropping a day's chunks. It continues to use
`MEMQL_LOGS_ARCHIVE_CONTAINER` (falling back to `MEMQL_AZURE_BLOB_CONTAINER`).

## Current liveness observations

Worker registration heartbeats, cluster-node health and mesh observations, and
module-readiness reports have one current row per record in `memory_liveness`.
Repeated observations replace that row instead of appending history. The normal
engine read path merges this table with `MemoryNodes` before filtering, sorting,
authorization and pagination. Its `createdAt` remains the observation version,
so live feeds and reads from another replica order updates consistently.

The first record and changes outside the closed observation-field list still
append to `MemoryNodes`. A business change clears its preceding observation in
the same transaction. Every engine write to these concepts carries a version
fence: a concurrent change rejects the stale decision rather than overwriting
it. Normal ownership checks, schema validation and cluster event publication
remain in the mutation path. Retiring all base versions also removes the current
observation. The earlier bounded status-history sweep continues to clear legacy
versions and keeps the latest base record.

Historical reads retain business versions and, when within the requested time,
the current observation. Superseded heartbeat samples are deliberately absent;
backups retain the durable business records, and fresh heartbeats rebuild current
liveness after a restore. During an upgrade, `memory_liveness_readers` must show
recent support from every non-stopped node before `memory_liveness_storage`
enables the switch. Offline nodes still block it. Downgrading after activation
requires a complete backup restore; an old binary cannot read the observation
store.

## Steady writers

Custom-domain `lastCheckedAt` and mirror `lastReconcileAt` / `lastAttemptAt`
use the same current-observation store. Rechecking a healthy domain or a mirror
with no drift advances its freshness and scheduling watermark without another
business version. Changes to status, failures, drift or configuration still append
history. Identical repeated results do nothing. This extension has its own
`memory_reconcile_observation_readers` handshake, so an older reader that knows
only the first three liveness concepts cannot enable it prematurely.

Observation-only writes still refresh live readers and invalidate caches across
replicas, but do not release a held scheduled work journal. Unchanged inbound
retries also suppress graph events: the comparison runs after validation,
create-only fields and read-merge, under the row's transaction fence. A retry
cannot reset a processed delivery's state or its original arrival metadata.

Reputation flushes acknowledge successfully saved windows and skip unchanged
ones; failed saves remain eligible for retry. The magic-link sweep excludes
already consumed requests. Package polling writes only a changed upstream head
or availability, and seed materialization compares against a fresh read rather
than a replica's cached result. These guards keep normal idle polling from
creating business history.

## Shared row schemas

`MemoryNodes` and `SecretMemoryNodes` store an immutable schema reference and
its original `$id`. The schema body lives once in `memory_node_schemas`, keyed
by its content fingerprint. Different historical definitions remain distinct;
reads resolve the version a row actually used, including deletion schemas.
Insert triggers also cover direct writers and backup restores. A missing
reference is refused rather than returned as a partial schema.

During a rolling upgrade, normalization waits until every registered node that
has not stopped has recently advertised schema-reference support in
`memory_schema_readers`. An offline node still blocks: it may be serving reads.
Once the cluster is ready, `memory_schema_storage.references_enabled` is set and
remains set. An orphan node that never records its stopped state must be retired
through the existing topology lifecycle before conversion can start.

Public graph reads, repository reads, backups and retention archives expand
references to full schemas. Backups and archives therefore remain self-contained
and keep their existing format. The catalog is not garbage-collected.

Existing inline schemas are converted after startup, in batches of at most
500 rows. A shared cursor row is locked in the same transaction as each batch;
other replicas skip a held batch, and a replacement resumes committed progress.
Each table is eligible at most once every five seconds, each batch has a
five-second deadline, and failures back off for one minute. Payloads, provenance,
IDs and version timestamps are unchanged. The conversion runs independently
of node liveness and never holds startup for a history rewrite. Schema-only
updates do not enqueue work-run recovery invalidations.

Inspect conversion progress with:

```sql
SELECT table_name, cursor_id, cursor_at, completed, next_at
FROM memory_schema_compaction;
```

Conversion makes old row storage reusable; it does not promise to shrink the
volume immediately. New writes stop copying schema bodies after the reader
readiness gate enables normalization. Older and converted rows can coexist during
conversion. Downgrading to an engine without schema-reference support requires
a full-schema backup restore; the down migration refuses to discard the catalog.

## Incident context

The September 2026 incident audit found 103,324 runs across about 17 days:
98,838 scheduled, 2,481 webhook-triggered, 1,667 startup, 323 shutdown, four manual
and two system goal runs. Approximately 5,500–7,800 runs/day reflected the
configured two-minute/five-minute/ten-minute maintenance schedules. Almost every
schedule had no duplicate minute slots. The count was not a large population of
user goals or a single load-test burst.

A separate defect created 5,877 waiting runs: feedback approval creation supplied
strings where object options were required, then ignored the write error and
parked the run on a nonexistent approval. Failed periodic maintenance now ends
as failed, retries on its next schedule, and only proven unowned scheduled waits
with missing approvals are closed by recovery. User goals and real approvals
keep their existing lifecycle.

## Bounded current-run recovery

The first covering-index repair improved warm queries but still traversed every
current run ID. Cold production repeats exceeded the eight-second serving
statement limit even after ordinary vacuum. Recovery now uses a small derived
`work_run_heads` table with a partial index containing only unfinished heads.
It fetches at most 2,000 canonical payloads by exact version keys; terminal
history is not part of the recurring read. Canonical payloads still pass row
admission before leaving the integration.

The database migration installs transactional statement triggers on
`MemoryNodes`. INSERT/UPDATE/DELETE append affected IDs to
`work_run_head_events`; transition tables coalesce many deleted historical
versions into one event per ID per statement. This requires TimescaleDB 2.18+
(the production baseline is 2.29.1). Source writers never acquire projector or
cross-run locks. A single projector claims the state row with `NOWAIT`, resolves
current versions using the existing latest-row index, and deletes only the
exact event sequences it consumed. Sequence allocation is not commit order.

Backfill cursor, heads and consumed events commit together in bounded batches.
Capture begins before backfill, including changes behind its cursor. The
recovery reader refreshes and reads within the same repeatable-read transaction;
a concurrent projector serialization failure retries the whole transaction.
The reader commits up to four 1,000-event/ID batches per two-minute sweep and
returns an explicit catching-up error if a visible backlog remains. It never
reports incomplete heads as a cluster with no waiting work. This provides up to
2,000 dirty events/minute of catch-up capacity; monitor arrival rate against
that capacity as the cluster grows.

Bootstrap is deliberately outside the startup migration. Before accepting an
upgrade with a large history, repeatedly run this bounded transaction on the
primary until `ready` is true (each iteration must commit independently):

```sql
BEGIN ISOLATION LEVEL REPEATABLE READ;
SET LOCAL statement_timeout = '8s';
SET LOCAL lock_timeout = '1s';
SELECT refresh_work_run_heads(1000);
COMMIT;
```

The result exposes `backfillComplete`, processed ID/event counts,
`pendingEventsAtLeast` (capped at 10,001), and `oldestPendingAt`. Investigate
persistent backlog/age growth instead of increasing the serving timeout.
Interrupted bootstrap resumes from its committed cursor. Normal sweeps also
advance bootstrap, but a 104,000-ID history takes many ticks without the
operator catch-up loop. Missing tables/state or disabled capture triggers fail
visibly. TRUNCATE of source, heads or events invalidates and resets the
projection. Direct edits to derived tables, disabled triggers, writes directly
to chunks, and administrative `drop_chunks` are outside normal retention:
repair capture, then `TRUNCATE work_run_heads` and backfill before recovery.
Normal exact-version retention DELETEs are captured transactionally.

Keep parent key statistics and autovacuum current too. They benefit other reads
and the one-time backfill; they no longer determine whether recovery must scan
100,000 completed IDs on every tick.

## Current Fleet reads

The Fleet readiness and stale-connection sweeps also encountered eight-second
cancellations. Their current-key subquery was fast, but PostgreSQL could reorder
an ordinary join into a hash join over historical JSON payloads after inherited
statistics changed. The production plan estimated 264,635 registration history
rows although only 13 current machines were needed.

The shared DSL reader and latest-version recheck now use a lateral exact-version
probe with `LIMIT 1`. This keeps payload access tied to each current key while
preserving outer authorization, filters, ordering, pagination and as-of selection.
A production read-only test returned all 13 machines in about 33 ms. A real
Timescale regression captures and explains the actual Bun SQL with dense machine
history and many unrelated IDs, verifying indexed payload probes rather than a
historical heap scan. Parent statistics and vacuum remain scheduled maintenance;
correctness and bounded payload access do not depend on a planner setting.

## Bounded retention statements

On 2026-10-10 the nightly sweep found 1,045 candidates and then timed out inside
the statement that loads a batch's versions; on 2026-10-11 it timed out reading
the candidates. Nothing was archived or deleted either night, on an instance
whose database volume filled on the second. The request pool cancels a statement
at eight seconds (the driver stops waiting at ten), and no statement of the pass
may come near that however long the history is.

Each policy now asks for the oldest version of its concept before its cutoff,
then reads candidates one UTC day window at a time, oldest first, up to the
cutoff, a hundred per read, and retires each read as one batch. A window with
more than a hundred candidates is read in pages, each starting after the last
row of the one before, so a batch the guard kept is not read again. The
predicates, the archive-before-delete rule and the guards are unchanged.

The pass has four minutes, shared out. When a policy's turn comes it may spend
the time left divided by the policies still to run (itself included), and no
more, checked before each read; time an earlier policy did not use is the later
ones', and a policy with a backlog cannot starve the ones after it. A policy
that runs out of its share with day windows unread carries `budgetExhausted:
true` in the result; the policies after it still run with their shares. The
system-run policy is fifth of eight, so with nothing to do before it, it may
spend a quarter of the four minutes, a minute.

Completed pages and day windows are checkpointed in `work_retention_progress`,
keyed by concept, policy and configured lifetime. A different cron leader resumes
from that point. A generation fence refuses a stale replica's attempt to rewind
it, and dry runs neither consume nor alter progress. A complete traversal resets
the checkpoint: dependency-protected records, denied rows and backdated inserts
must be reconsidered on a later cycle. The `(concept, createdAt, id)` index makes
these day probes cheap even when years of protected history remain. Empty spans
are skipped directly to the next stored version.

Two statements were the ones that timed out. The batch's version load joined the
parents' lookup to the children's with an OR, which the planner answers by
scanning the whole `(concept, id, createdAt)` index; the delete's guard that no
child was left out of the archive had the same shape. Both now find a run's
children with one probe of the `runId` index per run. Measured on a seeded
history of 200,000 runs, 800,000 step versions and 4.05 million rows in all, a
batch of 100 runs loads in about 20 ms at the median (it took 2.2 s, and 2.7 to 8 s
on the first pass over fresh data), and deletes in 46 ms (it took 0.31 s). A pass that
retired 21,832 runs in ten minutes and fifty seconds now retires all 43,019 in a
little over a minute. The measurements and the plans are in the header comment of
`operationalRetention` (`integrations/work/operational_retention.go`).

The goal policy's dependency check now uses a partial expression index on
normalized `goalId`, matching both canonical and historical bare references.
Index migrations use the verified per-chunk builder: interrupted builds and
missing chunk coverage are repaired before success is reported. Journal detail
reads use the same hundred-row day windows and latest-version/active-parent
checks as operational retention. `workRetentionSweep` still runs both halves
independently and returns both errors if they fail.

## Safeguards

- No arbitrary-concept TTL. User-owned runs/goals, active work, business rows and
  unknown concepts are excluded from system-run retirement.
- The archive contains all versions with their intrinsic fields. Content-derived
  object names prevent a later partial batch from overwriting earlier evidence.
- Upload plus bounded read-back equality precedes deletion. No configured
  archive, a read error, oversized batch or failed admission preserves records.
- Each batch is at most 100 candidates, 10,000 historical versions and 32 MiB
  uncompressed. A policy reads its candidates a hundred at a time, one UTC day
  at a time (see [Bounded retention statements](#bounded-retention-statements)),
  and a pass spends at most four minutes of that, shared among the policies. A batch over budget fails
  visibly; investigate that record before adjusting limits. Model-call/observation
  discovery retains its 50,000-candidate cap.
- Deletion uses exact archived `(concept,id,createdAt)` keys and keeps a whole
  batch if a newer revision or unarchived child appeared. Vector cleanup shares
  the transaction and preserves vectors belonging to any remaining version.
- System runs and their step/closed-approval histories share the same archive
  transaction. A retained observation/model call, pending approval, recent child
  or child owned by a user preserves the parent. Longer detail policies win.
- Index migrations build one Timescale chunk per transaction, serialize index
  builders, and verify coverage after interrupted-build repair.

## Owner storage warning

MemQL OS reads the existing `database-health` Kubernetes Event through the
owner-only `clusterStorageStatus` builtin on its ordinary stream. The read
verifies the Event's source and current CloudNativePG cluster UID, so a report
from a replaced installation cannot describe the new one. The shared base grants
only `get` on that named Event and cluster. There is no new monitor or stored
health history in the engine.

The OS uses the monitor's data/WAL capacity warning (over 80%). Owners see one
notice above the dock, opening Cluster → Readiness for usage and measurement
time. Data usage is the monitor's estimate from summed database sizes and
configured capacity; WAL usage comes from the exporter. It is not a filesystem
free-space measurement. The five-minute monitor is read once a minute while the
OS is visible and connected. Reports older than fifteen minutes, future reports,
missing measurements and read failures are unavailable, never zero. A previously
seen warning remains until a fresh complete report clears it. Other roles neither
request nor render the report, and the native capability independently enforces
the owner gate.

## Verify a deployment

1. Confirm the work integration constructed its Azure archive client. A configured
   container without storage credentials produces an explicit startup warning.
2. Invoke `workRetentionSweep(dryRun: true)` as an authenticated cluster owner to
   review candidates and resolved per-concept windows without writes or uploads.
3. After the nightly run, inspect `work: operational retention pass complete`
   (`component=work.retention`) and the run result. Check candidates, archived
   versions, deleted versions, object names and errors. A successful no-op with
   zero expired candidates is expected while records are younger than policy.
   `budgetExhausted` on a policy means it spent its share of the four minutes
   with day windows unread (`work: a retention policy spent its share` says
   which); the next night starts again at the oldest version stored. It is a
   backlog, not a failure, unless it is still set after several nights.
4. Watch run ages and failed maintenance counts. Retention does not repair a
   lifecycle bug or make a slow query efficient; investigate those independently.
5. PostgreSQL reuses vacuumed space; deletion does not normally shrink the cloud
   disk allocation. Monitor live/dead tuples, autovacuum and disk growth.

## Recover evidence

Download the recorded private blob, verify its filename SHA-256 against its gzip
bytes, decompress, and parse one node per line. Preserve the intrinsic fields
when restoring into an isolated database for investigation. Compare IDs and
version keys before any production restoration; do not overwrite live history.
Archive deletion is a separate object-storage policy, not part of this sweep.
