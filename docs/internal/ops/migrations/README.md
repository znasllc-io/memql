---
title: Data Migrations
audience: ops
status: stable
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Data Migrations

One-shot scripts that fix up data shapes that earlier engine code
left in a non-canonical state. These are **not** schema migrations
in the conventional ORM sense — memQL has no rigid schema; every
node row is a JSON payload validated at write time against the
concept's `@field` declarations. What lives here are corrective
scripts run on existing databases to bring rows in line with a
new contract a code fix has just introduced.

## Index

| Migration | Issue | When to run | Symptoms it fixes |
|---|---|---|---|
| [dedupe-peruser-seeds](dedupe-peruser-seeds.md) | [#275](https://github.com/znasllc-io/memql/issues/275) | Any environment cluster-booted **before PR #274 merged** | A user's daily-space participant panel shows 2+ "Assistant" entries; `queryActiveAgents` returns multiple agent rows for the same `(ownerUserId, role)` pair. |

## Convention

A migration is a Go program under `scripts/<migration-name>/` with:

- `--dry-run` and `--execute` flags (mutually exclusive; one
  required). Dry-run prints the plan and exits without writing.
- Plan computation extracted to a pure function with table-driven
  tests in `plan.go` + `plan_test.go`. No DB access in the
  pure part — the SQL execution layer in `main.go` is the only
  thing that touches `database/sql`.
- An operational README at `scripts/<migration-name>/README.md`
  describing the DSN, the exact dry-run / execute commands, and
  the post-run cluster restart (if any) the script's plan output
  requires.
- A corresponding entry in this `docs/migrations/` directory that
  cross-links the script's README and the tracking issue, so an
  operator can land on a discoverable doc when triaging a
  symptom.

`MEMORY_NODES_DATABASE_DSN` is the canonical environment variable
every migration script reads; `--dsn=...` is the explicit override.

## Why hard delete?

memQL is time-series. Soft delete (a new node version with
`deleted=true`) leaves the prior versions in `MemoryNodes` and the
PK includes `createdAt`, so even a "deleted" version of a
doomed-id row keeps every prior version queryable. For
corrective cleanups where the goal is **the doomed-id row should
never have existed**, hard delete via `DELETE FROM "MemoryNodes"
WHERE id = ANY($1)` is the only path that achieves the intent.

The contract for hard delete: the migration script must compute
its plan deterministically from the current DB state, run the
plan inside one transaction (so partial failure rolls back), and
print enough detail in the dry-run to let an operator diff the
script's intent against the live data before approving the
execute pass.

## Cross-client impact

Migrations touch the memql data plane, so any client (copresent,
memql-cockpit, future clients) inherits the post-state
automatically. No client-side change is needed — that's a
deliberate property of the data-plane-first design.

## Runbook expectations

A migration's docs/migrations/<name>.md entry should answer, in
order:

1. **Symptom** an operator would observe before running the
   migration.
2. **Affected environments** by date or by triggering merge —
   "any cluster booted before PR #N", "any environment whose
   first user signup predates 2026-04-20", etc.
3. **Dry-run command** the operator runs first, with the exact
   `MEMORY_NODES_DATABASE_DSN` shape for each environment
   (compose, staging, prod).
4. **What to verify** in the dry-run output before approving the
   execute pass.
5. **Execute command** plus any post-run cluster restart.
6. **Acceptance check** the operator runs after execute (a DB
   query or a UI verification) that confirms the migration
   landed.
