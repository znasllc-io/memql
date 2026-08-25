---
title: Access Model
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Access Model

> **Status (#56 complete):** the per-partition ACL layer is **gone**,
> not deprecated. Authentication + identity are unchanged;
> authorization is enforced **per row** -- see
> [per-row-authz-audit.md](per-row-authz-audit.md) for the four
> buckets (owned / granted / admin / public) and how each domain
> classifies its constructs.
>
> Measured in this checkout rather than asserted (memql#3305):
> `v1:identity:partitionAccess` does not exist in
> `dsl/identity/concepts.memql`; `"MemoryNodes"` has **no `partition`
> column** and its primary key is `(id, "createdAt")`; and
> `component/grpc/memql.proto` carries `reserved "partition"` in two
> messages plus `reserved "partitions"`. There is no envelope
> dimension under the DSL any more, so the per-row check is not
> defense in depth -- it is the only gate.
>
> What survives under the "partition" name is
> `v1:platform:partitionSecret` / `partitionVariable` in
> `dsl/platform/concepts.memql`. Those are **config storage** and
> derive nobody's visibility.

MemQL's authorization has three layers: **authentication** (who are
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

Identity concepts are cluster-wide: one row set, the same from every
caller's view. There is no scope annotation and no wire-level
selector that hides them -- `@scope("global")` / `@scope("partition")`
were retired with the rest of partitioning in #56, and authoring one
today is a load error (`dsl/_reference/_concept.memql` §5). What
limits who may *read* a given row is the per-row check described
under [Enforcement](#enforcement), nothing above it.

### `v1:identity:user`

The person. One record per human (or synthetic principal). Dedup key
is `primaryEmail`.

Key fields:

- `displayName`, `primaryEmail`
- `role` -- cluster-wide role: `owner` / `admin` / `developer` / `writer` / `reader`
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

One enum, cluster-wide, on `v1:identity:user.role`. Five values, as
declared in `component/auth/rbac.go` (`AllRoles()`) and in the
concept's own `role` field:

| Role      | Meaning                                                                  |
|-----------|--------------------------------------------------------------------------|
| owner     | The cluster operator. The only role `auth.IsClusterOwner()` accepts.      |
| admin     | User + cluster management. Not the same as owner -- see below.            |
| developer | Engineering power: authoring, inline DSL, deploy / cut-version. Not user management. |
| writer    | Regular data producer.                                                    |
| reader    | Regular data consumer.                                                    |

There is **no second, per-partition role**. The grant row that used to
carry one is gone; a user has exactly one role and it is cluster-wide.

## What the role actually decides

Since #56 there is no ACL layer between the caller and the row, so a
role matters only where something reads it. Two places do:

- **`auth.IsClusterOwner()` -- `Role == RoleOwner`, and nothing else.**
  This is the sole escape in the row-authz write guard
  (`rowAuthzWriteEscape`, `component/memql/rowauthz_write_guard.go`),
  alongside internal server origin. `admin` is deliberately **not** an
  escape there, and is not inferred from the read side or from the
  fact that admin sounds privileged.
- **Filter conjuncts that name the role**, such as the
  `requiresOwnerOrAdmin` spec guarding `searchUsers` and `userById` in
  `dsl/identity/queries.memql`. These are author-written, per-construct,
  and reach only the construct that names them.

The consequence is worth stating plainly: **a role is not a boundary
the engine applies on your behalf.** Row visibility comes from the
concept's `@rowAuthz` tier where one is declared, and from the
construct's own filter where one is not.

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
   `sid`) are stamped onto the request context using
   `auth.ContextWithClaims` + `auth.BuildTokenInfo`, exactly as the
   legacy auth path did. The identity-issued JWT no longer carries a
   `partitions` claim.

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
| `server.SelfAuthenticatedPaths()` | reachable **without a MemQL credential on a node that DOES install the verifier**, because the route authenticates itself with a credential that is not a MemQL identity |

The first two cannot express a third-party webhook. `PublicPaths()`
would work, but it is matched with an open **prefix** walk, so listing
`/inbound/` there would exempt anything mounted beneath it later -- and
it would declare the route *unauthenticated* rather than
*differently-authenticated*. `HandlerAuthorizedPaths()` is consulted
**only** on a binary with no verifier, so on the bff -- which installs
one -- it never runs, and a webhook carrying a vendor HMAC instead of a
MemQL bearer is rejected before the handler's allowlist and signature
check ever execute.

Membership in the third tier means **the bearer middleware steps
aside**, not that the route is unauthenticated. Three properties keep
that narrow:

- **Matching is bounded to one path segment.** `/inbound/shopify`
  matches `/inbound/`; `/inbound/shopify/anything` does not, and neither
  does `/inbound/` alone. A route mounted deeper later cannot inherit
  the exemption, so granting one stays an explicit act.
- **An exempted path is one the mux will actually route to the exempting
  handler.** Where the middleware's view of the request and the mux's can
  differ, the request is not exempted: an encoded separator in the mount
  segment (`/inbound%2Fx`), and any spelling normalization would rewrite
  before matching -- a trailing slash (`/inbound/shopify/`), or trailing
  whitespace (`/inbound/shopify/%20`, `/inbound/shopify%20`). None of
  these is a path `POST /inbound/{source}` matches, so each would hand an
  exemption to whatever answers a request the self-authenticating handler
  never sees. Today nothing does -- but that is a property of the current
  route table rather than of the matcher, which is why the refusal is on
  the general property ("the path arrived already normalized") rather
  than on the trailing slash alone (memql#3128).
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

One qualification, because the tier's whole justification rests on this
sentence. That holds for every source configured with a signature
scheme. A source configured `MEMQL_INBOUND_SOURCE_<X>_SIGNATURE_SCHEME=none`
is a **listed** source that accepts an unsigned request and stages the
row — `verify()` returns "unverified, no error" for that scheme, so the
handler proceeds and the staged row carries `signatureVerified=false`.
It is a deliberate per-source opt-in: the receiver fatals at boot if the
scheme is unset, and logs `source accepts UNVERIFIED requests` when it
is `none`. So it is loud rather than silent — but the fail-closed
property is **operator-configuration-dependent, not a property of the
code**, and the boot check certifies the declaration, not the
configuration.

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

There is no further per-message envelope check after that. #56 removed the
per-partition ACL this step used to run (`CheckPartition` against the
caller's ACL, and a `listPartitions` post-filter trimming the response) --
the `partition` wire field is `reserved` in `component/grpc/memql.proto` and
nothing derives scope from it any more. What decides whether a given
message may read or write a given row from here is the per-row check
described in [per-row-authz-audit.md](per-row-authz-audit.md): the query or
mutation the message executes carries its own `owned` / `granted` / `admin`
/ `public` gate, evaluated against the `AccessContext` this step resolved.
There is no separate envelope-level gate above that to describe.

### Session revocation

After the verifier accepts a JWT, the session-revocation middleware
(`component/grpc/auth_session_middleware.go`) hashes the bearer
token and looks up the matching `v1:identity:authSession` row.
Revoked rows fail the request with 401 / `Unauthenticated`. The
check runs at stream-open time only -- already-established streams
keep their socket open until the JWT expires or the client
disconnects.

## Shared mailboxes and passkey-only sign-in

An account can be registered with a group alias -- `team@example.com`,
`support@example.com` -- and nothing about it says so. That matters more
than it looks: anyone who can read the mailbox can request a sign-in link
and enter the account, so **the account's real sign-in surface is the
mailbox's reader list**. Two fields on `v1:identity:user` make that fact
visible and give its owner a way to close it (memql#4300, design
`docs/superpowers/specs/2026-08-22-magic-link-hardening-design.md`).

### `sharedMailbox` -- a hint

Set at registration by a local-part heuristic in
`component/identity/registration/shared_mailbox.go`: RFC 2142 role names
(`postmaster`, `abuse`, `security`, `support`, `info`, `sales`, ...) plus
the common team names (`team`, `hello`, `contact`, `admin`, `ops`, `hr`,
`billing`, ...). Exact match on the local part, `+tag` stripped, case
folded.

**It gates nothing.** It drives copy: a warning on `/me` and `/me/settings`,
a note on `/login` when a matching address is typed, and a field the
portal's Users view renders like any other. The user or an admin can set
or clear it; every change is audited `shared_mailbox_changed{by, from, to}`.

The heuristic is a guess, and both directions of correction matter: `info@`
belongs to plenty of solo operators, and a genuinely shared `orders-uk@`
will not be recognised. Matching is exact rather than substring precisely so
that `itzhak@`, `devon@` and `sales.rodriguez@` are not told their personal
accounts look like team inboxes.

### `signInPolicy` -- `any` (default) or `passkey_only`

`passkey_only` disables sign-in **links** for the account. A request for one
writes no row, sends no link, redirects the caller to `/check-email` exactly
as an ordinary request does, and mails a *notice* instead: "sign-in links
are disabled for this account; use your passkey; if this wasn't you, nothing
has happened." Audited `magic_link_refused_policy`.

The identical response is deliberate. If a passkey-only account answered a
sign-in request differently from any other address, the difference would be
an oracle for which accounts have hardened themselves. The only thing that
differs is which email arrives, and only the mailbox sees that.

**Turning it on requires at least one active passkey**, enforced by the
server and not merely by a disabled button -- a policy that can lock its
owner out is not a security control. The control lives on `/me/settings`.

**Rescue:** an owner or admin resets the policy to `any` over
`IdentityAdminMsg` (`ResetSignInPolicyRequest`), audited
`sign_in_policy_reset_by_admin`. The reset is one-directional by design:
there is no admin path to turn `passkey_only` ON for somebody else, because
that would let an admin lock a colleague out of their own account and
nothing operational needs it. It is the same trust level as issuing an
enrolment token for that user, which admins can already do.

### What this does not cover

**An enrolment or recovery link pasted into a mail to a shared mailbox
enrols whoever opens it.** They are shown to the issuing admin and never
emailed for that reason -- `adminops` returns the URL once, in the reply,
and stores only a digest. Nothing stops a human copying one into a message;
if that happens, the consequence is a permanent credential for any recipient.
Enrolment tokens and the owner recovery key are the designed "I lost my
passkey" routes and are deliberately unaffected by `passkey_only`.

## Sessions a user can see and end

Every session the identity service mints has a `v1:identity:authSession`
row, including the first-party browser-cookie session (`source=oidc_cookie`)
-- which for most of its life had none, and so could be neither listed nor
revoked (memql#4303).

- `authSessionsForSelf` (`dsl/identity/queries.memql`) is the self-scoped
  read: `userId==actor.userId`, no arguments, live rows only, and a shape
  that carries **no token hashes**.
- `/me/devices` renders it server-side, marks the current session, and
  offers per-session and revoke-all sign-out.
- `MyAccessResult.session_id` names the session behind the current
  connection so a client can mark "this device" without decoding the JWT.

**Every new session mails the account** -- when, from where, and with what
(memql#4305). There is no action link in that message, deliberately: an
unauthenticated revoke link mailed to a shared mailbox is a
denial-of-service handle for anybody who can read the mailbox. The copy says
to sign in and revoke from the profile page. Refresh rotations never send
it; only a genuinely new session does. Audited
`sign_in_notification_sent`.

## Cockpit Settings: My Access

The Cockpit's Settings tab includes a **MY ACCESS** panel showing the
caller's own identity: user id, primary email, cluster-wide role, and the
current session id. The data comes from a dedicated gRPC message
(`MyAccessMsg` / `MyAccessResult`, `component/grpc/my_access_handler.go`).
There is no per-partition grant list to show any more -- the proto's
`partitions` field is `reserved` (`component/grpc/memql.proto`), and a role
is the only access-relevant fact a `v1:identity:user` row carries (see
"Role spectrum," above).

## Granting access

A cluster-wide role is set by an owner or admin over `IdentityAdminMsg`
(`Service.SetUserRole`, `component/identity/adminops/adminops.go`), audited
as `user_role_changed`. The admin screens that call it live in the MemQL
portal, not the identity binary's own web app -- `/admin/*` on the identity
binary itself is now just its sign-in pages plus an `/admin/` root that
answers `410 Gone`. There is no separate partition-grant mutation to run:
setting the role is the whole of it.

## Out of scope (deferred)

- **Per-concept ACL beyond `@rowAuthz`.** Today access is at the granularity
  of a construct's declared tier (`owned` / `granted` / `admin` / `public`)
  or, for an undeclared concept, whatever its own filters happen to gate on
  -- see [per-row-authz-audit.md](per-row-authz-audit.md) for what's
  declared today and what remains undeclared.
- **Writer-vs-reader enforcement beyond the ExecuteQuery handler.**
  A `reader` issuing a mutation over `ExecuteQueryMsg` is now refused
  with `PermissionDenied` by the coarse data-plane capability gate
  (`component/grpc/data_capability_gate.go`, memql#3179): the handler
  resolves the caller's role and asks
  `auth.Capable(role, "create", "data")` before the engine sees the
  query. That gate is **partial by construction** -- it sits at the
  handler layer, so it covers `ExecuteQueryMsg` and nothing else, and
  its complete residual-bypass set is enumerated with reasons in
  `dataPlaneGateExemptions` (same file):
  - **Guest streams are explicitly exempt.** A guest's real
    authorization dimension is the invitation itself, not the cluster
    role -- the `reader` on a guest stream is a placeholder. (The
    gate's own source comment in `data_capability_gate.go` still
    describes this as "invitation scope plus the partition grant";
    there is no partition grant any more -- #56 retired it, and the
    `partitionId` field the comment is echoing is now just a product
    routing hint on the invitation row, not an access grant.) Guest
    participation necessarily rides
    `ExecuteQueryMsg` (the dedicated guest message types cover only the
    invite/join lifecycle), so gating it would break guest chat. The
    exemption keys off the guest claim, never off the role.
  - **`CallToolMsg` is not gated at all.** It can reach mutation-backed
    tools, but that surface is also driven by machine credentials
    (`class="voice_agent"` JWTs carry no role claim and resolve to the
    reader fallback), so gating it would refuse the live voice path.

  Constructs reached in-process -- automations, the logic runner, the
  planner loop, node bootstrap -- are likewise not covered, by design.
  It is also the COARSE half only: it answers "may this actor write at
  all", never "which rows".
- **Identity-merge UI.** If the same human ends up with two users
  (different emails), there's no merge tool. Avoid by using
  `primaryEmail` as the dedup key at registration.

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
