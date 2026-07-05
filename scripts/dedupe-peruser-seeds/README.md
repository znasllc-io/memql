# dedupe-peruser-seeds

One-shot data migration that cleans up duplicate per-user seed rows
created by pre-PR-#274 cluster boots. Tracked at [#275](https://github.com/znasllc-io/memql/issues/275)
under epic [#271](https://github.com/znasllc-io/memql/issues/271).

## When to run

Run on any environment that was cluster-booted before [PR #274](https://github.com/znasllc-io/memql/pull/274)
landed. Symptom: a user's agent list contains multiple rows for the
same `role` (e.g. three "Assistant" entries in the participant panel),
or the daily space participant panel shows multiple AI participants
that all share the same display name.

The PR #274 fix prevents NEW duplicates by making the seed materializer
write deterministic `<seedName>-<userId>` ids. This script cleans up
the pre-existing dupes that #274 can't retroactively fix.

## How to run

```bash
# Inspect the plan without writing.
MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable' \
  go run ./scripts/dedupe-peruser-seeds --dry-run

# Apply the plan.
MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable' \
  go run ./scripts/dedupe-peruser-seeds --execute
```

For the local k3d cluster, the host can reach the DB at `localhost:5432`
(the postgres port-forward). For staging / prod, substitute the
appropriate DSN.

The script accepts `--dsn=postgres://...` as an explicit alternative
to the env var.

## What it does

1. Loads every distinct `v1:agents:agent` (or any concept) row whose
   `provenance.kind = 'seed'` and `payload.ownerUserId` is non-empty
   — the per-user seed-materialized rows.
2. Loads every non-`left` `v1:cognition:participant` AI row pointing
   at any of those agents.
3. For each `(provenance.name, ownerUserId)` group, computes the
   canonical id `<concept>:<seedName>-<bareUserId>` (mirrors
   `deterministicPerUserSeedId` in `component/memql/seed_materializer.go`).
4. Dooms every row whose id doesn't match the canonical form.
5. Dooms every participant pointing at a doomed agent id.
6. Runs a **read-only audit** of every other concept that carries a
   single-valued `payload.agentId` field (authorizations, delegations,
   audio/video overrides, client-tool requests, utterance source ids)
   and prints per-concept row counts pointing at the doomed agent
   ids. The audit is informational only — those rows are NOT touched
   by either the dry-run or execute pass. See the runbook at
   [`docs/internal/ops/migrations/dedupe-peruser-seeds.md`](../../docs/internal/ops/migrations/dedupe-peruser-seeds.md)
   for follow-up guidance on each audited concept.
7. In `--execute` mode, runs both deletes (agent + participant rows)
   inside one transaction.

Hard delete is intentional. memql's `(id, "createdAt")` PK means soft
delete (a new version with `deleted=true`) would still leave the
doomed-id versions in the time-series, defeating the cleanup. The
duplicates were never legitimate data.

## After `--execute`

If any group ends up with no keeper (the common case — every existing
row carries a random UUID, none match the deterministic form), the
script prints a `NEXT STEP` message. **Restart the memql cluster** so
the seed materializer's startup sweep re-writes the canonical row:

```bash
# local k3d:
make dev NODE=bff,cognition,agent,planner
# or directly:
kubectl rollout restart deploy/bff deploy/cognition deploy/agent deploy/planner deploy/voice -n memql
```

Each restarted node's `SeedMaterializer.Start()` will re-iterate
every user and materialize the perUser seeds at the canonical id.
Concurrent races across nodes collapse to versions of one logical
row via #274's fix.

## Plan computation is pure

`plan.go` carries the doomed-id computation as a pure function with
no DB access. `plan_test.go` covers the cases that came up in the
#273 / #275 investigation: no-dupes, all-doomed-needs-reseed,
keeper-present-dooms-the-rest, multiple-seeds-per-owner,
multiple-owners-independent, participants-doomed-with-their-agents,
and defensive filtering of non-perUser rows.

## Cross-client impact

The cleanup is at the DB layer, so both the product SPA and memql-cockpit
inherit the post-state automatically. No client-side change needed.
