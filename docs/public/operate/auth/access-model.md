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
  `MEMQL_IDENTITY_INTERNAL_DOMAINS`
- `preferences` -- theme, language, notifications, archive
  retention, voice mode, and product-specific control settings
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

### The unauthenticated HTTP surface is declared, not inherited

The verifier middleware is installed with `server.PublicPaths()`, an
explicit allowlist (health probes, JWKS, auth endpoints, metrics,
Polyphon service-to-service, the concept API). On a verifier-consuming
node **public is opt-in**: a route is unauthenticated only because
someone put it on that list.

Two binaries install no HTTP auth middleware at all -- the `identity`
binary (it is the JWKS authority and must not verify against itself)
and any node running `MEMQL_IDENTITY_ENABLED=false`. On those,
`PublicPaths()` is never consulted, so without a further check "public"
would become the default with no opt-out. That is how
`POST /automations/{name}/trigger` and `POST /automations/resume`
became unauthenticated on identity (memql#2937, memql#2908).

Every route such a binary serves through a path the check can see (the
"Scope" paragraph below states exactly which those are) must therefore be
accounted for by one of two declarations, and `createHTTPServer`
**refuses to boot** when one is in neither:

| declaration | meaning |
|---|---|
| `server.PublicPaths()` | genuinely public on every node |
| `server.HandlerAuthorizedPaths()` | not public, but authorizes inside the handler and fails closed with no credentials, so it is safe where no middleware runs ahead of it |

A **third** declaration exists, and it answers a different question from
those two (memql#3062):

| declaration | meaning |
|---|---|
| `server.SelfAuthenticatedPaths()` | reachable **without a memQL credential on a node that DOES install the verifier**, because the route authenticates itself with a credential that is not a memQL identity |

The first two cannot express a third-party webhook. `PublicPaths()`
would work, but it is matched with an open **prefix** walk, so listing
`/inbound/` there would exempt anything mounted beneath it later -- and
it would declare the route *unauthenticated* rather than
*differently-authenticated*. `HandlerAuthorizedPaths()` is consulted
**only** on a binary with no verifier, so on the bff -- which installs
one -- it never runs, and a webhook carrying a vendor HMAC instead of a
memQL bearer is rejected before the handler's allowlist and signature
check ever execute.

Membership in the third tier means **the bearer middleware steps
aside**, not that the route is unauthenticated. Two properties keep that
narrow:

- **Matching is bounded to one path segment.** `/inbound/shopify`
  matches `/inbound/`; `/inbound/shopify/anything` does not, and neither
  does `/inbound/` alone. A route mounted deeper later cannot inherit
  the exemption, so granting one stays an explicit act.
- **The route must independently fail closed.** A self-authenticated
  route must ALSO be declared in `HandlerAuthorizedPaths()`, which is
  what certifies that property, and
  `server.AssertSelfAuthenticatedRoutesFailClosed()` **refuses to boot**
  -- on *every* binary -- when the two lists disagree. That check runs
  regardless of whether a verifier is installed, because the hole it
  guards opens on the nodes where one *is*.

`POST /inbound/{source}` is the only member today. It fails closed twice
with no credentials: an unlisted source is `404` (the allowlist is empty
unless an operator populates it) and a listed one without a matching
HMAC signature is `401`.

The automations routes sit in the second list, justified by the
owner-or-admin checks added in memql#2938. They are deliberately **not**
in `PublicPaths()`: that list is consulted on every verifier-consuming
node, so listing them there would make them unauthenticated everywhere.

`/memql/ws` is also declared there, on narrower grounds: the upgrade
itself performs no auth check, but on a verifier-less binary it tunnels
to a gRPC chain of `OperatorAware(RejectAll)`, so it fails closed at the
next hop rather than in the handler.

Identity's own discovery documents -- `/.well-known/memql-config.json`
and `/.well-known/oauth-authorization-server` -- are declared in
`PublicPaths()` via `server.IdentityDiscoveryPaths()`. They reach the mux
through `Service.RegisterRoutes`, which the assertion does not see, so a
test reads their registration out of the source and fails if either stops
being declared.

**Scope differs between the two binaries, deliberately.** The identity
binary asserts the contract routes plus everything app code mounts
through `a.handleRoute` (7 paths today). Routes mounted by middleware
ahead of the mux, or registered via an aliased copy of it, are not yet
covered -- see memql#3004. A node running
`MEMQL_IDENTITY_ENABLED=false` asserts the contract routes only -- and
**not** because the other routes are safe on that node. They are not.
That mode disables authentication outright, and the details matter if you
are tempted to rely on it: "everything is the cluster owner" is true of
gRPC and not of HTTP (the local-dev admit path is a *stream*
interceptor); attachments and audio return 401 on the missing actor, but
the Polyphon room-token and status handlers check nothing at all; and the
gateway middleware sits *ahead* of the mux and admits
`POST /memql/query` as the synthetic cluster owner. That is the toggle's
documented meaning, which is why it is loudly warned and must never be
set in staging or production.

The scope is a floor rather than a full check because a per-route "safe
unauthenticated" declaration would assert something false by construction
in a mode where nothing is authenticated. The identity binary gets the
wider scope because there the declarations mean something: that node
serves an unauthenticated HTTP surface **in production**.

**Being declared is not enough on its own -- it has to be declared in
time.** `createHTTPServer` reads the registered set once, and later build
phases still run after it, so a route mounted afterwards would be served
having never been checked. The set is sealed when it is asserted, and
registering after that point is fatal.

This authenticates nothing new. It makes leaving a route unauthenticated
an explicit, reviewable act rather than an omission. Registering a route
directly on the mux instead of through `a.handleRoute` fails an AST gate;
adding one to the contract without classifying it fails the
`ContractRoutes()` drift check; registering one too late fails the seal;
and either way the boot assertion is the backstop.

One thing the machinery deliberately does **not** do: stop you choosing
the wrong list. Putting a new route in `PublicPaths()` makes it
unauthenticated on every verifier-consuming node, and nothing fails,
because that is a legitimate classification -- it is how the health
probes and the JWKS feed are declared. The boot error names both lists
and states the test for each; picking between them is a review decision,
not a mechanical one. `HandlerAuthorizedPaths()` entries are guarded
against appearing in `PublicPaths()` as well, since for those two routes
the answer is already known.

See `component/server/unauthenticated_surface.go` and
`app/mux_registration_test.go` (memql#2939).

### Stream lifecycle

1. gRPC stream opens. The verifier middleware validates the JWT and
   attaches claims to the stream context.
2. First message reaches `handleMessage`. The access middleware
   calls `ensureAccess(ctx)` (`component/grpc/server.go`), which
   resolves the actor through `IdentityResolver.LoadFromClaims`
   (`component/auth/identity_resolver.go`):
   - `sub` must already be a canonical `v1:identity:user:<...>` id;
     every identity-service-issued JWT carries one, and anything else
     is rejected with `ErrUserNotProvisioned`.
   - It then runs **`userByIdSystem(userId)`** for the row that supplies
     Role and email. That query is `@serverOnly`, and it is the call
     that makes caller-scoping circular (#2800) -- the read that builds
     the actor cannot itself be filtered on the actor.
   The resolved `AccessContext` is cached on the stream.

   Note the query name. **`userById` is a different query**, gated by
   `requiresOwnerOrAdmin`, and it is NOT the bootstrap. Naming it here
   was wrong for long enough that it spread to five other places -- two
   documents and three code comments (memql#2984); a reader who follows the citation to an
   owner-or-admin-gated query concludes the circularity constraint is
   imaginary.
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
- [badge-operator-grant.md](badge-operator-grant.md) -- shared-terminal
  operator attribution: registered badges exchange into short-lived,
  role-ceiling-clamped class="badge" grants (memql#2513).
- [identity-service.md](identity-service.md) -- operator-side
  narrative.
- [docs/public/language/authoring-rules.md](../../language/authoring-rules.md)
- [docs/internal/planning/roadmap.md](../../../internal/planning/roadmap.md) -- deferred follow-up work.
