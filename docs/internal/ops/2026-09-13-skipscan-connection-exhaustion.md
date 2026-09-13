---
title: Incident: every concept read walked the whole hypertable (a production instance, 2026-09-13)
audience: internal
status: current
area: ops
sinceVersion: "0.21.25"
owner: platform
---

# Incident: every concept read walked the whole hypertable

**Where:** a production instance (the `entry` preset: one Postgres instance, 4 GiB,
200 connections). **When:** 16:56 UTC to about 18:05 UTC, 2026-09-13.
**Seen as:** "internal error" on most cold page loads; `/healthz` fine.

## What happened

The edge resolves a request host to a `v1:platform:site` row through the
engine. That read failed with `FATAL: remaining connection slots are reserved`
(SQLSTATE 53300): 184 of 200 slots were held, 149 of them by copies of ONE
query, `staleClusterNodes()`, each running 90 to 188 seconds.

The query itself was the cause. Every concept read the executor emits is "the
latest row per id within one concept":

```sql
SELECT DISTINCT ON (id) ... FROM "MemoryNodes"
WHERE concept = $1 ORDER BY id ASC, "createdAt" DESC
```

With only `(id, "createdAt" DESC)` and `(concept)` indexed, TimescaleDB plans
that as a SkipScan over the id index and filters `concept` per row, so a read
of one small concept walks every id in the table. Measured in production with
`EXPLAIN (ANALYZE, BUFFERS)`: 274 result rows, 1,006,847 rows removed by
filter, 6.9 GB of buffers, 178 seconds -- on a server running Postgres
defaults (128 MB `shared_buffers`) against a 3.8 GB hypertable.

Three callers issued it: the worker dialer on EVERY `v1:cluster:node`
heartbeat event on every pod, the readiness recompute on every worker
registration event, the topology reconciler every 3 s on the leader. The
v0.21.24 rollout restarted every pod at 16:20, each boot re-materialized every
seed row (2,628 `v1:rbac:capability` versions in one hour, each raising the
catalog-reload event on every node), the database fell behind, and from then
on each new copy of the read queued behind the last. A Go context that
expires closes the socket but the Postgres backend keeps running the
statement, so pods capped at 4 connections held 30 backends each.

## What fixed it, and what keeps it fixed

Each layer is a change in this repo, so a fresh install carries all of them.

| Layer | Change | Where |
|---|---|---|
| The plan | Composite index `(concept, id, "createdAt" DESC)` on `MemoryNodes`; the same query reads 379 rows instead of a million | migration `20260913000000_memory_nodes_concept_id_created_at_idx`, PR #5314 |
| The server | `statement_timeout` (60 s, `MEMQL_DB_STATEMENT_TIMEOUT_MS`) on every request-serving backend; migrations run over their own single connection without it | `component/database` |
| The database | `shared_buffers`, `effective_cache_size`, `work_mem`, `maintenance_work_mem` sized per preset beside the memory they assume | `deploy/k8s/components/cnpg-db/presets/*` |
| The writers | Seeds are written only when the row differs; a readiness verdict is restated only when it changes or is ten minutes old; heartbeat-driven rediscovery coalesces over 2 s | `seed_materializer.go`, `readiness_write.go`, `worker_dialer.go` |

Applying the preset memory parameters to a running instance RESTARTS Postgres
(`shared_buffers` needs one); the other three reload.

## Still open

Superseded row VERSIONS accumulate by design (heartbeats and readiness
reports change every time they are written). A per-concept retention sweep --
keep the latest N versions or a time window, archive first -- is the owner's
decision and waits on the backup strategy. Until then the composite index
keeps every read proportional to the concept it names, not to the table.

## How to read the next one

```sql
-- who holds the slots, and on what
select application_name, state, count(*) from pg_stat_activity
 where usename = 'memql' group by 1, 2 order by 3 desc;
-- the active statements by concept
select substring(query from 'concept = ''([^'']+)''') concept, count(*),
       round(avg(extract(epoch from now() - query_start))) avg_s
  from pg_stat_activity where state = 'active' and usename = 'memql'
 group by 1 order by 2 desc;
```

Then `EXPLAIN (ANALYZE, BUFFERS)` the worst statement verbatim and look for
`Custom Scan (SkipScan)` with a large `Rows Removed by Filter`. The
per-hour count of `connection slots are reserved` in the Postgres log says
when it began; correlate with the rollout time in `memql-znas`.
