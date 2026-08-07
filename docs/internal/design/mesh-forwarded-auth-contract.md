# Mesh forwarded-auth contract

**Status:** accepted, implemented in memql#3205
**Supersedes:** the parked design request on memql#2814 and memql#2876
**Applies to:** `AiForwardRequest` on `NodeService.Stream` — every BFF -> worker
and cognition -> agent hop

---

## The problem this replaces

A forwarded request used to carry the caller's **claims** as a
`map<string, string>`. The receiver rebuilt a `TokenInfo` from that map and
attached it to the context. It never built an `AccessContext`, so every engine
actor surface (`resolveActorPath`, the spec evaluator, mutation templates, the
automation actor envelope) saw no actor: `actor.userId` resolved to `""` and
`actor.isClusterOwner` to `false`. Actor-gated DSL executed on the worker side
returned zero rows, or wrote rows stamped `createdBy: ""`.

The obvious repair — carry `class` + `role_ceiling` too, and re-run
`applyBadgeRoleCeiling` on the worker — was attempted and reverted. It is worse
than the bug it fixes, for a reason that generalizes:

> **A receiver cannot safely re-derive a decision from inputs it cannot prove it
> received in full.**

Two optional claims whose absence is indistinguishable from "not a badge
session" cannot carry a ceiling, however they are sourced. An absent `class`
reads as "ordinary session", the worker resolves the user's *unclamped* stored
role, and attaching an `AccessContext` on top of that converts a zero-rows bug
into a reachable cluster-owner escalation.

## The contract

**The sender forwards the DECISION, not the inputs to the decision.**

The BFF has already resolved the caller: `ensureAccess` loaded the user row,
`applyBadgeRoleCeiling` clamped the role, `handleRotateAuth` swapped in whatever
arrived mid-stream. That resolved `AccessContext` is the answer. Forwarding it
directly means the worker has nothing to re-derive, so there is nothing for it
to get wrong.

Everything else follows from that one move:

| Property | How it falls out |
|---|---|
| The ceiling is always applied | The BFF applied it. The worker never sees `class` or `role_ceiling` and has no code path that could skip clamping. |
| `FallbackFromClaims` is unreachable on the mesh | The worker never calls `LoadFromClaims`, so it never falls through to the fallback. |
| No sender-asserted role | The wire carries no claims map. A malicious `sub`/`role` pair is not *rejected* — it is unrepresentable. |
| No per-message DB round-trip | The worker seeds `sess.access` from the carried decision, so `ensureAccess` returns without touching `userByIdSystem`. |

### The carrier

`ForwardedAuthority` is a **sub-message**, not a set of scalar fields. proto3 has
no `required`, and a singular scalar has no presence — an absent `kind` and a
present `""` are identical bytes. A message field has presence unconditionally,
so `authority == nil` is a distinguishable state, and the values it carries are
indivisible: a ceiling cannot arrive without its expiry.

```proto
message ForwardedAuthority {
  string kind           = 1;  // "user" | "badge" | "system" | "internal"
  string user_id        = 2;  // resolved AccessContext.UserId
  string primary_email  = 3;
  string role           = 4;  // POST-ceiling. Never re-clamped downstream.
  int64  badge_exp_unix = 5;  // required and enforced iff kind == "badge"
  bool   local_dev      = 6;  // provenance marker, no authorization meaning
}
```

`AiForwardRequest.auth` (the old `map<string, string>`) is **reserved**, not
deprecated-in-place. Keeping both fields would leave a request with claims and
no authority constructible, which makes the refusal a convention rather than a
structural property. Reserving it also makes the compiler, not the author's
memory, find every producer.

### The four kinds

Every forward asserts exactly one. "No badge" is a value; there is no absence to
misread.

- **`user`** — an ordinary end-user principal. `user_id` / `primary_email` /
  `role` come from the sender's resolved `AccessContext`. `badge_exp_unix` is
  zero.
