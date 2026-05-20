# Caller envelope contract

> The `caller.*` fields are the **single source of authorization
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
through dotted-path fields on the `caller` namespace. Author-side:

```memql
@caller
shape callerActor {
  caller.userId
  caller.role
  caller.identityId
  caller.isClusterOwner
  caller.partitions
  caller.partition
  caller.now
}
```

Engine-side, dotted paths route through
`component/memql/executor.go:resolveCallerReferences` →
`resolveCallerPath`.

## Field reference

| Field | Type | Meaning | Per-identity-shape behavior |
|---|---|---|---|
| `caller.userId` | string | Canonical `v1:identity:user.id` for the user behind the request | **user (magic-link / OAuth)**: the user's id. **PAT**: the user who owns the PAT. **worker token**: the user who issued the worker token. **guest invite**: empty -- guests have no `userId`. **system**: empty -- system actor; `caller.role == "system"`. |
| `caller.role` | string | Cluster-wide role: `owner` / `admin` / `writer` / `reader` (plus `system` for the seed-materializer / automation actor and `guest` for guest invites) | Re-fetched from `v1:identity:user.role` at each request -- a role demotion takes effect at the next call, not on the next token refresh. |
| `caller.identityId` | string | `v1:identity:identity.id` of the credential the request was authenticated with (distinct from `userId`: one user can own many credentials) | **user (magic-link)**: the `magic_link` identity row's id. **PAT**: the `api_key` identity row's id. **worker token**: the `worker_token` identity row's id. |
| `caller.isClusterOwner` | bool | True iff `caller.userId` is the registered cluster owner | Re-resolved per request via the cluster-settings lookup. |
| `caller.primaryEmail` | string | The user's primary email | Empty for guest / system / worker shapes. |
| `caller.partitions` | []string | Partition names the caller can read | **Going away in #56.** When partitioning is removed, this field disappears from the envelope. |
| `caller.partition` | string | The active request's partition | **Going away in #56.** Same reason. |
| `caller.now` | string | RFC3339 timestamp captured at request start | Same for every field reference within the request -- the clock is captured once. |
| `caller.config.<key>` | string | Allow-listed config value (whitelist in `component/config/policy_exposable.go`) | Reading an unlisted key is a parse-time error. |

## Identity shapes

The envelope exposes the same field names regardless of how the
caller authenticated, but the underlying behavior differs:

| Shape | How it authenticates | `caller.userId` | `caller.role` | `caller.identityId` | Surface restrictions |
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

- **Not a permission set.** The envelope says *who* the caller is,
  not *what they can do*. The DSL composes permissions on top of
  the envelope: policies + specs gate behavior on `caller.role`,
  `caller.isClusterOwner`, `caller.userId == payload.ownerUserId`,
  etc.
- **Not mutable inside a request.** Every field is captured once at
  request entry. Engine code MUST NOT update the envelope mid-call.
- **Not a substitute for storage-level enforcement** today.
  `PartitionACL` middleware enforces partition isolation at the
  storage layer; the envelope is the *authorization* contract. After
  #56 lands and partitioning goes away, the envelope is the only
  layer.

## Author rules

1. **Always read through `caller.*`** -- never inspect
   `auth.Identity` directly from Go code that's making an authz
   decision. (Engine plumbing that constructs the envelope is the
   sole exception.)
2. **`@caller` shapes carry the binding.** Specs that want to gate
   on caller state declare `@shape("callerActor")` (or a more
   specific caller shape). The post-load validator catches missing
   bindings.
3. **`caller.now` is the only clock.** Never use Go's `time.Now()`
   in a DSL evaluator path; that's a source of test-flake +
   replay-skew.

## Anti-patterns

- **Reaching around the envelope for "fast path" reads.** Don't
  read `tokenInfo.Subject` directly to compose an authz decision --
  read `caller.userId` through the envelope. Today the auth
  context's Subject and the envelope's userId match; reaching
  around it lets them silently drift.
- **Composing partial envelopes.** Don't build a partial caller
  envelope for a sub-call (e.g. "this query runs as the user but
  with admin role"). If a privilege escalation is intended, use the
  `delegation` concept; otherwise the envelope reflects the
  authenticated caller exactly as-is.
- **Caching the envelope across requests.** Per-request only.

## What's going away in #56

- `caller.partitions` — the partition-list field.
- `caller.partition` — the active-partition field.
- `PartitionACL` storage-layer enforcement.

The envelope's remaining fields stay the same; they're the
authorization surface for the post-partition world.

## Related issues

- #54 — per-row authorization audit (uses the envelope to gate
  every user-scoped query / mutation)
- #56 — removes partitioning (deletes the partition fields)
