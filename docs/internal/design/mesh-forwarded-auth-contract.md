# The mesh forwarded-auth contract

**Status:** shipped (memql#3205, carrying memql#2876 and absorbing memql#2814's
"design it once for all forwards").

A forwarded request either **proves** its authority or is **refused**. The
receiver never infers safety from an absent field.

---

## The defect this replaces

`component/grpc/ai_forward.go`'s receiver attached the caller's **claims** and
never an **AccessContext**:

```go
// Reconstruct auth context so worker-side ACLs work.
ctx = auth.ContextWithForwardedClaims(ctx, req.GetAuth())
```

`ContextWithForwardedClaims` sets `TokenInfo` and `Claims` only. Every engine
actor surface reads `auth.AccessFromContext` — `resolveActorPath`, the spec
evaluator, mutation templates, the result-cache policy, the automation actor
envelope. So under the deny-on-nil default (memql#2801) `actor.userId` resolved
to `""`, and any actor-gated construct executed on the worker side of a
BFF → worker hop silently returned **zero rows**, or wrote a row stamped
`createdBy: ""`. The comment named exactly what did not work.

This is memql#2814's defect on a **live** path: every BFF → Voice and
BFF → Agent forward goes through it, including `handleCallTool` and
`handleAgentGenerateTurn`.

## Why the obvious fix was refused

The prior attempt carried the badge grant's `class` / `role_ceiling` across the
hop as two **optional** claims, then attached an AccessContext via the direct
path's `ensureAccess`. It was rejected as a **net security regression**, for
reasons worth keeping written down:

1. **The source was wrong.** `forwardedAuthClaims` read `s.stream.Context()`. A
   gRPC stream context is fixed at **stream-open**, and badge grants arrive
   **mid-stream via `RotateAuth`**, which swaps `s.access` / `s.identity` / the
   expiry stamp without touching the context, because it cannot. Measured, with
   the operator's stored role `owner` and a terminal ceiling `reader`:

   ```
   DIRECT     role="reader"  isClusterOwner=false
   FORWARDED  role="owner"   isClusterOwner=true
   ```

   Before that change the forwarded path had no AccessContext at all, so
   `isClusterOwner` was `false`. Attaching one without a reliable ceiling
   **created** a reachable cluster-owner escalation.

2. **The shape was wrong, whatever the source.** Two *optional* claims whose
   absence is indistinguishable from "no badge" cannot carry a ceiling. The
   receiver cannot detect the loss.

## The contract

### The carrier

A **mandatory, typed** `ForwardedAuthority` message on `AiForwardRequest`
(`component/node/node.proto`), carrying the producer's **already-resolved,
already-clamped decision** — not the inputs to one:

| field | meaning |
|---|---|
| `contract_version` | `"v1"`; anything else is refused |
| `principal_kind` | USER / SYSTEM; the zero value is always refused |
| `subject` / `primary_email` / `role` / `identity_id` | the resolved `AccessContext`, field for field. `role` is already clamped |
| `credential_class` | **never empty.** "No badge" is the value `"user"` |
| `role_ceiling` | non-empty **iff** class is `badge` |
| `expires_at_unix` | non-zero for a class that expires mid-stream |
| `origin_node_id` / `origin_node_type` / `asserted_at_unix` | audit only, never an input to a decision |

It lives on the **node-layer** message deliberately. `MemqlClientMessage` is the
client-facing wire type where a browser can set any field;
`AiForwardRequest` is reachable only behind the `class="node"` stream
interceptor.

The `auth` claims map is **kept**, with its job narrowed to `createdBy`
attribution (`ActorFromContext` → `TokenInfo`; a mutation with no actor
hard-fails). It is **derived** from the assertion and cross-checked on receipt,
so the two carriers cannot drift.

### The source

`streamSession` records `credentialClass` / `credentialCeiling` alongside
`badgeExpiresAt`, stamped by the same two paths and under the same
`accessMu`: lazily from the stream-open claims, and re-stamped by
`handleRotateAuth` **inside the critical section that swaps `s.access`**. The
producer reads the session, never the context.

`forwardedPrincipal()`'s lock discipline is load-bearing: `ensureAccess`
acquires `accessMu` itself, so it is called *before* the lock; the credential
stamp and `s.access` are then read in **one** critical section, so a concurrent
rotation cannot be observed half-applied (a pre-rotate role beside a post-rotate
ceiling would be refused by the proof-of-clamp — a self-inflicted outage).

### The verifier

`auth.VerifyForwardedAuthority` is the single gateway from wire to
`AccessContext`. Every rule fails closed, each with its own sentinel so a log
line, a metric label and a test assertion name the same failure:

| rule | sentinel |
|---|---|
| version ≠ `v1` | `ErrForwardUnsupportedContract` |
| no principal kind | `ErrForwardMissingPrincipalKind` |
| no subject | `ErrForwardMissingSubject` |
| **empty credential class** | `ErrForwardMissingClass` |
| unknown credential class | `ErrForwardUnknownClass` |
| invalid role | `ErrForwardInvalidRole` |
| SYSTEM asserting more than reader | `ErrForwardSystemRoleTooHigh` |
| SYSTEM naming a canonical user id | `ErrForwardSystemImpersonatesUser` |
| USER asserting the system class | `ErrForwardUserClaimsSystemClass` |
| badge with no ceiling | `ErrForwardBadgeMissingCeiling` |
| badge with no expiry | `ErrForwardBadgeMissingExpiry` |
| **`RoleAtMost(role, ceiling) != role`** | `ErrForwardCeilingNotApplied` |
| ceiling on a class that will not enforce one | `ErrForwardStrayCeiling` |
| past expiry | `ErrForwardAuthorityExpired` |

The class set is **closed**: an unknown class is refused rather than treated as
an ordinary user session, so adding a credential type is a loud change instead
of one that lands in the most permissive bucket.

`RoleAtMost` is the exact function the producer's clamp ran
(`applyBadgeRoleCeiling`), so re-running it is a true **proof of application**
rather than a restatement of the claim. It inherits that function's coarseness
by construction — `RoleLevel` maps admin and developer to one level, so an admin
under a developer ceiling passes, identically to the direct path.

On success, and only on success, the receiver binds the assertion verbatim. **No
DB. No `LoadFromClaims`. No `FallbackFromClaims`. No `userByIdSystem` keyed by a
wire-supplied subject.**

### What this buys — and what it does not

The mesh wire is **already peer-authenticated**: only a `class="node"` JWT may
open `NodeService.Stream`. This contract is therefore **not** authentication of
the producer and cannot defend against a fully compromised mesh node — such a
node can assert anything. Overclaiming here is how the last attempt got its
threat model wrong. What it buys:

1. **Completeness** — a producer that forgets to establish authority fails
   loudly instead of silently escalating.
2. **Integrity** — the receiver re-runs the clamp and refuses on disagreement.
3. **Independent expiry** — enforced on the receiver, not inherited from a
   producer's possibly-stale session state.
4. **One resolution path** — `FallbackFromClaims`, which lifts `role` straight
   off a map, is unreachable from wire-supplied input on the receiver. It
   remains reachable on the **producer**, where its input is the verifier's own
   output.
5. **No per-message DB round trip.** Each `AiForwardRequest` builds a fresh
   `streamSession`, so re-resolving would have run `userByIdSystem` **per
   message** — including once per audio chunk on the streaming-transcription
   path. The receiver now performs zero identity-resolver calls; the one
   remaining read is `ensureAccess` on the producer, session-cached for the life
   of the client stream.

## The refusal shape

This is the part that can break a running turn, so it is decided carefully.

`sendForwardError` hardcodes `Done: true`; `AiForwardRouter.Dispatch` calls
`cleanupInflight` on done; `cleanupInflight` closes the response channel. A
**continuation shares the parent turn's `request_id`**. So answering a refused
continuation at all ends the parent turn while the agent keeps running — the
live breakage the epic identified (pausing a Plan in cluster mode would kill the
turn it was pausing). Even `done=false` does not help: both consumers treat a
`QueryError` as fatal for the turn.

Therefore:

- **opener** → answered terminally, so the caller gets an error, not a hang;
- **continuation** → **dropped**, logged and counted, never answered.

"Answered terminally" only helps if the producer *relays* the answer. `proxyAI`
does (`relayForwardedResponses`). `proxyAIStream` drained the forward channel
and threw everything away, because under the substrate cutover the streamed
content arrives out-of-band and that channel normally carries only the terminal
— so a refusal vanished and `consumeTokenStream` blocked forever on a substrate
stream the worker never opened, with no timeout. That path has no self-healing:
streaming chat sends no continuation, so nothing later trips the `HasInflight`
check the transcribe sibling recovers on. It now forwards a `QueryError` to the
client, preserving the receiver's code (`PermissionDenied` vs
`Unauthenticated`) rather than flattening it.

