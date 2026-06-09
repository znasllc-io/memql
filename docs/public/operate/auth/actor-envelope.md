---
title: Actor envelope contract
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Actor envelope contract

> The `actor.*` fields are the **single source of authorization
> input** to the DSL. Every policy, spec, query filter, or mutation
> body that gates on the authenticated actor MUST read through the
> envelope; nothing in the engine should reach around it.

## What the envelope is

When a request hits the gRPC stream, the auth interceptor
(`component/grpc/auth_session_middleware.go`) validates the JWT (or
the API-key bearer / Worker token / Guest invite, per the identity
shape) and builds an `auth.Identity` struct. The engine binds that
identity onto the request context; resolver code reads it back via
`auth.UserIdentityFromContext`.

The DSL surface is **read-only** access to that same identity
through dotted-path fields on the `actor` namespace. Author-side:

```memql
@actor
shape actorEnvelope {
  actor.userId
  actor.role
  actor.identityId
  actor.isClusterOwner
  actor.now
}
```

Engine-side, dotted paths route through
`component/memql/executor.go:resolveCallerReferences` →
`resolveCallerPath`. (The internal Go types still carry the
historical `CallerReference` / `resolveCaller*` names; that is
implementation detail behind the `actor.` author surface.
[#221](https://github.com/znasllc-io/memql/issues/221) renamed the
DSL surface; renaming the internal types is a follow-up candidate.)

## Field reference

| Field | Type | Meaning | Per-identity-shape behavior |
|---|---|---|---|
| `actor.userId` | string | Canonical `v1:identity:user.id` for the user behind the request | **user (magic-link / OAuth)**: the user's id. **PAT**: the user who owns the PAT. **worker token**: the user who issued the worker token. **guest invite**: empty -- guests have no `userId`. **system**: empty -- system actor; `actor.role == "system"`. |
| `actor.role` | string | Cluster-wide role: `owner` / `admin` / `writer` / `reader` (plus `system` for the seed-materializer / automation actor and `guest` for guest invites) | Re-fetched from `v1:identity:user.role` at each request -- a role demotion takes effect at the next call, not on the next token refresh. |
| `actor.identityId` | string | `v1:identity:identity.id` of the credential the request was authenticated with (distinct from `userId`: one user can own many credentials) | **user (magic-link)**: the `magic_link` identity row's id. **PAT**: the `api_key` identity row's id. **worker token**: the `worker_token` identity row's id. |
| `actor.isClusterOwner` | bool | True iff `actor.userId` is the registered cluster owner | Re-resolved per request via the cluster-settings lookup. |
| `actor.primaryEmail` | string | The user's primary email | Empty for guest / system / worker shapes. |
| `actor.now` | string | RFC3339 timestamp captured at request start | Same for every field reference within the request -- the clock is captured once. |
| `actor.config.<key>` | string | Allow-listed config value (whitelist in `component/config/policy_exposable.go`) | Reading an unlisted key is a parse-time error. |

(The pre-#56 envelope also exposed `actor.partitions` + `actor.partition`. Those are retired and the parser rejects them with a migration hint -- partitioning came out in #56 phase 5.)

## Identity shapes

The envelope exposes the same field names regardless of how the
caller authenticated, but the underlying behavior differs:

| Shape | How it authenticates | `actor.userId` | `actor.role` | `actor.identityId` | Surface restrictions |
|---|---|---|---|---|---|
| **user (magic-link)** | JWT verified against JWKS | user's id | user's role | the `magic_link` identity | full stream |
| **user (PAT)** | API key bearer | owner's id | owner's role | the `api_key` identity | full stream |
| **worker token** | `Authorization: Worker mql_wkr_<token>` | registering user's id | `system` | the `worker_token` identity | only `WorkerService.Stream`; everything else 401 |
| **guest invite** | `Authorization: Guest <token>` or `?guest_token=<token>` on WS | empty | `guest` | empty | only the explicitly-scoped reads on the invitation |
| **system** | `systemActorContext(ctx)` -- internal-only | empty | `system` | empty | seed materializer, automations -- never reachable from a network request |

The auth interceptor (`component/grpc/auth_session_middleware.go`)
+ the worker / guest interceptors (`worker_stream_interceptor.go` /
`guest_stream_interceptor.go`) build the right envelope based on
which token shape arrived. The engine never re-classifies; it just
reads through the envelope.

## What the envelope is NOT

- **Not a permission set.** The envelope says *who* the actor is,
  not *what they can do*. The DSL composes permissions on top of
  the envelope: policies + specs gate behavior on `actor.role`,
  `actor.isClusterOwner`, `actor.userId == payload.ownerUserId`,
  etc.
- **Not mutable inside a request.** Every field is captured once at
  request entry. Engine code MUST NOT update the envelope mid-call.
- **Not a substitute for storage-level enforcement** today. After
  #56 lands and partitioning went away, the envelope is the only
  authorization layer.

## Author rules

1. **Always read through `actor.*`** -- never inspect
   `auth.Identity` directly from Go code that's making an authz
   decision. (Engine plumbing that constructs the envelope is the
   sole exception.)
2. **`@actor` shapes carry the binding.** Specs that want to gate
   on actor state declare `@shape("actorEnvelope")` (or a more
   specific actor shape). The post-load validator catches missing
   bindings.
3. **`actor.now` is the only clock.** Never use Go's `time.Now()`
   in a DSL evaluator path; that's a source of test-flake +
   replay-skew.

## Anti-patterns

- **Reaching around the envelope for "fast path" reads.** Don't
  read `tokenInfo.Subject` directly to compose an authz decision --
  read `actor.userId` through the envelope. Today the auth
  context's Subject and the envelope's userId match; reaching
  around it lets them silently drift.
- **Composing partial envelopes.** Don't build a partial actor
  envelope for a sub-call (e.g. "this query runs as the user but
  with admin role"). If a privilege escalation is intended, use the
  `delegation` concept; otherwise the envelope reflects the
  authenticated actor exactly as-is.
- **Caching the envelope across requests.** Per-request only.
- **`caller.X` / `@caller`.** Retired in #221. The parser rejects
  both spellings; the canonical form is `actor.X` / `@actor`.

## Related issues

- #54 — per-row authorization audit (uses the envelope to gate
  every user-scoped query / mutation)
- #56 — removed partitioning (deleted the partition fields)
- #221 — consolidated `caller.` / `actor.` -> `actor.` for one
  canonical auth-context vocabulary across the DSL
