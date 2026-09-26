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
keep their existing lifecycle. The recovery query materializes current version
keys, probes the partial nonterminal index, and only then fetches bounded
payloads. Production Timescale verification found that a correlated concept
reference in the earlier anti-join forced historical heap reads despite the
index. The current-key plan completed in 1.3 seconds with 104,018 logical runs
and 5,922 unfinished runs, while the earlier plan exceeded the eight-second
statement limit. It no longer reads completed payload histories every two minutes.

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
