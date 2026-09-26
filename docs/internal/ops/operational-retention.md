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

## Safeguards

- No arbitrary-concept TTL. User-owned runs/goals, active work, business rows and
  unknown concepts are excluded from system-run retirement.
- The archive contains all versions with their intrinsic fields. Content-derived
  object names prevent a later partial batch from overwriting earlier evidence.
- Upload plus bounded read-back equality precedes deletion. No configured
  archive, a read error, oversized batch or failed admission preserves records.
- Each batch is at most 100 candidates, 10,000 historical versions and 32 MiB
  uncompressed. Each policy considers at most 20,000 current candidates/night.
  A batch over budget fails visibly; investigate that record before adjusting
  limits. Model-call/observation discovery retains its 50,000-candidate cap.
- Deletion uses exact archived `(concept,id,createdAt)` keys and keeps a whole
  batch if a newer revision or unarchived child appeared. Vector cleanup shares
  the transaction and preserves vectors belonging to any remaining version.
- System runs and their step/closed-approval histories share the same archive
  transaction. A retained observation/model call, pending approval, recent child
  or child owned by a user preserves the parent. Longer detail policies win.
- Index migrations build one Timescale chunk per transaction, serialize index
  builders, and verify coverage after interrupted-build repair.

## Verify a deployment

1. Confirm the work integration constructed its Azure archive client. A configured
   container without storage credentials produces an explicit startup warning.
2. Invoke `workRetentionSweep(dryRun: true)` as an authenticated cluster owner to
   review candidates and resolved per-concept windows without writes or uploads.
3. After the nightly run, inspect `work: operational retention pass complete`
   (`component=work.retention`) and the run result. Check candidates, archived
   versions, deleted versions, object names and errors. A successful no-op with
   zero expired candidates is expected while records are younger than policy.
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
