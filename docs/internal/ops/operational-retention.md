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

Nothing is stored between nights. The next pass starts again at the oldest
version still stored, which is not always where the work is: a version no policy
retires (a person's run, an old version of live work) anchors the start, and the
windows from there to the work are read again every night and found to hold no
candidate. A window with no version stored at all is jumped, so a version dated
1970 costs a few reads and not tens of thousands; a window that holds versions
no policy retires is read and passed, at about 21 ms at 5,000 runs a day, so
roughly 10 to 30 s per year of installation age, per policy. A backlog is
retired over as many nights as it takes, oldest first, and a persisted low-water
mark or an index on `(concept, "createdAt")` that would end the re-walk is
deferred to release 2.

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

Not bounded by a day: the goal policy's "no run of it remains" compares every run
version with the goal and no index serves it (0.34 s a read at 600,000 run
versions, 1.26 s with cold buffers). That is accepted for this release; it runs
last, so a failure there costs nothing that went before it, and the partial
expression index on the run's `goalId` that would bound it is deferred to
release 2. The journal sweep's expired-rows read takes the latest version of
every id of one concept (0.41 s for 400,000 model-call versions) and grows with
the concept; windowing it is deferred to release 2. It no longer decides the
night: `workRetentionSweep` runs the operational policies even when the journal
half fails, and returns both errors.

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