- **`badge`** — a shared-terminal operator grant (memql#2513). Identical to
  `user`, except `badge_exp_unix` is mandatory and non-zero, and the worker
  refuses the request once that instant has passed. The direct stream gates
  every envelope through `badgeGate`; this is the forwarded path's equivalent,
  and it is why `exp` is on the wire at all — without it a walked-away kiosk's
  expired grant would be rejected on the direct stream and honored on every
  forwarded `AiChat` / `CallTool`.
- **`system`** — a named service principal for work with no end user behind it
  (greet-on-join, post-approval Plan dispatch). The role is **pinned by the
  receiver** and never named by the sender. The subject *is* named by the
  sender, but only from a **receiver-held allowlist**
  (`auth.systemActorAllowlist`): the planner and cognition deliberately carry
  different ids so a stamped row can be attributed to the integration that
  produced it, and collapsing them into one constant would preserve safety
  while destroying that audit distinction. So the contract *constrains* the
  subject rather than fixing it — the sender chooses among known actors, and
  cannot invent one.

  This path genuinely needs an actor: memql#1107 is the bug where it had none
  and the agent's taskstamp Stamper failed with "no actor found in context".
- **`internal`** — no principal at all. The receiver binds **no** actor, and
  actor-gated DSL reached on this path fails closed exactly as it does today.
  Used by the two producers that legitimately have no caller: the planner's
  `AgentPreemptTurn` (flips a pause flag, keyed by request id) and cognition's
  client-tool relay (`ClientToolResult`, resolved against a waiter keyed by
  call id). Neither persists anything.

### Receiver rules

```
authority == nil                       -> REFUSE   (the whole point)
kind not one of the four               -> REFUSE
kind in {user,badge} && no user_id     -> REFUSE
kind in {user,badge} && role invalid   -> REFUSE   (never silently clamp)
kind == "badge" && exp == 0            -> REFUSE   (unenforceable ceiling)
kind == "badge" && exp <= now          -> REFUSE   (expired grant)
kind == "system" && user_id not in the allowlist -> REFUSE
kind == "system"                       -> accept; role pinned by the receiver
kind == "internal"                     -> accept; bind no actor
```

On accept for a principal-bearing kind, the receiver builds:

- an `auth.AccessContext` directly from the carried fields, and seeds
  `sess.access` / `sess.accessLoaded` so `ensureAccess` is a cache hit;
- an `auth.TokenInfo` so `UserIdentityFromContext` works — the many
  `component/memql/*_validation.go` actor validators reachable on the worker
  resolve through it;
- a claims map **synthesized from the decision** (`sub`, `email`, `role`), so
  claims-reading consumers keep working. It deliberately omits `class` and
  `role_ceiling`: the role is already final and must never be re-clamped.

### Where the sender reads the authority

`s.ensureAccess()` / `s.currentAccess()`, never `s.stream.Context()`.

A gRPC stream's context is fixed at stream-open. Badge grants arrive
**mid-stream** via `RotateAuth`, and `handleRotateAuth` swaps `s.access`,
`s.identity`, and `s.badgeExpiresAt` without touching the context, because it
cannot. Sourced from the context, a mid-stream badge session forwards the
pre-rotation state. Measured through the real chain, operator stored role
`owner`, terminal ceiling `reader`:

```
DIRECT     role="reader"  isClusterOwner=false
FORWARDED  role="owner"   isClusterOwner=true      <- context-sourced
FORWARDED  role="reader"  isClusterOwner=false     <- session-sourced (correct)
```

This is the most load-bearing line in the contract, and it is why a test that
exercises the packing function without driving a real `handleRotateAuth`
rotation proves nothing — it passes against the defect.

### Multi-hop: re-assert, do not rebuild

`ContextWithForwardedAuthority` stashes the validated authority on the context,
and a node that forwards onward re-asserts it verbatim
(`auth.ForwardedAuthorityFromContext`).

This matters for the two-hop chain BFF -> cognition -> agent, where
`handleCallTool` and `handleAgentGenerateTurn` run. Rebuilding the authority
from the `AccessContext` at hop two would preserve the clamped role but silently
drop `BadgeExpires`, leaving the final node unable to enforce expiry.
Re-forwarding the original keeps kind, ceiling and expiry intact across any
number of hops.

For a co-resident origin with no inbound forward, cognition falls back to
building from the context's `AccessContext`. Its role is already post-ceiling
(`applyBadgeRoleCeiling` runs inside `LoadFromClaims`), so the ceiling is
provably applied; what is not carried is the expiry instant, because an
`AccessContext` does not hold one. The exposure is bounded — the originating
stream's `badgeGate` has already admitted that envelope, and the forward happens
within its handling.

## Refusals must not be terminal on a continuation

`sendForwardError` set `Done: true` unconditionally. `AiForwardRouter.Dispatch`
calls `cleanupInflight` on `done`, which closes the response channel for that
`requestId` — and continuations (`AiTranscribeStreamChunk` / `End`,
`ClientToolResult`, `AgentPreemptTurn`) share the **parent turn's** request id.
So a refusal on a continuation would kill the parent turn's stream while the
agent kept running: pausing a Plan in cluster mode would blank the user's
in-flight reply.

Refusals on continuation payload types send `Done: false`. Only a refusal on a
stream-initiating envelope is terminal.

## Deploy consequence

The refusal is fail-closed, so a mixed-version mesh refuses: an old BFF sending
no authority to a new worker is rejected across the board. Producers and
receivers roll together. This is the correct direction for a security fix — the
alternative is a window in which absence is tolerated, which is the defect — but
it is a deploy requirement, not a preference.

Per the pre-release rule in CLAUDE.md there is no compatibility shim and no
tolerate-then-enforce phase.

## Known gap

`WorkbenchForwardRequest.auth` is a third `map<string, string>` auth carrier in
`node.proto`. Nothing populates it and nothing reads it — the only reference in
the tree is the generated getter. It is the same class as the `QueryForward`
field removed in memql#2814: a dead auth carrier that reads as load-bearing.
Out of scope here (memql#3205 names two producers); tracked separately.

## Related

memql#2876 (carried here) · memql#2814 (same class, dead path) · memql#216
(the client-facing stream fix this path was left out of) · memql#2801
(deny-on-nil) · memql#2513 (badge grants + ceiling) · memql#1107 (the
system-actor fallback) · memql#902/#906 (`AgentPreemptTurn`)