The class is decided by the **receiver's own payload-class table**, not by the
producer's `continuation` bool. The wire flag is a fallback for the one case
where the table cannot be consulted — an envelope that will not parse — and a
cross-check otherwise. A mislabelled opener would hang a client forever; a
mislabelled continuation would re-create the parent-turn kill.

Dropping is an **availability trade, deliberately taken**: a badge expiring
mid-transcription stops the transcript with no error frame rather than tearing
down the turn. That matches `badgeGate`, which also only gates.

## Producers

Six live call sites across three binaries. `Forward` / `ForwardContinuation`
take a single `auth.ForwardedPrincipal` carrying both the assertion and its
derived claims, so **shipping claims without an authority is inexpressible at
the call site** rather than merely discouraged.

| producer | principal |
|---|---|
| BFF `proxyAI` | USER, from the session |
| BFF `proxyAIStream` | USER, from the session |
| BFF transcribe Chunk / End | USER, from the session |
| cognition → agent turn | SYSTEM |
| cognition client-tool relay | SYSTEM (was a documented `nil`) |
| planner dispatch + preempt | SYSTEM (preempt was a documented `nil`) |

Two notes on the SYSTEM hops:

- **Role is `reader`, not `writer`.** `RoleLevel` ranks writer(2) **above**
  reader(3) — lower is more privileged. These hops send the invalid string
  `"system"` today, which `IsValidRole` rejects and `FallbackFromClaims` clamps
  to reader, so reader is the no-widening choice and writer would have *granted*
  them more than they have ever had.
