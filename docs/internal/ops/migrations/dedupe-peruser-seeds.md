---
title: dedupe-peruser-seeds
audience: ops
status: stable
area: internal
sinceVersion: 0.9.0
owner: znas
---

# dedupe-peruser-seeds

Tracked: [#275](https://github.com/znasllc-io/memql/issues/275) under
epic [#271](https://github.com/znasllc-io/memql/issues/271). Companion
script: [`scripts/dedupe-peruser-seeds/README.md`](../../../../scripts/dedupe-peruser-seeds/README.md).

## Symptom

- Daily-space participant panel shows two or more entries for the
  same agent role (e.g. three "Assistant" rows that share a display
  name and gender).
- `activeAgents` / `activeAgentsForUser` returns multiple
  rows with the same `(ownerUserId, role)` pair.
- The Roster / Team tab on the product frontend or the cockpit's equivalent
  surface lists more rows than the user has ever created.

## Affected environments

Any environment that was **cluster-booted before
[PR #274](https://github.com/znasllc-io/memql/pull/274) merged**.
Pre-#274 the seed materializer minted a fresh random UUID for every
concurrent startup-sweep racer instead of the documented deterministic
`<seedName>-<userId>` id. Each node's startup sweep wrote its own
row, leaving 2-5 duplicate agent rows per user per perUser seed.

The fix (PR #274) prevents NEW duplicates by making `materializePerUser`
hash the (seed, userId) pair into a stable id. It does not retroactively
remove the dupes already on disk; this migration does.

## Dry-run command

Run from a host that can reach the target Postgres instance.

```bash
# Local k3d cluster (via the postgres port-forward, make db):
MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable' \
  go run ./scripts/dedupe-peruser-seeds --dry-run

# Staging / Cloud Run:
MEMQL_DATABASE_DSN='<staging-dsn>' \
  go run ./scripts/dedupe-peruser-seeds --dry-run
```

The script prints:

- A `--- migration plan ---` block listing every `(seedName,
  ownerUserId)` group and the agent + participant ids it intends to
  delete (`DOOM:` lines) plus the canonical id it will keep
  (`KEEP (canonical):`).
- A `--- agentId reference audit (read-only) ---` block reporting
  row counts in other concepts that point at the doomed ids
  (authorizations, delegations, audio/video overrides, utterances,
  client-tool requests). This block is INFORMATIONAL — the audit
  surfaces what else references the cleaned-up agents so operators
  can decide whether a follow-up rewrite is warranted; the migration
  itself does not touch these rows.
- A `summary-json:` line for machine consumption.

## What to verify in dry-run

1. Doomed agent count matches what you'd expect from the live
   symptom (typically 2-4 per affected user).
2. Every `(seedName, ownerUserId)` group's `KEEP (canonical)` line
   either names an existing row at the canonical id OR says
   `<missing -- reseed required>`. The latter is the COMMON case
   pre-#274 (no node ever wrote the canonical id) and triggers the
   post-execute cluster restart described below.
3. The agentId reference audit's counts are bounded — utterance
   counts can be large for chatty spaces, but
   authorization / delegation / override counts should be small
   (single-digit per affected user). Large numbers in those
   surfaces suggest a different bug.

## Execute command

```bash
MEMQL_DATABASE_DSN='<dsn>' \
  go run ./scripts/dedupe-peruser-seeds --execute
```

The execute pass runs both deletes (agent rows + participant rows)
inside a single transaction. Partial failure rolls back; no
cleanup is required after a failed execute.

If the dry-run reported any `<missing -- reseed required>`
groups, the execute pass prints a `NEXT STEP` block instructing a
cluster restart so the seed materializer's startup sweep writes
the canonical row:

```bash
# Local k3d cluster -- roll the seed-writing nodes:
kubectl rollout restart -n memql \
  deploy/bff deploy/cognition deploy/agent deploy/planner deploy/voice

# Staging / prod: the same kubectl rollout restart against the target cluster.
```

Concurrent racers across the restarted nodes collapse to versions
of one logical row via PR #274's fix.

## Acceptance check

After execute (and the cluster restart if it was required):

```sql
-- One assistant row per affected user
SELECT payload->>'ownerUserId' AS owner, COUNT(*) AS rows
FROM "MemoryNodes"
WHERE concept = 'v1:agents:agent'
  AND payload->>'role' = 'assistant'
  AND provenance->>'kind' = 'seed'
GROUP BY owner
HAVING COUNT(*) > 1;
-- Expect: zero rows returned.

-- One AI participant per daily space (run for a known affected user)
SELECT id, payload->>'agentId' AS agent_id, payload->>'displayName' AS name
FROM "MemoryNodes" mn
WHERE concept = 'v1:cognition:participant'
  AND payload->>'participantType' = 'si'
  AND payload->>'spaceId' = '<daily-space-id>'
  AND payload->>'status' <> 'left'
ORDER BY agent_id;
-- Expect: one row, agent_id matches the user's canonical assistant.
```

A successful run plus a user opening their daily space and seeing
exactly one Assistant in the participant panel is the acceptance
criterion.

## Followups not covered by this migration

The reference audit deliberately does NOT rewrite rows in the
audited concepts. The follow-up decisions:

- **Authorizations / delegations** pointing at doomed agents:
  operationally treat the agent as "unauthorized post-cleanup"
  and let the user re-grant. Rewriting payload.agentId on these
  rows would also work but is out of scope for a one-shot
  cleanup.
- **Audio / video overrides** pointing at doomed agents: same
  pattern. The next time the user toggles the orb on the
  canonical agent's row a fresh override is written; the doomed
  override row becomes harmless residue.
- **Utterances** with `payload.source.agentId` pointing at doomed
  agents: this is historical chat content; rewriting would
  rewrite history, which would also be visible in the chat
  transcript. Leave as-is unless a downstream consumer requires
  source-id reconciliation, in which case file a focused
  follow-up issue.

Array-valued `agentIds[]` references (e.g. on group rosters) are
**not** audited by this migration. If the cluster pre-#274 booted
agents that ended up on a group's roster, the audit's CAVEAT line
flags this and an operator should sweep manually.
