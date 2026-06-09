---
title: Access Model
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Access Model

> **Status (#56 in progress):** the per-partition ACL layer described
> below has been retired (phase 4). Authentication + identity stay
> identical; authorization is enforced **per row** inside DSL queries
> and mutations -- see
> [per-row-authz-audit.md](per-row-authz-audit.md) for the four
> buckets (owned / granted / admin / public) and how each domain
> classifies its constructs. The remaining `partition` references in
> this doc reflect historical behavior; later #56 phases strip the
> envelope dimension entirely.

memQL's authorization has three layers: **authentication** (who are
you), **identity** (which credential you're using), and
**authorization** (per-row checks inside the DSL: ownership /
grants / admin / public). This document describes the data model and
the enforcement points after the cluster's cutover to the in-house
identity service (`component/identity`).

For the registration / first-login flow see
[user-provisioning.md](user-provisioning.md). For the operator-side
narrative (env vars, deployment, key rotation) see
[identity-service.md](identity-service.md).

## Concept model

All identity concepts are **global-scoped** (`@scope("global")`):
rows live in the reserved `_system` partition and are readable from
every tenant's view. The partition selector on the wire does not
hide them.

### `v1:identity:user`

The person. One record per human (or synthetic principal). Dedup key
is `primaryEmail`.

Key fields:

- `displayName`, `primaryEmail`
- `role` -- cluster-wide role: `owner` / `admin` / `writer` / `reader`
- `internal` -- true when registration matched
  `IDENTITY_INTERNAL_DOMAINS`
- `preferences` -- theme, language, notifications, archive
  retention, voice mode, CoPresent Control settings
- `active`, `suspendedAt`, `suspendedReason`, `lastSeenAt`
- `legalAcceptance[]` -- append-only history of ToS / Privacy
  acceptances
- `deletionScheduledAt` -- soft-delete request timestamp; honored by
  the `accountDeletionSweep` cron after the configured cooldown

### `v1:identity:identity`

A credential set owned by a user. One user can have many identities:

- A magic-link verified email (`identityType: "magic_link"`) -- the
  primary path, produced by the identity service's magic-link flow
- An OAuth token for an external app (`identityType: "oauth"`) --
  used by agents acting through user-owned external connections
- A Personal Access Token (`identityType: "api_key"`) -- CLI clients
  authenticate with `mql_pat_<...>`
- A service account (`identityType: "service_account"`) -- reserved
- A worker token (`identityType: "worker_token"`) -- used by
  memql-cockpit-worker processes; admitted only on
  `WorkerService.Stream`

Key fields:

- `userId` -- owner (links to `v1:identity:user`)
- `identityType` -- the credential family
- `credentials` -- shape depends on identityType (see the concept
  file for the variant block)
- `usableByAgents` -- whether `v1:identity:delegation` can borrow
  this identity for agent work
- `active`, `lastUsedAt`

### `v1:identity:partitionAccess`

The grant. One row per `(userId, partition)`. Re-granting appends a
new time-series version so history is preserved; hard-delete is
never used for access rows.

Key fields:

- `userId` -- recipient
- `partitionName` -- the target partition's name
- `role` -- per-partition role (same enum as user.role)
- `grantedBy`, `grantedAt`, `expiresAt`
- `active` -- soft-revoke flag
- `source` -- `manual` today. The enum is reserved for future
  provenance variants (e.g. SCIM-driven, SSO-group-driven) so a
  sync job can later own only its own rows via `sourceRef`.

> `partitionName`, not `partition` -- `partition` is reserved at the
> engine's payload level (see
> [memql-authoring-rules.md](../../language/authoring-rules.md#12-partition-is-a-reserved-payload-field----use-partitionname)).

### `v1:identity:authSession`

Per-token session record. The identity service's magic-link / refresh
handlers create one row per access token. Looked up on every
authenticated request to enforce per-session revocation.

Key fields: `userId`, `subject`, `tokenHash`, `expiresAt`,
`firstAuthenticatedAt`, `lastRefreshedAt`, `refreshTokenHash`,
`previousRefreshTokenHash`, `previousRotatedAt`, `revokedAt`.

`previousRefreshTokenHash` + `previousRotatedAt` carry a 30-second
grace window for the IMMEDIATELY-PREVIOUS refresh-token hash. The
rotator accepts the previous hash inside that window, which fixes
the "client hard-refreshed mid-rotation" race where the server
already rotated the cookie but the browser aborted the response
before consuming the `Set-Cookie` header. Past the window the
previous hash is treated as stale. See
`component/identity/refresh/rotate.go`.

### `v1:identity:delegation`

Orthogonal. Grants an agent the right to act through a user's
identity for a bounded role / scope / lifetime. Also global-scoped.

### `v1:identity:invitation`

Token-hashed invitation credential for guest invites and
admin-issued user invitations.

## Role spectrum

One enum, used everywhere: **owner / admin / writer / reader**.

| Role   | Cluster-wide effect                                    | Per-partition effect                                      |
|--------|--------------------------------------------------------|-----------------------------------------------------------|
| owner  | Bypasses the per-partition ACL entirely                | (N/A -- cluster owners see everything)                    |
| admin  | No ACL bypass. Still needs a grant to touch any        | Partition-level root. Manages other roles within          |
|        | partition's data.                                      | the partition.                                            |
| writer | Regular data producer.                                 | Can read and mutate data within the partition.            |
| reader | Regular data consumer.                                 | Read-only.                                                |

## Cluster role vs partition role

The cluster-wide role on `v1:identity:user.role` answers:

- **Owner?** Then the partition ACL is irrelevant -- you can target
  any partition.
- **Everyone else?** Then your access is defined by your
  `v1:identity:partitionAccess` rows. A user with `role: "admin"`
  cluster-wide but no partition grants can't read or write any data;
  they can only perform cluster-level management operations
  (granting access, managing users).

The split is intentional: "I can manage users" and "I can see
partition X" are different concerns.

## Enforcement

### Token verification

Every node binary other than `identity` runs the per-node verifier
middleware (`component/identity/verifier`). On each gRPC stream open:

1. Bearer token is extracted from `Authorization`.
2. **PAT path** (`mql_pat_<...>`): rejected on bff/voice/etc.
   PAT verification is the identity binary's responsibility; CLI
   clients hit the identity binary directly.
3. **JWT path**: parsed for the `kid` header, validated against the
   JWKS-cached EdDSA public key. The verifier checks signature, exp,
   `iss`, and `aud`. Unknown `kid` triggers a one-shot JWKS refresh
   to handle rotation overlap.
4. The verified claims (`sub`, `email`, `name`, `role`, `internal`,
   `partitions`, `sid`) are stamped onto the request context using
   `auth.ContextWithClaims` + `auth.BuildTokenInfo`, exactly as the
   legacy auth path did.

### Stream lifecycle

1. gRPC stream opens. The verifier middleware validates the JWT and
   attaches claims to the stream context.
2. First message reaches `handleMessage`. The access middleware
   calls `ensureAccess(ctx)`, which runs `LoadAccessFromClaims`:
   - If `sub` is already a canonical `v1:identity:user:<...>` id
     (the identity-service path), the lookup skips straight to
     `userById(sub)` and `accessForUser(sub)`.
   - For legacy external subjects, it walks
     `identityBySubject(sub)` -> `userById(userId)` ->
     `accessForUser(userId)`.
   The resolved `AccessContext` is cached on the stream.
3. Per message: `CheckPartition(ctx, accessCtx, envelope.partition,
   messageId)`:
   - Reject `_system` unconditionally.
   - Cluster owners bypass.
   - Otherwise the partition must appear in the caller's ACL.
4. `listPartitions` post-filter: the gRPC server trims the response
   to only partitions in the caller's ACL (owners see everything).

### Subscription scoping

Stream subscriptions that send a `*` partition wildcard get
server-side rewritten via `scopeGraphPatternToPartition` so a
subscriber cannot observe other tenants' events. Cluster owners
ride the same path -- they bypass the per-partition ACL but the
events still scope by envelope.

### Session revocation

After the verifier accepts a JWT, the session-revocation middleware
(`component/grpc/auth_session_middleware.go`) hashes the bearer
token and looks up the matching `v1:identity:authSession` row.
Revoked rows fail the request with 401 / `Unauthenticated`. The
check runs at stream-open time only -- already-established streams
keep their socket open until the JWT expires or the client
disconnects.

### Audit

Every rejection logs at `Info` level with subject / user id /
partition / reason. Reasons today:

- `system_addressed`  -- caller set partition=`_system`
- `no_access`         -- caller has no grant for that partition
- `no_access_context` -- internal: middleware ran before access
  context loaded

## Cockpit Settings: My Access

The Cockpit's Settings tab includes a **MY ACCESS** panel showing
account + per-partition grants. The data comes from a dedicated
gRPC message (`MyAccessMsg` / `MyAccessResult`).

## Granting access

Today granting access goes through `mutationGrantPartitionAccess`.
The admin web app under `/admin/*` (mounted by the identity
binary) provides a UI for it.

## Out of scope (deferred)

- **Per-concept ACL.** Today access is at partition granularity.
- **Writer-vs-reader enforcement inside a partition.** The
  middleware checks "is the caller granted ANY role in this
  partition?"; it does not yet block readers from issuing
  mutations. Tracked in [ROADMAP.md](../../../internal/planning/roadmap.md).
- **Time-bounded grants UI.** `expiresAt` exists on the concept but
  Cockpit doesn't expose it as a form field yet.
- **Identity-merge UI.** If the same human ends up with two users
  (different emails), there's no merge tool. Avoid by using
  `primaryEmail` as the dedup key at registration.
- **Partition rename.** Access rows reference partitions by name;
  renaming would orphan grants.

## Related

- [user-provisioning.md](user-provisioning.md) -- registration modes,
  invitations, magic-link flow.
- [identity-service.md](identity-service.md) -- operator-side
  narrative.
- [docs/public/language/authoring-rules.md](../../language/authoring-rules.md)
- [docs/internal/planning/roadmap.md](../../../internal/planning/roadmap.md) -- deferred follow-up work.