- **The subject must not look like a canonical user id.** Downstream owner
  resolution keys on the `v1:identity:user:` prefix, so a system assertion
  wearing a user id would be adopted as that user's owner id. Keeping the
  system shape means the agent replier falls through to hint-based owner
  resolution exactly as it does now.

Cognition's "prefer the originating user's identity" branch was **deleted as
dead code**: `handleUtteranceForCognition` roots its context at
`contextWithSystemActor(context.Background())` and nothing reintroduces the
inbound stream's principal, so it could not fire. Its test passed only by
hand-building a context and feeding it straight to the builder.

## Rollout

Producers and receivers are separate Deployments with `RollingUpdate`, so a
mixed window exists during a deploy:

- **new producer → old receiver:** works. The old receiver ignores the new
  fields and reads `auth` (retained) exactly as before.
- **old producer → new receiver:** **refused** (`forward_authority_missing`) for
  the length of the rollout. AI forwards fail with a typed `PermissionDenied`.

This ships as **one release with no feature flag**, per CLAUDE.md's
pre-release rule (no compat shims, no "keep working while we migrate" layers). A
`MEMQL_MESH_AUTHORITY_ENFORCE`-style break-glass was considered and rejected: a
default-on toggle that can be set false re-creates exactly the "absence reads as
safe" shape this contract exists to kill. The window fails **closed**, which is
the correct direction.

Refusals are counted on the **`node_forward` metrics surface**, deliberately
distinct from `node` — that one means the mesh transport interceptor rejected a
node token and has an alert watching it for a token storm. A forwarded-authority
refusal is a different event with a different remedy (usually version skew), and
folding the two would fire that alert on it.

## Coverage

| what | where | runs |
|---|---|---|
| verifier rule table | `component/auth/forward_authority_test.go` | anywhere |
| **the SOURCE**, through a real `handleRotateAuth` rotation | `component/grpc/forwarded_authority_source_test.go` | anywhere |
| expiry parity with `badgeGate` | same file | anywhere |
| refusal shapes (opener terminal / continuation dropped) | `component/grpc/ai_forward_refusal_test.go` | anywhere |
| **parent turn survives a refused continuation** | `component/grpc/ai_forward_parent_stream_test.go` | anywhere |
| payload-class drift gate | `component/grpc/ai_forward_refusal_test.go` | anywhere |
| planner preempt carries an acceptable authority | `integrations/planner/preempt_test.go` | anywhere |
| cognition turn principal is SYSTEM | `integrations/cognition/agent_forward_authclaims_test.go` | anywhere |
| **the actual cross-node hop** | `test/clustere2e/forwarded_actor_test.go` | **live 2-replica cluster only** |

The source test carries the control that makes it about the source: it asserts
the **stream context carries no `role_ceiling`**, so a ceiling appearing on the
assertion cannot have come from there. Without that control the test cannot
distinguish the two sources — which is precisely how the previous attempt's test
passed against the defect it was written for.

Three tests were **deleted** with the carrier they covered
(`WithForwardedAuthorityContext`). They were not merely obsolete: one asserted
"the packed map contains these two strings" without ever calling the function
that sourced them, so it passed against the defect; another pinned as *correct*
the absence of those keys for a non-badge session — i.e. the exact property that
makes an unstated ceiling undetectable.

## Known gaps

- **The durable client-tool return path is not covered.**
  `component/node/client_tool_rpc.go` (`ClientToolResultClient.Deliver`)
  superseded the `ForwardContinuation` leg this change fixes, resumes a parked
  agent tool loop across replicas, and carries **no authority at all**. It is a
  different transport with a different message and is out of scope here; it
  wants the same treatment.
- **`WorkbenchForwardRequest`** declares an `auth` map that no producer fills
  and no receiver reads. It should adopt this message when
  `MEMQL_WORKBENCH_REMOTE` ships.
- **Authority expansion is real and intended.** A forwarded owner session now
  binds owner on the receiving node, where before it bound nothing. That is the
  fix; it is also a genuine increase in what worker-side DSL can do, and the
  admin-gated constructs in the embedded tree (`actor.isClusterOwner`) are now
  reachable there. With `MEMQL_IDENTITY_ENABLED=false` the local-dev principal
  legitimately asserts owner, named as `credential_class="local_dev"` so it
  stays greppable rather than looking like an ordinary user.
- **`identity_id` is always empty** — neither `LoadFromClaims` nor
  `FallbackFromClaims` populates `AccessContext.IdentityId`. Carried rather than
  dropped so the mesh keeps parity with the direct path if that changes.
