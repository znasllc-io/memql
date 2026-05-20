# Per-row authorization audit

> **Status:** Framework in place. Per-domain audit + gap closure is
> follow-up work tracked under issue #54.

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
| **granted** | Row visible via a relationship (e.g. space participant, group member) | Filter must reference a relationship spec that gates on `caller.userId` |
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
but lack a caller-check (the `caller.userId == ...` reference or a
known caller-scope spec).

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

## Per-domain gap closure (follow-up)

Each domain gets a small focused PR that classifies its constructs
and adds the appropriate gating. Done in this order to control
review burden + regression risk:

1. agents (small, well-defined ownership model — good warmup)
2. worker (similar to agents)
3. workbench (small)
4. planner (per-user)
5. knowledge (mixed workspace + private)
6. cognition (largest behavioral surface)
7. platform + router + data + cluster (mostly admin/internal)
8. identity (largest; lots of public-by-design endpoints — JWKS,
   login pages, etc. — careful classification needed)

When each domain is green, that domain's exempt-list entry in the
validator gets removed. When all domains are green, the validator
flips to hard-fail.

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
