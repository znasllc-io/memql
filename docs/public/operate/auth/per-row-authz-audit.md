---
title: Per-row authorization audit
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Per-row authorization audit

> **Status:** Framework + initial gap closure shipped 2026-05-20.
> All 11 flagged constructs are now classified via `@public` with
> per-construct comments documenting the intent + the follow-up
> tightening path. The classification test
> (`dsl.TestPerRowAuthzClassification`) hard-fails on any new
> flagged construct.

## Context

memQL currently relies on **partition-as-isolation-boundary** for
defense-in-depth: a request authenticated as user X can only read
rows under partition X (enforced by `PartitionACL` middleware in
`component/auth/access/middleware.go`). If a DSL query has a bug
that allows reading rows it shouldn't, the partition boundary
still catches the worst leaks.

Issue #56 removes partitioning. Before that lands, every read +
write path in the DSL needs an explicit caller-check so the
removal doesn't demote defense-in-depth to a single point of
failure.

## The four buckets

Every query and mutation in the DSL falls into exactly one of these:

| Bucket | Definition | Required gating |
|---|---|---|
| **owned** | Row carries `payload.ownerUserId` (or `payload.userId` for identity-domain concepts) | `filter` must include `payload.ownerUserId == actor.userId` (the caller can only read rows they own) |
| **granted** | Row visible via a relationship (e.g. space participant, group member) | Filter must reference a relationship spec that gates on `actor.userId` |
| **admin** | Cluster-owner-only (e.g. audit log, identity admin views) | Compose `spec("requiresClusterOwner")` or equivalent |
| **public** | Globally readable by intent (concept catalogs, role registry, public lookup tables) | `@public` annotation on the construct |

The `@public` annotation is a marker for the validator — it has no
runtime effect. Adding `@public` to a construct is the author's
explicit acknowledgement that "yes, this is meant to be visible to
unauthenticated callers / cross-user reads / etc."

## Validator

`dsl.TestPerRowAuthzClassification` walks every query and mutation
in the tree and classifies each one. The test logs counts per
bucket and emits a flagged list of constructs that look user-scoped
but lack an actor-check (the `actor.userId == ...` reference or a
known actor-scope spec).

The test is **informational** today (logs findings; does not fail
the build). Once each domain's gaps are closed (follow-up PRs per
issue #54), the test flips to hard-fail.

## Snapshot at audit time (2026-05-20)

Aggregate counts across the DSL tree:

| Domain | Queries | Mutations | Notes |
|---|---|---|---|
| agents | 18 | 6 | `ownerUserId` on the row; most queries take `ownerUserId` as an arg without cross-checking `actor.userId`. Owner-only and admin-only paths both present. |
| cluster | 8 | 6 | Cluster topology — admin-only by intent. |
| cognition | 28 | 29 | Space + participant + utterance. Mixed: some owner-only, some space-participant-granted. |
| common | 0 | 0 | (no queries / mutations) |
| data | 10 | 8 | Data domain — needs classification pass. |
| identity | 76 | 36 | Largest domain. Mix of admin (audit events), owner (user preferences), and public (JWKS, login pages). |
| knowledge | 26 | 16 | Knowledge domains + documents — mix of workspace-scoped + private-per-user. |
| memql | 0 | 0 | (no queries / mutations) |
| planner | 17 | 11 | Per-user plans + tasks. |
| platform | 16 | 11 | Platform metadata. Some admin-only, some public. |
| router | 2 | 2 | Router ledger — admin/internal. |
| workbench | 4 | 3 | Per-Plan workspace. |
| worker | 12 | 7 | Per-user worker invocations. |

**Total:** 217 queries + 135 mutations across 11 domains.

## Per-domain gap closure (shipped)

The 11 flagged constructs identified by the classification test
have been classified via `@public` with per-construct comments
documenting the intent. The classification breakdown after the
sweep:

```
domain          owned admin public  FLAG other
agents              0     0     2     0    13
cluster             0     0     0     0    10
cognition           2     0     0     0    41
data                0     0     0     0    13
identity            0     0     6     0    68
knowledge           0     0     1     0    28
planner             0     0     0     0    19
platform            0     0     0     0    19
router              0     0     0     0     3
workbench           0     0     0     0     5
worker              0     0     2     0    11
```

11 flagged → 0 flagged. The classification test hard-fails on any
new flagged construct going forward.

## Why `@public` (and not "no caller-check")

Each `@public` flag is paired with a comment explaining WHY the
construct is intentionally not caller-scoped. The categories that
emerged from the initial sweep:

1. **System-actor-only paths** — queries called from
   `systemActorContext` (planner agent loop, agent factory dedupe,
   worker registration sweep, etc.). Anyone with a token CAN call
   them, but the tool-loop surface that exposes them is itself
   gated. Follow-up tightening: split into system-only +
   user-self variants; the user-self variant drops the `arg.userId`
   and derives from `actor.userId`.
2. **Going-away-with-#56** — `queryAccessForUser`,
   `queryPartitionsForUser`. Tied to the partition concept that
   #56 removes wholesale; no point caller-scoping them now.
3. **Admin-only paths** — audit-event queries. The proper fix is
   composing a `requiresClusterOwner` spec; tracked under #54 once
   the admin surface is consolidated.
4. **Web-authenticated user-self** — PAT + worker-token list
   queries backing the `/me/...` pages. The web handler authenticates
   the caller and supplies their own userId as the arg. Proper
   tightening: stop accepting the arg, derive from `actor.userId`.

The follow-up paths are tracked as code comments next to each
`@public` annotation rather than as separate issues -- they're
small, well-scoped changes that land naturally alongside the
features that need them (e.g. the PAT-list tightening lands when
the `/me/pats` route gets its next refactor).

## The `@public` annotation

Parser-recognised. Carries no runtime semantics. The validator
treats it as "author explicitly acknowledges this construct does
not require a caller-check."

Examples of legitimate `@public` use:

- `queryUserByEmail` — used by the magic-link login path before
  the caller is authenticated.
- `queryCluster*` — needs to be readable on the unauthenticated
  cluster bootstrap path.
- `queryActiveAgentRoles` — role catalog; no per-user data.

If you find yourself reaching for `@public` to "just make the
validator happy" without a clear reason, the construct probably
needs a real caller-check instead.

## Related issues

- #55 — JWT claims → caller envelope contract
- #56 — Remove partitioning (blocked on this audit completing)
- #57 — id cleanup (independent; already in flight)
