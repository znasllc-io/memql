# Requirements archive -- memql

**This file is temporary. Delete it once the transfer described below is done.**

## What this is

The engineering substance of the 39 issues that were open in `memql` when
the project moved off GitHub-issue tracking. Each entry states a problem, the
evidence for it, the options considered, and what "done" would mean.

The issues themselves were deleted from GitHub. This is the surviving record of
the work they described.

## What was deliberately left out

Everything belonging to the old tracking methodology rather than to the work:
claim locks, session identifiers, workflow-state labels, assignees, review
handoffs, branch and PR pointers, and timestamps. None of it survives the move,
so carrying it across would only be noise.

Labels are kept **only** where they classify the work itself -- `bug`,
`security`, `dsl`, `engine`, `auth`, `reliability`, `architecture`,
`enhancement`, `hygiene`, `area:infra`.

The original issue numbers are kept for one reason: the write-ups reference each
other heavily (`Refs #2989`, `split from #2960`, `the same class as #2814`), and
stripping them would break those cross-references. Treat a number as a name, not
as a live GitHub link -- the issues are gone.

## What has to happen next

These are **pending transfer into epics**. Nothing here is resolved. Each entry
needs to be re-expressed under the new methodology, or consciously dropped.

Where a later investigation changed what the work should be, that is recorded
under **Later findings** on the entry -- read it before re-filing, because in
several cases it supersedes the original framing entirely.

## When to delete this file

Once every entry below has been moved into an epic or deliberately abandoned.

This file is migration scaffolding. Left in the tree afterwards it becomes a
stale second backlog competing with the real one; deleted before the transfer it
takes the backlog with it.

---

## Contents

- **#2802** -- The data resource has no request-path enforcement -- Capable(role, VerbRead, ResourceData), CanRead and CanWrite have zero callers
- **#2876** -- bug(grpc): AiForward worker path attaches claims but no AccessContext, so forwarded DSL runs with no actor (reachable in production)
- **#3038** -- @default is never applied on insert; decide whether it should be
- **#3052** -- Four pre-existing go/clear-text-logging alerts in executor_mutation.go are re-attributed to every unrelated PR
- **#3059** -- raw insert() bypasses accept/stamp entirely, so a declared owner tier can still be forged on most concepts
- **#3063** -- The three per-user credential lists still take a caller-supplied userId with no caller check
- **#3067** -- seeder.go writes straight to the store, bypassing executeWrite and every write guard (empty input set today)
- **#3075** -- Two workflows both publish a required check named `scan`, so the required set does not say what it means
- **#3076** -- rowAuthz Phase 3: enforce the declared tier on the read path (measured no-op: 0 would-narrow, 0 undecidable)
- **#3077** -- rowAuthz: gate the undeclared population with a shrink-only list, so 168 unmeasured constructs (48 of them identity) cannot stay invisible
- **#3079** -- rowAuthz Phase 5: enforce the declared owner tier on update/delete in the engine (120 of 215 mutations take a caller-supplied target)
- **#3082** -- dsl/deployment has no live canonicalId call, so the remapped-ambient path is only fixture-tested (#3026 DoD item 5, second clause)
- **#3084** -- The signature-concept ambient path still uses the LAST path segment and has no scope check, so a nested file can bind a foreign namespace
- **#3089** -- GrammarVersion has not been bumped since 2026-07-21 despite repeated grammar narrowings, so the durable-rehydration stamp guard never fires
- **#3093** -- The call-graph automation-condition rules are unreachable from CheckTree, so 33 authored automations are analysed by nothing
- **#3095** -- scripts/cidb: the db-tests coverage gate is one-directional -- deleting a TestMain leaves it green
- **#3096** -- dbtest.EnsureSchema poisons MEMQL_DATABASE_DSN before pinging, turning its own failure into every downstream test's
- **#3099** -- Several MemQL statement builders still render strings with %q, which the lexer refuses (the #3035 defect, elsewhere)
- **#3101** -- event_payload_args: the block-structure scan still runs on raw source, so a `}` in a literal truncates an automation and a commented-out automation is rewritten
- **#3105** -- declared_usage_validator's struct-form keyword loop is dead code carrying the retired 'mutation' keyword
- **#3108** -- The OpenAI embedding client puts the verbatim upstream response body into its error, and that error is logged at Warn by two engine call sites
- **#3111** -- @secret is not redacted by the automation args binder, which also writes the value to a WARN log
- **#3112** -- @secret is not redacted by concept payload JSON-schema validation (@minimum / @maximum / @format)
- **#3113** -- Nothing in the DSL tree carries @secret, so #3036's redaction protects zero fields
- **#3114** -- agentRole slug is documented as canonical but nothing enforces uniqueness, so the shadow row can still be minted
- **#3116** -- blankCommentsAndStrings ends a string at a newline and claims the lexer does too; the lexer accepts multi-line literals
- **#3117** -- @secret is not redacted by validateToolArgs, which WARN-logs the entire args map and runs before the covered validator
- **#3120** -- The one-byte-lookback string-scan bug survives in 11 more places after #3045; nothing stops a twelfth
- **#3123** -- A branch-less @variant(discriminator=...) is silently dropped at every depth
- **#3124** -- Value constraints that are inert for the element's type (@pattern on []int, @minLength on []object) build silently
- **#3128** -- A trailing slash exempts /inbound/{source}/ from the verifier but does not match the mux route
- **#3129** -- revoke/updateAgentAuthorization take a caller-supplied target with no owner check, and the concept declares no tier to enforce one
- **#3131** -- LoadFromRows keys the specialist registry by roleSlug last-wins over an unscoped agent set
- **#3135** -- undeclaredRowAuthzConstructs has no slot for the issue reference its own failure message demands
- **#3138** -- updateAgentAuthorization splats a caller payload, so userId is still caller-writable after #3081
- **#3143** -- Four documents claim the tool-args validator is auto-registered for EVERY enabled query and mutation; it is not, and #3127's narrowing is also false
- **#3145** -- v1:identity:authCode stores the OAuth code in cleartext when codeHash already serves the lookup
- **#3148** -- EnsureSchema still amplifies its own failure under MEMQL_REQUIRE_DB, which is what the db-tests lane sets
- **#3149** -- 14 test files hardcode the database DSN while dbtest.DSN() exists to resolve it, so credential drift stays possible

---


## #2802 -- The data resource has no request-path enforcement -- Capable(role, VerbRead, ResourceData), CanRead and CanWrite have zero callers

**Classification:** `architecture`, `auth`, `enhancement`, `security`

The consolidated RBAC model (epic #2062) defines five verbs across seven resource types and resolves them through one primitive:

```go
// Capable is the canonical, single server-side authorization primitive ...
// Every enforcement decision on the request path -- handlers, executor guards,
// the migrated Can* adapters -- resolves through THIS function
func Capable(role Role, verb, resourceType string) bool
```
— component/auth/rbac_model.go:167-169

Three of those resource types have request-path callers. `data` does not.

### Evidence

| resource | wired? | where |
|---|---|---|
| `construct` | yes | `auth.CanAuthor` → component/grpc/authoring_handlers.go:267, component/mcp/tier.go:100-104 |
| `principal` | yes | `auth.CanCreatePrincipal` / `GovernPrincipal` → integrations/rbac/capabilities.go:84-89, reached from DSL via the `rbacCanCreatePrincipal` / `rbacGovernPrincipal` builtins and the `governanceCan*Principal` logics |
| `data` | **no** | `auth.CanRead` (rbac.go:245) and `auth.CanWrite` (rbac.go:224) have **zero** non-test callers |

`grep -rn "CanRead(\|CanWrite(" --include=*.go .` returns only definitions and tests (`rbac_test.go`, `rbac_enforcement_test.go`, `rbac_migration_conformance_test.go`).

Executing a named query or mutation consults no capability. `evaluateExpressionSetWithContext` (component/memql/executor.go:198) compiles the author's filter and runs it; the only actor-derived term in the emitted SQL is whatever the author wrote in the filter.

### Why it matters

The model is load-tested and internally consistent — `TestRBACEnforcementWiringLoadsEndToEnd` (component/memql/rbac_enforcement_wiring_test.go) asserts the concepts, governance logics and builtins all load — but that test verifies the chain is **present**, not that anything **calls** it on the data path. E1.6 (#2074, closed 2026-06-23) scoped "wire enforcement through specs + executor + handlers across the request path"; the handler and governance halves landed, the executor half for `ResourceData` did not.

The practical consequence: `reader` and `writer` are indistinguishable at the data plane. A `reader` can execute any mutation the DSL exposes, because nothing checks `create`/`update`/`delete` on `data` before the write.

### Considerations

This is the **coarse** half of authorization — "may this role touch rows of this kind at all" — and it is genuinely useful on its own: it is a cheap, pure, node-local lookup with no per-row cost, and it would close the reader/writer gap immediately.

It is explicitly **not** sufficient for row scoping. `Capable(role, VerbRead, ResourceData)` answers "may this actor read call records," never "which call records." Those need a per-concept ownership rule; see the row-authz design issue. Wiring this one first is still worthwhile — it is small, additive, and independent — but it should not be mistaken for closing the row-visibility gap.

Open questions for the implementation:
- Where does the check go — the executor (one chokepoint, catches every caller including automations and the tool loop) or the gRPC handlers (closer to the request, but several entry points)? The executor is the only place that sees *all* traffic.
- How do non-user actors resolve a role — automations, the logic runner, node-to-node forwards, `class="service_account"` and `class="voice_agent"` tokens? Each needs a defined role or an explicit system bypass, and the bypass needs to be narrow.
- Does `resourceType` stay a single `data` bucket, or does it become per-concept? A single bucket ships now; per-concept is the natural extension and wants the concept→resourceType mapping the row-authz design would introduce anyway.

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

**(1)**

Structural context: #2803 (decide: concept-declared row authorization). This issue stands on its own and is worth fixing regardless of that ruling — #2799/#2800/#2801 are Phase 0 prerequisites there, and #2802 is independently shippable.

**(2)**

## Parked — the gap is real and confirmed, but it cannot be closed correctly without two decisions and a test surface I do not have.

I claimed this, investigated, ran a probe against the real executor, and stopped without shipping. No PR. Below is the research so whoever takes it does not have to redo it.

### Confirmed: the gap is exactly as reported

`grep -rn "CanRead(\|CanWrite(" --include=*.go .` returns **only the definitions** (`component/auth/rbac.go:224,245`) and tests. Zero request-path callers. `reader` and `writer` are indistinguishable at the data plane.

### Finding 1 — the mesh-forward path is DEAD, not a live bypass (good news, with a trap)

I expected `handleQueryForward` (`component/node/stream_handler.go:609`, `Execute(context.Background(), ...)`) to be a live hole that would make any executor-level check bypassable by routing across the mesh. It is not, and #2814 is right: there are **zero producers**. The only `QueryForward{}` in the tree is the generated protobuf reset at `component/node/gen/node.pb.go:1433`, and `RegisterConceptOwnership` has **zero non-test registrations**.

So the receive side is unreachable machinery today. But it is a **latent trap for this issue specifically**: wire enforcement now, and the day someone lands a `QueryForward` producer, every forwarded query arrives with no `AccessContext` and the check either denies it (breaking cross-node reads) or waves it through (a bypass that looks enforced). Whichever fix lands here should land with #2814, or with a guard that makes the actor-less forward path fail loudly rather than silently pick a side.

### Finding 2 — one live actor-less executor call, and it is node bootstrap

`component/node/bootstrap.go:105`:

```go
result, err := ctx.Engine.Execute(context.Background(), "concept==v1:cluster:node")
```

This one *is* reachable, on every node start. A fail-closed check at the executor breaks node bootstrap. That is the "each needs a defined role or an explicit system bypass" question from the issue body, and it has to be answered before any executor-level check can land — it is not a detail that can be deferred.

### Finding 3 — the role inventory, for the non-user-actor question

Every internal path that builds an `AccessContext` today, with the role it picks:

| path | role |
|---|---|
| `component/memql/authoring_{promote,demote}_durable.go`, `authoring_session.go` | `RoleWriter` (hardcoded) |
| `component/memql/authoring_rearm.go` | `RoleWriter` and `RoleOwner` |
| `component/automations/authored_scheduler.go` | `RoleWriter` (hardcoded) |
| `integrations/planner/reactive_loop_helpers.go` | `RoleWriter` (hardcoded) |
| `app/mcp_automation_runner.go` | `RoleWriter` (hardcoded) |
| `component/mcp/server.go`, `tier.go` | from config `ActingRole` |
| `component/auth/identity_resolver.go` | from the user row / claims, falling back to `RoleReader` |
| `component/identity/admin/deployments.go` | from claims |

So automations, the planner loop and the authoring paths would all pass a `data`-write check as `writer` — which is probably intended, but it means the check does **not** constrain internal machinery at all. That is worth deciding deliberately rather than discovering later.

### Finding 4 — this change cannot be validated locally, which changes how it should be built

I ran the actual probe: a fail-closed `Capable(role, VerbRead, ResourceData)` check inserted at the executor chokepoint (`component/memql/engine.go`, after `parseWithFunctions`), compiled, present at runtime.

```
go test ./component/memql/   ok  54.292s   0 failures, 0 denials
go test ./dsl/               ok   1.706s   0 failures, 0 denials
```

**Zero denials.** Not because the check is harmless — because those suites do not drive `Execute` with real queries. The suites that do are `db-tests` and `mcp-conformance`, both of which need postgres, and postgres is not available on this machine (`pg_isready` → down).

That is the practical blocker. An authorization change on the request path whose blast radius is invisible to every locally-runnable test should not be developed blind and pushed to find out in CI. Whoever takes this wants a live database (`make up`) so `db-tests` runs, or the change wants to land behind a flag with the enforcement path exercised by a new test that actually executes queries.

### What is still an open decision (unchanged from the issue body)

1. **Executor vs handlers.** The executor is the only chokepoint that sees all traffic — which is also why it sees automations, the logic runner and the tool loop, each of which then needs a role or a bypass (findings 2 and 3). Handlers see only user requests, so a handler-level check needs no bypass at all, but it is partial by construction and leaves the DSL-callable surfaces uncovered. Picking one *is* the design decision; I am not going to make it unilaterally on an auth path.
2. **Single `data` bucket vs per-concept.** The issue notes per-concept wants the concept→resourceType mapping the row-authz design (#2803) would introduce, so this partly depends on that decision.

### Recommended sequencing

#2803 (row-authz design) is the decision issue and should go first or alongside — the issue body itself says a single-bucket check "should not be mistaken for closing the row-visibility gap", and #2803 is where the concept→resource mapping gets settled. If the coarse check is wanted sooner, the smallest safe slice is the **handler** layer, because it needs no system bypass; the executor version wants #2814 and the bootstrap decision resolved first.

**(3)**

Park reason, unchanged and still holding: the gap is confirmed, but closing it needs two architectural decisions — what happens to the actor-less `Execute` at node bootstrap (`component/node/bootstrap.go:105`), and how this lands alongside #2814's forward path — plus a test surface that did not exist at park time. That is a judgement about the work, not a stale moment, and nothing here re-opens it.

Cleared on the repo owner's explicit instruction during a sweep.

**(4)**

Still parked, and correctly so. Recording one change to the park's premise that postdates the last sweep, so whoever un-parks does not re-derive a constraint that no longer exists.

**The #2814 coupling is dissolved.** The park (2026-07-26) said "whichever fix lands here should land with #2814, or with a guard that makes the actor-less forward path fail loudly rather than silently pick a side." #2814 closed **2026-07-28T13:54Z** via PR #2924, which **removed the QueryForward machinery** rather than arming it -- almost two hours after the previous sweep (`rf842e5`, 11:48Z) commented here, so no comment in this thread reflects it.

Verified against the tree just now: `grep -rn "QueryForward\|handleQueryForward" --include=*.go .` returns nothing outside generated protobuf. There is no forwarded-query path left to coordinate with, so that half of the park is moot.

**The other half still holds, and it is the one that matters.** `component/node/bootstrap.go:105` still reads:

```go
result, err := ctx.Engine.Execute(context.Background(), "concept==v1:cluster:node")
```

A fail-closed check at the executor still breaks node bootstrap on every node start, and "each needs a defined role or an explicit system bypass" is still an architectural decision rather than a stale moment.

**The issue's own premise is also unchanged.** `grep -rn "CanRead(\|CanWrite(" --include=*.go .` still returns only the two definitions at `component/auth/rbac.go:224,245` -- zero request-path callers. `reader` and `writer` remain indistinguishable at the data plane.

So: one of the two blockers cleared, one stands, and the park is a judgement about the work rather than a moment that passed.


---

## #2876 -- bug(grpc): AiForward worker path attaches claims but no AccessContext, so forwarded DSL runs with no actor (reachable in production)

**Classification:** `security`

## What

The AI-forward worker path attaches the caller's **claims** but never builds an **AccessContext**, so DSL executed on the worker side of a BFF -> worker hop sees no actor.

`component/grpc/ai_forward.go:349`:

```go
// Reconstruct auth context so worker-side ACLs work.
ctx = auth.ContextWithForwardedClaims(ctx, req.GetAuth())
```

`ContextWithForwardedClaims` (`component/auth/forward.go:93-106`) sets only `TokenInfoContextKey` and `ClaimsContextKey`. It never sets `AccessContextKey`.

Every engine actor surface reads `auth.AccessFromContext`:

- `component/memql/executor.go:357` (`resolveActorPath`)
- `component/memql/spec_evaluator.go:81`
- `component/memql/mutation_templates.go:631`
- `component/memql/result_cache_policy.go:163`
- `component/automations/actor_envelope_binding.go:25`

The only `auth.ContextWithAccess` call in `component/grpc` is `server.go:1520`, on the direct `handleQuery` path. The AiForward dispatch never reaches it.

## Why it matters

Under the deny-on-nil default (#2801), `actor.userId` resolves to `""` and `actor.isClusterOwner` to `false`, so any actor-gated construct executed on the worker side silently returns **zero rows** — or writes a row stamped `createdBy: ""` on the mutation path. Comment at `:348` says "so worker-side ACLs work"; the ACLs it names are exactly what does not work.

This is the same defect as #2814 on `handleQueryForward`, with one important difference: **#2814 was unreachable (zero producers), and this path is live in production.** Every BFF -> Voice / BFF -> Agent forward goes through it — `handleAiChat`, `handleCallTool`, `handleAgentGenerateTurn`.

The repo already documents this exact failure at `component/grpc/server.go:1512-1520`:

> Without this the engine's `resolveActorPath` sees no AccessContext, `actor.userId` resolves to `""`, and every self-scoped query/mutation silently no-ops (zero rows). See memql#216.

So #216 was fixed on the client-facing stream and the mesh-forward path was left behind.

## Scope note

Surfaced while reviewing the #2814 fix (PR #2864). Deliberately not folded into that PR, which covers the unreachable `QueryForward` path — this one is reachable and deserves its own change plus its own cross-node test.

## Suggested direction

Resolve and attach an AccessContext on the worker side before dispatch, the way `ensureAccess` does: try `identityResolver.LoadFromClaims` first so the role comes from the user row, not the forwarded claim.

Note the design question #2814 had to settle: `auth.FallbackFromClaims` takes `role` straight from the claims map, and `IsClusterOwner()` is just `Role == RoleOwner`, so a claims-derived fallback across a mesh hop lets the sending side assert authority. On `QueryForward` that was resolved by requiring a DB-resolved principal and refusing otherwise. This path currently substitutes a synthetic `identity.Subject = "forwarded"` when the subject is empty (`ai_forward.go:351-353`), so it needs a deliberate decision about what an unidentified AI forward should do — probably not "execute as nobody", and definitely not "execute as owner".

Worth a cluster-e2e case that forwards an actor-gated construct BFF -> worker and asserts the receiving node binds the same actor, per the CLAUDE.md rule that a green single-node test is a false signal for cross-node behaviour.

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

Parking. My attempt is on `issue-2876-aiforward-access-context` (PR #2891, now a **draft** — do not merge it) and the tests there are reusable, but the change as written is a **net security regression** and pre-merge review caught it. Recording exactly why, because the failure is instructive and it converges with #2814.

## What I built, and why it looked right

Two halves: carry the badge grant's `class` / `role_ceiling` across the hop so `applyBadgeRoleCeiling` can be reapplied, then attach an AccessContext via the direct path's own `ensureAccess` (DB-authoritative role). The mechanism genuinely works — with those two claims on the wire, the worker resolves `reader` / `isClusterOwner=false` against a stored-`owner` row. I verified that.

## Why it is wrong anyway

**1. The ceiling claims are not visible where I read them.** `forwardedAuthClaims` reads `s.stream.Context()`. A gRPC stream's context is fixed at **stream-open**, and badge grants arrive **mid-stream via `RotateAuth`** — this repo says so itself at `component/grpc/server.go:1090` (*"badge identities arrive mid-stream via RotateAuth where interceptors cannot see them"*). `handleRotateAuth` swaps `s.access`, `s.identity` and `s.badgeExpiresAt` (`server.go:1270-1281`) and never touches the stream context, because it cannot.

Measured through the real chain (real `IssueBadgeGrantToken` + verifier + `handleRotateAuth` + `ensureAccess`), operator stored role `owner`, terminal ceiling `reader`:

```
DIRECT     role="reader"  isClusterOwner=false
FORWARDED  role="owner"   isClusterOwner=true
```

That is character-for-character the measurement my own code comment quotes as the thing it prevents.

**And it is worse than not fixing it.** Before this change the forwarded path had no AccessContext at all, so `isClusterOwner` was `false` under the #2801 deny-on-nil default. Attaching one without a reliable ceiling *creates* a reachable cluster-owner escalation on the mesh. An operator on a kiosk with a `reader` ceiling, chatting → BFF → agent node, binds an owner actor for the whole turn.

The correct source was on the session the whole time: `s.currentAccess().Role` is the post-rotate **clamped** role. Reading ceiling inputs from an immutable context, when the entire point of badge is that it mutates mid-stream, is the structural error.

**2. I patched one of two producers.** `integrations/cognition/agent_forward.go:236-248` still projects through `UserIdentity` and drops the ceiling — on the BFF → cognition → agent hop, which is where `handleCallTool` / `handleAgentGenerateTurn` run. So even a stream-open badge (the case finding 1 does not cover) escalates as soon as the turn crosses that hop.

**3. The wire-asserted-owner fallback is untouched.** `ensureAccess` falls through to `auth.FallbackFromClaims` (`server.go:1036`) on *any* `LoadFromClaims` failure — non-canonical subject, missing row, nil resolver, or a transient engine error. That lifts `role` straight off the wire and `IsClusterOwner()` is `Role == RoleOwner`:

```
sub="attacker" role="owner" -> isClusterOwner=true
```

This is #2814's confirmed round-2 defect, and my PR comment claims *"the role is the row's, not the wire's"* — true only on the success branch. My new refusal checks that the sender named **someone**; it never checks the sender was entitled to name them. Wrong property.

**4. The refusal breaks two live producers.** `integrations/planner/plan_execution.go:269` passes `nil` claims for `AgentPreemptTurn` (with a comment saying that is fine), and `integrations/cognition/client_tool_relay.go:164` documents `authClaims stays nil`. Both payload types are in the forward dispatch switch. Worse, `sendForwardError` sets `Done: true`, and `AiForwardRouter.Dispatch` calls `cleanupInflight` on done — which closes the **parent turn's** response channel, since continuations share the parent `requestId`. Net effect: pausing a Plan in cluster mode kills the parent turn's stream while the agent keeps running.

**5. Badge expiry is not enforced, and my design cannot enforce it.** The direct path gates every envelope through `badgeGate`; `HandleForwardedRequest` does not. My allowlist carries `class` and `role_ceiling` but **not `exp`**, so the worker could not check expiry even if it tried. A walked-away kiosk's expired grant is rejected on the direct stream and honored on every forwarded `AiChat` / `CallTool`.

**6. Per-message DB round-trip.** Each `AiForwardRequest` builds a fresh `streamSession` with an empty access cache, so `ensureAccess` → `userByIdSystem` runs per message — including once per audio chunk on the streaming-transcription path (`ai_transcribe_stream.go:203`). The direct path caches for the life of the stream; the forward path structurally cannot, because the session is per-message.

**7. My own test committed the error its file header warns about.** `TestForwardedAuthorityContextCarriesTheBadgeCeiling` hand-builds a claims map and asserts two keys survive packing. It never calls `forwardedAuthClaims()`, so it never touches the `s.stream.Context()` read that is finding 1, and nothing asserts a **resolved clamped role on the worker**. Asserting "the map contains two strings" as a proxy for "the ceiling is enforced" is the same class of mistake as trusting `UserIdentityFromContext` — which that header explicitly calls out. A test built from a real `handleRotateAuth` rotation fails immediately.

## What this means for the design

#2814's park comment is right, and this is the confirmation it predicted: *"an absent `class` is indistinguishable from 'not a badge session', so the receiver cannot detect the loss."* Carrying two **optional** claims and trusting their absence is exactly that failure mode. Finding 1 is it firing.

So this wants **Option A from #2814**: a forwarded-auth contract where the receiver **refuses when it cannot prove the ceiling was applied**, rather than inferring safety from absence. Concretely that means the carrier is mandatory and explicit (a "no badge" assertion is a *value*, not a missing key), it is sourced from the session's post-rotate access rather than the stream context, it carries `exp` so expiry is enforceable, it covers **both** producers, and it replaces `FallbackFromClaims` on the mesh with a refusal. That is a change to live mesh auth and it should be designed once for all three forwards — which is what #2814 asked for and what I should have started from.

## What is still true and still worth fixing

The original defect is real and unfixed: `ContextWithForwardedClaims` sets TokenInfo + Claims only, every engine actor surface reads `auth.AccessFromContext`, so worker-side DSL runs with no actor — zero rows on reads, `createdBy: ""` on writes, on a path that is live in production. The comment saying *"so worker-side ACLs work"* is still wrong.

Reusable from the branch: `TestForwardedClaimsAloneLeaveTheEngineWithNoActor` pins the defect itself and is correct as-is. `WithForwardedAuthorityContext` is a reasonable allowlist primitive and its allowlist test is sound — it is the *source* feeding it and the *optionality* that are wrong, not the packing.

Related: #2814 (same class, dead path, parked), #2888 (MCP `run_automation` trusted-automation selection), #2889 (client-origin stamp coverage).


---

## #3038 -- @default is never applied on insert; decide whether it should be

**Classification:** `dsl`, `engine`

## RULING (triage 2026-08-04, REVISED by owner 2026-08-05) -- narrowed predicate, enforced as a CONFORMANCE TEST

`@default` stays **documentation-only** -- not applied on insert, not retired. The check ships as
a **test in `dsl/conformance_test.go`, NOT as a load-time boot gate**, and it fires on a narrowed
predicate.

**Two things changed from the original ruling, both driven by measurement.**

### 1. The predicate is narrowed

A `@default` is a finding only when ALL of:

- the concept field is **optional** (no `!`) -- a required arg has no omitted case, so the
  annotation is documenting the conventional value, not silently doing nothing;
- **no** mutation binding that concept writes the field with `??`;
- **no** mutation binding that concept writes the field as a literal or other stamped value.

The original rule ("no mutation coalesces it") flagged two legitimate patterns it cannot
distinguish from the defect: a mutation that stamps a literal (`insert { done: false }` -- the
default IS realised) and a required argument (`registrationMode` -- no omitted case exists).

### 2. It is a conformance test, not a boot gate

**Because narrowing does not get the population to a handful.** Measured on `main`:

| set | count |
|---|---|
| fields carrying `@default` | 203 |
| required (no omitted case) | 13 |
| optional | 190 |
| optional & never coalesced -- *approximately the original rule* | 74 |
| **optional & never written by any mutation -- the narrowed set** | **51** |

By domain: cognition 17, identity 14, agents 10, planner 5, observability 3, telephony 1,
router 1. (Regex measurement, generous in the "covered" direction and imprecise on enum types --
treat as +/-5. The direction is not in doubt.)

51 is still exemption-list sized, and **a boot gate that reds on 51 fields is precisely what this
issue family exists to avoid.** A conformance test:

- **cannot refuse boot on a legitimate bundle topology** -- which resolves the design question
  below rather than answering it;
- is this repo's **established pattern** for tree-wide DSL authoring rules
  (`TestFilterIntrinsicsUseRowNamespace`, `TestSortKeysUseRowNamespace`,
  `TestNoRetiredOperatorForms`, `dsl/no_coalesce_longhand_test.go`);
- still converts a silent no-op into a caught mistake at authoring time, which is the failure the
  original ruling identified: **the author never finds out**.

**The accepted cost:** it covers the in-repo tree only, not domains mounted at runtime via
`MEMQL_DSL_PATH`. A product bundle's own `@default` mistakes are not caught. That is a deliberate
trade against the false-positive-refuses-boot risk, and the docs must say so.

### The ~51 are part of this story, triaged once

- [ ] Report the narrowed set **in this thread** before changing any of them.
- [ ] Each member is then fixed, stamped with a mutation write, or has the annotation removed --
      a human judgement per field, not a blanket rewrite. Do NOT encode them as a permanent
      exemption list; that is the outcome this ruling exists to avoid.

**Why not "apply it on insert"** (unchanged from the original ruling). It buys a second problem:
an update-path decision (does a default re-apply when a field is cleared?), and `parseDefaultValue`
(`concept_parser.go:720`) has **no datetime branch** -- a `datetime` field silently accepts a bool
default today, so parsing would have to be trustworthy before application could be. Two problems
where one will do.

**Why not retire it** (unchanged). The emitted `default` keyword is legitimately consumed as
documentation by form generators, sense hover and the generated SDK.

**Dependency satisfied.** #2960 / PR #3039 has landed -- `TestDefaultIsEmittedButNeverApplied` and
the corrected `_concept.memql` section 8 are on `main`. The original "do not start until it lands"
no longer applies.

### The runtime-mounted-domain question is RESOLVED, not deferred

The original ruling asked the builder to state and defend the gate's scope against the
`MEMQL_DSL_PATH` case, warning that "a gate that fails boot on a legitimate bundle topology is
strictly worse than the silent no-op it replaces". **Moving off the boot path answers it:** the
test runs over the in-repo tree, per concept, checking only mutations bound to that concept. No
boot-time false positive is possible because there is no boot-time check.

- [ ] Document that runtime-mounted domains are **not** covered, in the same place the check is
      described. A reader must not infer tree-wide coverage.

---

Split from #2960, which recorded the decision to document `@default` as **never applied** rather than change the write path inside a documentation story.

## Where it stands

A concept-field `@default("value")` reaches the emitted schema as the JSON Schema `default` keyword — which is annotation, not behaviour: JSON Schema validators do not fill it, and `Concept.Create` clones, validates and marshals the payload verbatim. So the field is simply absent when a caller omits it.

CLAUDE.md already states this ("a concept-field `@default` is NOT a substitute — it is never applied on insert either (memql#2960), so `??` is the only mechanism"), and #2960 made the reference agree. `TestDefaultIsEmittedButNeverApplied` pins it.

The working mechanism is the `??` null-coalescing operator in the mutation body:

```memql
insert {
  status: args.status ?? "draft"
}
```

## The parsing half is also narrower than it looked

`parseDefaultValue` (`concept_parser.go:720`) coerces `"true"`/`"false"` to bools, integers to int64 and floats to float64, and returns everything else as a string. There is **no datetime branch**, so a `datetime` field takes a bool default without complaint — the reference used to claim RFC3339 strings were parsed as datetimes. That sentence is corrected and the behaviour is pinned.

## The question

Three defensible answers:

1. **Apply it on insert.** Closest to what the annotation looks like it does. Means touching the write path, and needs a decision about update: does a default re-apply when a field is cleared?
2. **Retire it, and rely on `??`.** One mechanism instead of two. Costs the schema its `default` keyword, which consumers (form generators, sense hover, the generated SDK) may legitimately want as documentation.
3. **Keep it as documentation-only, as now**, and consider whether the DSL should *reject* `@default` on a field whose mutations never coalesce it — turning a silent no-op into a load error.

The second half of option 3 may be the highest value for the least risk: the failure today is not that the default is missing, it is that the author never finds out.

## Definition of done

- [ ] A recorded decision between the three.
- [ ] If applied: a test driving an insert that omits the field and asserting the stored row carries the value — not that the schema carries the `default` keyword. An answer for the update path too.
- [ ] If retired: `@default` removed from the tree and rejected at load with a migration hint pointing at `??`.
- [ ] If kept: consider the load-time gate, so a `@default` nothing coalesces is an error rather than a no-op.
- [ ] `dsl/_reference/_concept.memql` section 8, `docs/public/language/attribute-matrix.md` and `reserved.md` follow the outcome; `TestDefaultIsEmittedButNeverApplied` is updated with it.

Refs #2960

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

**(1)**

**Ruled and approved: option 3, including its second half.** `@default` stays documentation-only **and** gains the load-time gate. Recorded at the top of the body.

You called the second half of option 3 "the highest value for the least risk," and that is the ruling — with one addition, below, that I do not think is optional.

**Why not option 1 (apply on insert).** It buys a second problem. It needs an update-path answer (does a default re-apply when a field is cleared?), and `parseDefaultValue` at `concept_parser.go:720` has no datetime branch — a `datetime` field silently takes a bool default today, so the parsing would have to be trustworthy before the application could be. Two problems where one will do.

**Why not option 2 (retire).** The emitted `default` keyword is real documentation that form generators, sense hover and the generated SDK legitimately consume. Retiring pays that to solve what the gate solves for free.

**Blocked by #2960** — `TestDefaultIsEmittedButNeverApplied` and the corrected reference section exist only on PR #3039's head. Stamped on the body.

## The addition, and why I am not waving it through

The gate has a scoping question that decides whether it is safe: a core concept carrying `@default` whose only coalescing mutation lives in a **product bundle mounted at `MEMQL_DSL_PATH`** is invisible to a gate that walks the core tree alone. And this gate sits on the load path, so a false positive **refuses boot** — the same hazard #3024 carries, and the worst outcome available.

That inverts the value proposition if it is got wrong: the current state is a silent no-op, which is bad; a gate that fails boot on a legitimate bundle topology is worse. So I have added two DoD items requiring the scope to be stated and defended, with a test for the runtime-mounted-domain case. Treat that as the design work of this story, not a detail after the regex.

Claimable once #2960 lands.

**(2)**

**The gate as ruled would refuse boot on today's tree with ~85 errors, and a large share of them
would be wrong.** Nothing built; no commits on the branch.

The ruling is: *"a `@default` on a field that no mutation coalesces is a load error naming `??`."*
I measured the population before writing it, because the issue's own scope item warns that "a gate
that fails boot on a legitimate bundle topology is strictly worse than the silent no-op it
replaces". The bundle case turns out to be the *smaller* problem.

## The measurement

Across `dsl/*/concepts.memql`: **206 fields carry `@default`. 85 of them (41%) are not coalesced
by any `??` in any mutation in the tree.**

Uncoalesced by domain: cognition 22, identity 18, agents 11, planner 7, platform 4, observability
4, actions 3, data 3, cluster 2, harness 2, and a tail.

The matcher is deliberately GENEROUS in the "coalesced" direction -- it accepts a `field: ... ??`
written in *any* mutation anywhere, not just one bound to that concept -- so the true uncoalesced
count is **at least** 85, not at most.

## Why this is not simply "85 things to fix"

Two legitimate patterns produce an uncoalesced `@default`, and the rule cannot tell them from the
defect it is aiming at. Both are spot-checked by hand, not inferred from the regex:

**1. The mutation stamps a literal.** `dsl/todos`:

```memql
done  bool  @default("false")      // concepts.memql
...
insert { done: false }             // mutations.memql -- no `??`, and none needed
```

The default IS realised. There is no silent no-op and nothing for the author to learn.

**2. The argument is required.** `dsl/identity/concepts.memql:142`:

```memql
registrationMode  enum("open", "domain_restricted", "invite_only", "waitlist")!  @default("open")
```

The mutation takes `args.registrationMode` as a required enum, so there is no omitted-value case to
default. The `@default` is documentation of the *conventional* value -- which is precisely the role
the ruling wants to preserve when it declines to retire the annotation.

So the rule "no `??` anywhere" flags both, and in neither case is the author's intent silently
doing nothing. Shipping it would either red the tree or need an ~85-entry exemption list -- and an
exemption list of that size is the thing this whole issue family exists to avoid.

## What I did NOT find

I did not find that the ruling is wrong about the underlying defect. A `@default` on an *optional*
field whose mutation neither coalesces nor stamps it genuinely is a silent no-op, and the author
genuinely never finds out. That subset is real and worth a load error. It is just much smaller
than "not coalesced".

## Recommendation

**Narrow the predicate, then build it as ruled.** A `@default` is an error only when ALL of:

- the concept field is **optional** (no `!`) -- a required arg has no omitted case;
- **no** mutation binding that concept writes the field with `??`;
- **no** mutation binding that concept writes the field as a literal or other stamped value.

That is the set where the annotation truly does nothing, it keeps the load error the ruling asked
for, and it needs no exemption list. I would measure that set before building -- if it is small and
each member is a real mistake, the gate is exactly right; if it is still large, that is a different
conversation.

On the **scope question** the issue raised: with the narrowed predicate I would run the gate over
the tree **as loaded**, per concept, and check only mutations bound to that concept. A bundle that
ships a concept must ship its own mutations (the #2617 import discipline already assumes this), so
a core-only boot never needs a bundle's mutation to justify a core concept's `@default`. That
removes the false-positive-on-boot risk the issue flags. I did not want to build on that reasoning
while the predicate itself is unsettled.

## Why I am asking rather than narrowing it myself

The narrowing changes what the ruling asserts -- "no mutation coalesces it" becomes "no mutation
coalesces, stamps, or requires it" -- and 85 findings versus a handful is a different decision
about how much churn this is worth. The issue's scope item explicitly asks the builder to state and
defend the gate's scope before building; I would rather bring the measurement back than pick a
predicate that quietly redefines the ruling.

**(3)**

**Owner ruled: narrow the predicate, and ship it as a conformance test rather than a boot gate.**

The builder's ASK above was right to stop, and its narrowing proposal is adopted. **But the
premise the narrowing rested on does not hold, and that is what moved the check off the boot
path.**

## The measurement the builder said it would want

The builder proposed narrowing and wrote: *"I would measure that set before building -- if it is
small and each member is a real mistake, the gate is exactly right; if it is still large, that is a
different conversation."* I measured it. **It is still large**, so this is that different
conversation.

| set | count |
|---|---|
| fields carrying `@default` | 203 |
| required (no omitted case) | 13 |
| optional | 190 |
| optional & never coalesced -- roughly the original rule | 74 |
| **optional & never written by ANY mutation -- the narrowed set** | **51** |

By domain: cognition 17, identity 14, agents 10, planner 5, observability 3, telephony 1, router 1.

**Caveat, stated because it bears on how much weight this carries:** this is a regex measurement.
It is generous in the "covered" direction (a write of that field name in *any* mutation counts,
not just one bound to that concept, so the true narrowed set is **at least** 51), and it
mis-parses enum-typed fields -- `identity.registrationMode` lands in my optional bucket though the
builder correctly identified it as required. Treat as +/-5. **The direction is not in doubt:
narrowing takes ~74 to ~51, not to a handful.**

## What follows from that

A boot gate reddening on 51 fields needs an exemption list of that size, and an exemption list of
that size is the thing this whole issue family exists to avoid. So the predicate is narrowed **and**
the check moves to `dsl/conformance_test.go`:

- it **cannot refuse boot on a legitimate bundle topology**, which *resolves* the design question
  the original ruling flagged rather than answering it -- there is no boot-time check, so there is
  no boot-time false positive;
- it is this repo's established pattern for tree-wide DSL authoring rules
  (`TestFilterIntrinsicsUseRowNamespace`, `TestSortKeysUseRowNamespace`,
  `TestNoRetiredOperatorForms`, `no_coalesce_longhand_test.go`);
- it still delivers the original ruling's actual goal -- **the author never finds out** becomes the
  author finds out at authoring time.

**The accepted cost, which must be documented:** the in-repo tree only. Domains mounted at runtime
via `MEMQL_DSL_PATH` are not covered, so a product bundle's own `@default` mistakes go uncaught.
That is a deliberate trade against false-positive-refuses-boot.

## Two DoD items that carry the weight

- **Report the narrowed set in this thread BEFORE changing any of it.** My number is a regex
  estimate; the real one comes from the implemented predicate.
- **Then triage the members individually** -- fix, stamp with a mutation write, or drop the
  annotation. A human judgement per field. **Do not encode them as a permanent exemption list.**

**Dependency satisfied:** #2960 / PR #3039 has landed, so `TestDefaultIsEmittedButNeverApplied`
and `_concept.memql` section 8 are on `main`. The "do not start until it lands" stamp is removed.
**This story has no open blocker.**

**(4)**

**Measurement first, before touching any field — as the ruling requires.**

## The corrected count is 66, not 51 — and the shape confirms the ruling's predicate

| set | count |
|---|---|
| fields carrying `@default` | 206 |
| required (no omitted case — carved out) | 14 |
| optional & **stamped** by a bound mutation | 126 |
| **optional & never stamped → FINDINGS** | **66** |

Per domain, mine vs the ruling's regex estimate:

| domain | ruling | measured |
|---|---|---|
| cognition | 17 | 18 |
| identity | 14 | 20 |
| agents | 10 | 12 |
| planner | 5 | 6 |
| observability | 3 | 3 |
| telephony | 1 | 1 |
| router | 1 | 1 |
| actions / data / harness / healing / platform | — | 1 each |

The profile tracks closely enough that I am confident this implements the **intended** predicate rather than a different one — the ruling undercounted (it said +/-5 and "the direction is not in doubt"), it did not mean something else.

## What changed in the predicate, and why it matters

The ruling says a field is covered when a mutation "writes the field with `??`" or "as a literal or other stamped value". Getting that right turned on three write-block forms a naive scan gets wrong, and each moves fields across the line:

- **`accept { a, b, c }`** binds each name to its same-named arg. Caller-supplied — if the caller omits it, nothing is written and the default is **not** realised. NOT covering.
- **bare `args.X` shorthand** inside a write block: same thing. NOT covering.
- **`f: args.X`** with no `??`: plain passthrough. NOT covering.
- **`stamp { f: args.X ?? "v" }`** and any literal / computed value: **covering**.

My first pass treated `accept` and splats as covering and reported 23. That was wrong — none of them realise a default. The 126 "covered" are genuinely stamped.

**Caveat, stated rather than hidden:** 29 of the 66 belong to a concept that some mutation writes with an object splat (`args.payload`). A splat can carry the field at runtime if the caller supplies it, so those 29 are "the default is still never applied" but not "the field is never populated". They are findings under the ruling's predicate; I flag them because the remedy may differ.

## Verified by hand, not just by regex

Four spot-checks against the tree: `participant.isGuest` and `budget.alertSent` appear in **no** mutation at all; `plan.tokenSpent` appears only in an `accept { }` list and an args declaration; `user.theme` appears only inside a comment. All four are genuine.

## The 66

actions/concepts.memql:75  action.kind
    agents/concepts.memql:42  agent.avatar
    agents/concepts.memql:43  agent.lipSync
    agents/concepts.memql:44  agent.vision
    agents/concepts.memql:45  agent.voiceToVoice
    agents/concepts.memql:46  agent.claw
    agents/concepts.memql:74  agent.autoJoin
    agents/concepts.memql:75  agent.greetOnJoin
    agents/concepts.memql:76  agent.interruptionStyle
    agents/concepts.memql:77  agent.speakWhen
    agents/concepts.memql:119  agentAuthorization.action
    agents/concepts.memql:166  agentRole.recommendedGender
    agents/concepts.memql:187  skill.tier
    cognition/concepts.memql:96  participant.isGuest
    cognition/concepts.memql:142  session.avatarEnabled
    cognition/concepts.memql:143  session.visionEnabled
    cognition/concepts.memql:144  session.voiceEnabled
    cognition/concepts.memql:147  session.cameraEnabled
    cognition/concepts.memql:148  session.isSpeaking
    cognition/concepts.memql:149  session.microphoneEnabled
    cognition/concepts.memql:166  state.action
    cognition/concepts.memql:168  state.mentionedAI
    cognition/concepts.memql:170  state.requiresHumanApproval
    cognition/concepts.memql:171  state.sentiment
    cognition/concepts.memql:172  state.shouldRespond
    cognition/concepts.memql:174  state.aiPermission
    cognition/concepts.memql:275  guardrailHealth.belowThresholdCount
    cognition/concepts.memql:276  guardrailHealth.handoffChainCapCount
    cognition/concepts.memql:277  guardrailHealth.noFitNoFallbackCount
    cognition/concepts.memql:278  guardrailHealth.routerEscalatedCount
    cognition/concepts.memql:279  guardrailHealth.routerFallbackCount
    data/concepts.memql:9  log.active
    harness/concepts.memql:143  semanticMemory.kind
    healing/concepts.memql:58  healedOverride.version
    identity/concepts.memql:142  clusterSettings.registrationMode
    identity/concepts.memql:145  clusterSettings.internalDefaultRole
    identity/concepts.memql:273  invitation.kind
    identity/concepts.memql:277  invitation.status
    identity/concepts.memql:338  user.notifications
    identity/concepts.memql:339  user.theme
    identity/concepts.memql:341  user.archiveRetentionDays
    identity/concepts.memql:342  user.dailySpaceEnabled
    identity/concepts.memql:343  user.dailySpaceRolloverAction
    identity/concepts.memql:344  user.cursorTweenMs
    identity/concepts.memql:345  user.takeoverMode
    identity/concepts.memql:346  user.interactivePace
    identity/concepts.memql:347  user.computerUseEnabled
    identity/concepts.memql:348  user.voiceMode
    identity/concepts.memql:355  user.internal
    identity/concepts.memql:359  user.revocationEpoch
    identity/concepts.memql:394  group.maxHumans
    identity/concepts.memql:395  group.maxAgents
    identity/concepts.memql:396  group.active
    identity/concepts.memql:416  accountEntitlement.tier
    observability/concepts.memql:22  codeProfile.level
    observability/concepts.memql:23  codeProfile.sampleRate
    observability/concepts.memql:24  codeProfile.retentionDays
    planner/concepts.memql:22  plan.retryThreshold
    planner/concepts.memql:37  plan.tokenSpent
    planner/concepts.memql:38  plan.tokenAllocatedToChildren
    planner/concepts.memql:39  plan.tokenCapDisabled
    planner/concepts.memql:46  plan.totalPausedMs
    planner/concepts.memql:50  plan.computerUseScope
    platform/concepts.memql:134  inboundRequest.signatureVerified
    router/concepts.memql:18  budget.alertSent
    telephony/concepts.memql:23  number.e911Registered

## Next

Building the predicate above as `TestDefaultIsCoalescedOrStamped` in `dsl/conformance_test.go` (a conformance test, **not** a boot gate), then triaging these 66 individually — never a blanket rewrite, never an exemption list.

**Triage principle I will apply, stated up front so it can be challenged:** removing a `@default` changes **no** behaviour (it is already a no-op), whereas adding a `??` stamp **does** change what a create writes. So a field gets a stamp only where the create path clearly ought to produce that value and its absence is a live defect; otherwise the annotation goes and its information moves into `@description`. I will report the per-field split when it is done.

**(5)**

Q: Should the `@default` gate cover **nested object leaves** (`user.preferences.theme`, `agent.capabilities.avatar`, ...), whose only possible remedy is deleting an annotation the ruling says is legitimately consumed — or should it be scoped to **top-level concept fields**, the only ones a mutation can actually stamp?

## Context

The gate is built and pushed (`0fcb7581`, branch `issue/3038-default-conformance-gate`). It implements the ruled predicate faithfully and is currently RED on **66** fields — the full measurement is in my previous comment, and its per-domain profile tracks the ruling's estimate closely, so the predicate is the intended one.

Then I split those 66 by nesting depth, and that is what I need a decision on:

| | count | can a mutation stamp it? |
|---|---|---|
| **top-level** concept fields | **36** | yes — `f: args.f ?? "v"` |
| **nested** object leaves (depth 2) | **30** | **no** |

The 30 nested ones:

| concept | fields |
|---|---|
| `identity:user.preferences` | 10 — theme, notifications, archiveRetentionDays, dailySpaceEnabled, dailySpaceRolloverAction, cursorTweenMs, takeoverMode, interactivePace, computerUseEnabled, voiceMode |
| `agents:agent` | 9 — capabilities.{avatar, lipSync, vision, voiceToVoice, claw}, triggerBehavior.{autoJoin, greetOnJoin, interruptionStyle, speakWhen} |
| `cognition:session` | 6 — avatarEnabled, visionEnabled, voiceEnabled, cameraEnabled, isSpeaking, microphoneEnabled |
| `cognition:state` | 5 — action, mentionedAI, requiresHumanApproval, sentiment, shouldRespond |

**Why the repo does not settle it.** A mutation writes the PARENT object wholesale (`createAgent` does `accept { capabilities, triggerBehavior }`; `updateUserPreferences` writes `preferences`). There is no write form that stamps a single leaf, so the gate's advertised remedy — *"stamp it in the mutation"* — **does not exist** for these 30. The only remaining remedy is deleting the annotation.

**And that collides with the ruling's own reasoning.** Under *"Why not retire it"* the ruling says the emitted `default` keyword *"is legitimately consumed as documentation by form generators, sense hover and the generated SDK."* Deleting `@default` from all ten `user.preferences` leaves removes exactly the schema documentation a preferences form generator would consume. So for the nested set, the gate as ruled pushes toward doing the thing the ruling declined to do — for the fields where that documentation is most obviously load-bearing.

This is a premise change rather than a detail: the ruling chose a conformance test over a boot gate reasoning about a population of ~51, and 30 of the real population cannot be remediated the way the ruling assumes.

## Options

**A. Scope the gate to top-level concept fields (RECOMMENDED).** Findings drop 66 → 36, every one of which can be either stamped or dropped as the ruling intends. Nested leaves get an explicit, documented carve-out in the same place the gate's scope is described: their `@default` is schema documentation, is consumed as such, and is not claimed to be applied. Costs: the gate does not catch a *new* nested `@default` that someone believes is applied — mitigated by the reference wording, which will say plainly that no `@default` is ever applied at any depth.

**B. Keep all 66 and delete the 30 nested annotations.** Fully consistent — "an annotation that does nothing is caught wherever it is" — but it strips `default` from the SDK/form-generator schema for user preferences and agent capabilities, which the ruling explicitly valued. Also the largest diff, and the least reversible.

**C. Keep all 66 and stamp the nested ones by constructing the parent objects in the create mutations.** Makes the annotations true. But it is a real behaviour change to `createAgent`, session creation and user provisioning — every create would start writing a fully-populated object where it currently writes what the caller passed. Well outside this story, and it needs its own issue.

I recommend **A**, and if you take it I would file the "should nested defaults be applied at all" question as a separate issue rather than fold it in.

## Blocked

The 36 top-level triage is answer-independent and I could have done it, but the gate cannot go green until the nested 30 are resolved one way or the other, so the PR is blocked on this either way.

## State


---

## #3052 -- Four pre-existing go/clear-text-logging alerts in executor_mutation.go are re-attributed to every unrelated PR

**Classification:** `engine`, `security`

Four high-severity `go/clear-text-logging` alerts sit on `component/memql/executor_mutation.go`. They surface on every PR that touches an unrelated part of the tree, reported as *"new alerts in code changed by this pull request"*, which makes them look like each PR's problem when they are not.

## The alerts

| line | sink |
|---|---|
| 1071 | `e.Logger.Warn("harness observation embed failed (recall will skip this row until backfilled)", "id", id, "error", err)` |
| 1076 | `e.Logger.Debug("harness observation embedded for recall", "id", id, "content_len", len(content))` |
| 1112 | `e.Logger.Warn("action intent embed failed (planner search will skip this action until backfilled)", "id", id, "error", err)` |
| 1117 | `e.Logger.Debug("action intent embedded for planner search", "id", id, "intent_len", len(intent))` |

## Why they keep getting attributed to the wrong PR

Found while landing PR #3034 (memql#2957, the inbound-delivery receiver). CodeQL reported 6 alerts there; the author diagnosed 2 as genuinely theirs and flagged these 4 as probably not. Confirmed during review:

- `git diff origin/main...HEAD -- component/memql/executor_mutation.go` on that branch is **empty** — the file is untouched.
- The only route that PR could take into these sinks is the staged row's `id`, and `requestIDFor` is `sha256(source + "\x00" + dedupeKey)` hex-truncated, so **no caller-controlled byte survives into it**.

So the attribution is dataflow reaching pre-existing sinks, not new code. Every future PR that adds a new taint source anywhere upstream will re-report them and burn the same review time.

## What to decide

Whether the alerts are true positives is a separate question from the attribution, and worth answering on its own:

- `id` is a node id. If any concept's ids are caller-derived rather than server-generated, logging them is a genuine leak; if they are always server-minted, these are false positives.
- `err` on the two `Warn` lines comes from the embedding provider. Whether provider errors can quote the embedded CONTENT is the load-bearing question — that content is user text.

Note the same class was just ruled on in memql#2957: the inbound handler stopped logging its engine error because the mutation embeds a webhook body and the parser quotes source text in its errors (`unexpected token after expression: %q`). If the embedding provider does anything similar, these two are the same defect.

## Definition of done

- [ ] Determine whether `err` on lines 1071 and 1112 can carry embedded content. If yes, stop logging it (the memql#2957 pattern: keep the id, drop the error).
- [ ] Determine whether `id` is ever caller-derived at these call sites.
- [ ] Either fix, or dismiss the alerts in the CodeQL UI with the reason recorded — so they stop being re-attributed to unrelated PRs.

CodeQL is **not** a required status check on this repo (required: `ci-required`, `scan`, `Analyze (go)`), so these do not block a merge — but they do make the CodeQL check red on unrelated work, which is how a real alert eventually gets ignored.

Found by the review of PR #3034 (memql#2957).

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

**(1)**

**This issue's premise is wrong, and I am correcting it rather than building on it.**

The title and body say these four `go/clear-text-logging` alerts in `component/memql/executor_mutation.go` are pre-existing and "re-attributed to every unrelated PR". Measured today while landing PR #3034:

- **Absent on `main`.** `repos/.../code-scanning/alerts?ref=refs/heads/main` returns no alerts for that path.
- **PR #3057 had CodeQL SUCCESS**, and it landed earlier today. If these attached to every unrelated PR, it would have been red.
- **The alert text names the source:** *"Sensitive data returned by **HTTP request headers** flows to a logging call"*, created `2026-08-03T07:59:59Z` -- the day the #2957 branch first appeared.

So they are not pre-existing and not re-attribution. They are **CodeQL dataflow findings introduced by #2957/#3034**, which adds the only component in the tree that reads untrusted third-party HTTP headers. An existing sink became newly *reachable* in CodeQL's model because a new taint SOURCE was added elsewhere -- which is exactly how a dataflow analysis is supposed to behave, and is why the file being untouched by the diff was a misleading signal.

**What I believe is true instead** (argued on #3034, still under review): the finding is a false positive, because the sink's sole caller is guarded by a constant-vs-constant equality --

```go
if conceptMeta.Name == memorynodes.ConceptHarnessObservation {   // "v1:harness:observation"
```

-- and inbound stages `v1:platform:inboundRequest`. CodeQL cannot evaluate that guard, so it reports a path that cannot execute.

That is a **different claim** from the one this issue records, and it matters: "pre-existing, ignore it" invites every future PR to wave these through, while "unreachable behind a concept guard" is a statement someone can check and that stops being true if the guard changes.

Recommend this issue be retitled and rescoped to the real question -- whether to dismiss the four as false positives with the guard as the stated reason, or suppress at the sink -- or closed in favour of that decision being recorded on #2957. Not doing either here: it is not mine to decide, and the previous confident claim about these alerts was wrong, which is the reason to be careful rather than fast.

**(2)**

**Both research questions in the DoD are now answered, and the answers dissolve the scope this issue is written around. The remaining item is a repo-wide call I should not make.**

## What I measured (all against `refs/heads/main`, today)

**1. The four alerts DO exist on main.** Alerts **#422, #423, #424, #425** — `go/clear-text-logging`, severity **high**, state **open**, path `component/memql/executor_mutation.go`, lines **1086 / 1091 / 1127 / 1132**, created `2026-08-03T07:59:59Z`.

So the original premise is right on that point and the earlier note above is wrong. I reproduced that session's result before I reproduced the truth: an unpaginated `code-scanning/alerts` query returns nothing for this path, because the `go/clear-text-logging` family is large enough that these four fall past the first page. With `--paginate` they are plainly there. Recording the trap because it is what misled the previous measurement, and it will mislead the next one.

**2. But the taint source is neither session's.** The alert text on main reads:

> Sensitive data returned by an access to **`SiOpenaiApiKey`** flows to a logging call.

Not "HTTP request headers" (the NOTE's reading, which I believe came from a PR-scoped instance rather than main), and not the embedded content (this issue's reading). So *"#2957 introduced them"* is also wrong.

**3. They are 4 of ~492.** `go/clear-text-logging` has **492 open alerts on main**, overwhelmingly this same `SiOpenaiApiKey` family, spread across `integrations/planner/*` (the bulk), `integrations/workbench`, `integrations/telephony` and more. These four are not special; they are four instances of one systemic modelling result.

## DoD items 1 and 2, answered

**Item 1 — can `err` carry embedded content? YES**, and the chain is unbroken: `openai_embedding.go:93` folds the verbatim unbounded upstream response body into the error (`"embedding API error %d: %s"`), which `embedding.go:134` and `:289` wrap and the two `Warn` lines log. The text being embedded at those sites is user content.

*But this is not what the alerts are reporting* — they flag `SiOpenaiApiKey`, not the body. So item 1's prescribed remedy ("stop logging it") is justified on its own merits and would **not** clear a single one of the four alerts. Filed separately as **#3108** rather than folded in here, since it is a provider-level defect affecting every consumer of the embedding client.

**Item 2 — is `id` ever caller-derived? YES**, at both sites:
- `recordHarnessObservation` (`dsl/harness/mutations.memql:187`) takes `observationId` as a caller arg and stamps `id: args.observationId`. It is `@mcp`-exposed.
- `mintAction` (`dsl/actions/mutations.memql:61`) takes `actionId` and stamps `id: args.actionId`.

So the NOTE's "unreachable behind a constant-vs-constant concept guard" argument is narrower than it reads. The guard does block the webhook path, but it does not make these sinks unreachable from caller-controlled data — the mutation's own args are a live source.

## Why I am handing this back rather than building

Item 3 is *"either fix, or dismiss the alerts with the reason recorded."* Neither branch is mine to pick:

- **Fixing cannot work as scoped.** Two of the four sinks (1091, 1132) are `Debug` lines carrying only `id` and a length — there is no `err` to drop. No edit to these four lines clears four alerts.
- **Dismissing is a ~492-alert, repo-wide security-posture decision**, not a local one. Dismissing four while 488 identical findings stay open is the "real alert eventually gets ignored" failure this issue was filed to prevent, pointed the other way.

The scoping choice — sanitize the `SiOpenaiApiKey` accessor at source, add a CodeQL filter/model tuning, dismiss the family with a recorded reason, or accept it — changes what gets built and touches a hundred files. That is a decision about intent, not about code.

## Recommendation

Close #3052 in favour of two issues that are each buildable:

1. **#3108** (filed) — the embedding client's verbatim-response-body leak. Real, self-contained, testable, and independent of CodeQL.
2. **A new repo-wide issue for the `SiOpenaiApiKey` `go/clear-text-logging` family** (492 alerts). One decision, applied once, that actually makes the CodeQL check mean something again.

If you would rather keep #3052, it needs retitling and rescoping to (2) — its current title ("four pre-existing alerts … re-attributed to every unrelated PR") describes a real symptom but names the wrong cause, and anyone who picks it up cold will re-derive what is above.


---

## #3059 -- raw insert() bypasses accept/stamp entirely, so a declared owner tier can still be forged on most concepts

**Classification:** `auth`, `dsl`, `security`

Found by the authorization lens during the #3055 landing review, and verified independently. **Pre-existing** — #3055 neither introduces nor worsens it — but it bounds what a `stamp { ownerUserId: actor.userId }` can promise, and #3055 was about to write an unbounded promise into the tree.

## The path

```
component/memql/parser.go:520   isInsertFunction(query) -> strings.HasPrefix(lower(q), "insert(")
component/memql/engine.go:517   -> executeMutation(ctx, plan.Mutations[0])
                                   with provenance.Direct("rawInsert:"+concept)
```

A query whose text begins `insert(` short-circuits the planner and goes straight to `executeMutation`. It never touches the MutationTemplate, so the `args { }` schema, `accept { }` and `stamp { }` are all bypassed — the payload is written as supplied.

The only remaining gates are the reserved-field check and the per-concept Go guards at `component/memql/executor_mutation.go:598-726`, and those cover exactly three concepts:

- `ConceptCognitionUtterance`
- `ConceptCognitionParticipant`
- `conceptAgentsAgent`

Every other concept — including `v1:library:generatedOutput` and `v1:library:documentVersion`, the two whose owner tier #2989 just made server-stamped — has no guard on this path.

## Why it matters

`@rowAuthz` does not close it. `component/memql/rowauthz_shadow.go` is explicit that the gate is read-side and shadow-only: *"It injects nothing."* And #2803's Phase 3 (read-time enforcement) is still deferred.

So for any concept with a declared `owner` tier and no per-concept Go guard, `insert("<concept>", ..., payload={"ownerUserId": "<someone-else>"})` writes an arbitrary owner. That is the same forgeability #2982 / #2988 / #2989 spent three PRs removing from the named-mutation surface, still open one door over.

**Reachability is the part that needs a decision rather than a patch.** The lens reports it is reachable by any authenticated role via `handleExecuteQuery` (`component/grpc/server.go:1582`), whose own comment states the policy: *"Any authenticated role may reach this handler; query/mutation bodies enforce ownership."* That comment is precisely the assumption the raw path breaks — there is no mutation body on this path to do the enforcing. It is also a documented surface (`docs/public/language/memql.md:704-706`), so it may well be intentional for operators and merely under-gated for everyone else. **I have not established what the surface is meant to be**, which is why this is filed rather than fixed.

## Contributing gap: `@serverSet` is inert here

`ownerUserId` is not marked `@serverSet` on either library concept, so the load-time guard `validateMutationCallerArgs` (`component/memql/function_loader.go:1523-1541`) — which would reject a future `accept { ownerUserId }` — never fires for them. Cheap defence in depth on the template path, orthogonal to the raw path. Worth doing regardless of how this issue is resolved.

There is also no load-time collision check between `accept` and `stamp` keys; only the duplicate-`id:` check at `component/language/parser/rewriter.go:1170`. `stamp` wins on collision (last-key-wins at `mutation_templates.go:1498`), which is the safe direction, but it is unasserted.

## Options

1. **Extend the per-concept guard table** to every concept carrying a declared `owner` tier — server-stamp the owner field on the raw path too. Closes it generally, and the table already exists.
2. **Gate the raw `insert(` surface by role** (operator/admin only), matching what the docs imply it is for.
3. **Land #2803 Phase 3**, so the read side stops trusting the write side. Bigger, and it does not stop a forged write — only its visibility.

Option 1 looks smallest and most targeted to me, but which is right depends on what the raw surface is supposed to be, which is a call for the repo owner.

Refs #2989 #2982 #2988 #2803 #3055

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

Q: Is the raw `insert(` query surface meant to be reachable by any authenticated role
(so the fix is to server-stamp owner fields on that path), or is it meant to be an
operator/system-actor tool (so the fix is to role-gate it)? Option 1 or option 2 -- one
word is enough.

Context: I claimed this to build it and I am handing it back, because the decision the
issue defers to you is not one I can take from the repo. I did the research first rather
than asking on the strength of the issue text, and it confirms the author's read: nothing
in the codebase settles what this surface is for.

What the research adds, beyond what the issue says:

- The public language doc is the strongest evidence, and it points AWAY from
  operator-only. docs/public/language/memql.md is `audience: public`, `status: stable`,
  and line 683 reads: "There are two write surfaces: the runtime `insert()` literal and
  DSL-defined named mutations." The six rules at 699-706 are purely mechanical -- one
  insert per statement, id must be a literal, and so on. Nothing names an audience, and
  nothing marks it legacy or diagnostic. It reads as a co-equal general feature. So
  option 2 is a breaking change to a documented public contract, not just a gate.
- The repo already HAS the gating machinery and deliberately did not apply it here.
  `allowInline` restricts other advanced query shapes to the inline tier plus
  owner/developer (parser.go:50-53, MCP Tier-3 #1535). The `insert(` short-circuit in
  engine.go fires before `origin` is consulted at all. That is either an oversight or a
  decision, and nothing records which.
- Two internal comments contradict each other, in the same file. executor_mutation.go
  538-541 calls the path a system-actor tool ("the polyphon HTTP utterance handler and any
  other system-actor write"), while the guard at 662-665 defends explicitly against a USER
  actor forging a row "over the raw ExecuteQuery mutation surface". Both framings are live
  and neither is reconciled anywhere.
- Correction to the issue body: the per-concept guard table covers NINE concepts, not
  three -- also conceptRbacRole, conceptIdentityIdentity, conceptHealingOverride,
  conceptHarnessStep, conceptPlannerPlan, conceptForgeRequest. Every guard cites a
  specific issue number as its origin (#403, #2070, #2072, #2140, #2143, #2513). So the
  table is reactive and per-incident; there is no stated rule that would tell me which
  concepts a general fix should cover.
- No role check exists on the path: handleExecuteQuery (server.go:1592-1594) states
  "Any authenticated role may reach this handler; query/mutation bodies enforce
  ownership." No test anywhere asserts authorization on the raw insert path --
  grep for rawInsert/isInsertFunction across *_test.go returns nothing.
- @serverSet would not help even if added: it is consulted only at mutation-template
  LOAD time (function_loader.go:1512-1538, the only non-test caller of ServerSetFields()).
  Concept.Create() never consults it, so a @serverSet field is still forgeable by a raw
  insert. The issue calls that marking "worth doing regardless" -- it is, but it is
  defence in depth on the template path only, and it does not narrow this decision.

Options, with what each costs:

1. Server-stamp the declared owner tier on the raw path (extend the guard table, or make
   the raw path consult the @rowAuthz owner declaration generically). Keeps the documented
   public surface working for everyone; closes the forgery. Cost: the raw path stops being
   "written as supplied", which any existing caller that sets ownerUserId deliberately
   would notice -- and per the f4b205b2 commit message the direct callers include cockpit,
   web UI, CLI and tests.
2. Role-gate the raw surface to operator/admin. Matches the system-actor comment and is
   the smaller diff. Cost: it contradicts a `status: stable` public doc that presents
   insert() as one of two general write surfaces, so it is an API break for any client
   using it today.
3. Land #2803 Phase 3 (read-time enforcement). Does not stop a forged write, only its
   visibility, and is much larger. Not a substitute for 1 or 2.

Recommendation: option 1, matching the author's lean, and for a reason the research
sharpened -- option 2 breaks a stable public contract while option 1 preserves it, and
the concepts most at risk (v1:library:generatedOutput, v1:library:documentVersion) already
declare @rowAuthz(owner="ownerUserId") in dsl/library/concepts.memql (lines 105, 173).
That declaration is a machine-readable list of exactly which fields to stamp, so option 1
can be general rather than another entry in a reactive table -- which also answers the
"which concepts" question the guard table cannot. If you pick 1, say whether the generic
form is wanted or whether you want the nine-entry table extended by hand; I would build
the generic form.


---

## #3063 -- The three per-user credential lists still take a caller-supplied userId with no caller check

**Classification:** `auth`, `dsl`, `security`

Split out of memql#2987, which re-audited the 10 `@public` queries in `dsl/identity/queries.memql` and resolved 7 of them. These three are the group that audit deliberately did not guess at; the analysis is below and is also recorded inline at each query.

## The three

| query | shape | Go caller |
|---|---|---|
| `patIdentitiesForUser` | `patSummary` | `component/identity/pat/store.go` `ListForUser` / `ListForUserPage` |
| `workerTokensForUser` | — | `component/identity/workertoken/store.go` |
| `badgesForUser` | `identitySummary` | `component/identity/badge/store.go` |

All three filter on `userId==args.userId` — **a caller-supplied id with no check that the caller IS that user**. So any authenticated client can enumerate another user's credential metadata by passing their id. No credential material is projected (that part was verified), but names, creation times and last-used times are.

`@public` enforces nothing at runtime; it only suppresses the classifier, and it is matched ahead of the flagged bucket in `dsl/conformance_test.go`.

## Why #2987 stopped rather than shipping it

The obviously-right filter is `userId==actor.userId` — "your own credentials". Two things have to be settled first, and neither is guessable from the DSL:

1. **Is every Go call site the owner acting on themselves?** `/me/tokens` is. If any path is an admin listing somebody *else's* credentials, that filter empties an admin surface. The call sites take a `userId` argument, so the question is what the callers pass, not what the query declares.
2. **`@serverOnly` is the wrong tool here** — unlike the node-token trio #2987 did move, these back a settings UI that consumes them over the wire, so removing them from the wire breaks it. And `component/identity/workertoken` and `component/identity/badge` are **not** in `call_origin_conformance_test.go`'s internal-origin allowlist, so that route would need an allowlist entry plus the argument for one — a second decision stacked on the first.

## What "done" looks like

- Walk the call sites of all three and record, per site, whether the `userId` passed is the acting user's.
- If they all are: `userId==actor.userId`, drop the arg, drop `@public`, and the concept moves toward the `owned` bucket.
- If any is an admin path: that path needs `requiresOwnerOrAdmin` as a top-level conjunct instead (the `searchUsers` precedent memql#2987 used for the audit-event queries), or its own query.
- Either way it is a wire change for the settings UI, so it wants its own review rather than riding an audit PR.

## Not urgent, and worth saying so

Metadata only, no credential material, and every row is reachable only by someone who already knows a valid user id. That is why #2987 shipped the credential-exposing half and left this.

Refs memql#2987 memql#2918 memql#2920 memql#54

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

**(1)**

**Severity correction from the #3064 landing review: this group is not credential-free, and `workerTokensForUser` is its worst item rather than a peer.**

This issue's body says *"No credential material is projected (that part was verified)"*, and #3064's code note said the PAT debt has the *"same shape on `workerTokensForUser` and `badgesForUser`"*. The first is true of two of the three. It is **false** of `workerTokensForUser`.

Measured on `main`:

| query | filter | shape | credentials on the wire? |
|---|---|---|---|
| `patIdentitiesForUser` | caller-supplied `userId`, no actor check | `patSummary` | no |
| `badgesForUser` | same | `identitySummary` | no |
| **`workerTokensForUser`** | same | **`identityFull`** | **yes** |

`identityFull` projects `credentials` (`dsl/identity/shapes.memql`), and the `worker_token` variant of that object carries **`keyHash`**, `registeredBy`, `lastSeenAt` and **`lastConnectedFromIP`** (`dsl/identity/concepts.memql`).

So any authenticated caller can read another user's worker-token digest and last-connect IP by passing their id. The keyHash is a SHA-256 rather than the token itself, so this is not directly replayable -- but `lastConnectedFromIP` is exactly the class of signal #2987 gated `auditEventsByActor` / `auditEventsByTarget` to protect, and the digest is credential material by any ordinary reading.

**Why this matters for how the group gets scheduled:** `workerTokensForUser` is materially identical to `nodeTokenIdentityById`, which #2987 **gated** -- caller-supplied id, no actor check, credentials projected. The audit gated one and deferred the other on a shape it had not checked. Notably `badgesForUser`'s shape *was* re-verified in the adjacent note, so this is a specific gap rather than a blanket omission.

Suggest re-ordering this issue so `workerTokensForUser` is addressed first, and treating it as a narrower question than the other two: the filter debate (`userId==actor.userId` vs the admin surfaces) is common to all three, but the **projection** is not. Swapping `identityFull` for a credential-free worker shape would remove the exposure without touching the filter, so it cannot break the `/me/workers` or cockpit Workers call sites the deferral was protecting -- which makes it separable from, and cheaper than, the scoping decision.

Not done in #3064: narrowing the projection is a wire change, and that PR deliberately deferred this group. What #3064 did change is the record -- the false "same shape" sentence and that query's own banner claim that it lists tokens *"without exposing the keyHash"*, which had been false for as long as the shape was `identityFull`.

Refs #2987 #3064

**(2)**

**Landed.** PR #3072 merged to `main` at `2026-08-05T00:10:36Z` via the merge queue (position 1, `AWAITING_CHECKS` -> merged). Verified in the tree, not just via the API: both fix commits (`78bc1682`, `51357832`) are contained in `origin/main`, `workerTokensForUser` carries `@serverOnly` there, and both new gate files are present.

**What landed.** `workerTokensForUser` was `@public` while projecting `identityFull` -- so the row carried `keyHash`, `registeredBy`, `lastSeenAt` and `lastConnectedFromIP` -- behind a filter keyed on a caller-supplied `userId` with no actor check. Any authenticated caller could read another user's worker-token digest and last-connect IP by passing their id. It is now `@serverOnly`, off the wire and out of the SDK, with the one legitimate reader (`workertoken.Store.ListForUser`, the revoke ownership check) stamping internal origin on a context scoped to that single query.

**Three REAL findings were fixed before merge, all of them guard gaps rather than live bugs -- and all three of the same family the issue itself is about: a protection nothing would notice the loss of.**

1. The PR's headline claim, *"both halves are tested"*, was false. The test stamped internal origin at its own call site and never routed through `ListForUser`; deleting the production stamp left the entire suite green while every worker-token revoke would fail closed at runtime.
2. `badgesForUser` cited `workerTokensForUser` as its "same auth shape" peer -- an anchor this PR moved off the wire, so the sentence had quietly inverted into an argument that `badgesForUser` should follow it.
3. The allowlist entry granting the internal-origin stamp claimed "server-initiated" against that file's own definition (*"None of them is a request handler"*). `ListForUser`'s only caller **is** a request handler, making this the second request-derived exception in the tree -- the shape #2989 refused -- and the precondition that makes it sound (the userId is always the authenticated caller's subject) was asserted by nothing. A future handler passing a payload field would have put another user's `keyHash` back on the wire with the annotation, the stamp test, and the conformance gate all green.

Each fix is a test that fails against the defect and passes against the fix, mutation-verified rather than reasoned about. The `#3063` exposure is now closed at three layers: the wire (`@serverOnly`, pinned by `dsl/server_only_parsed_test.go` and a DB-gated engine test), the stamp (`store_internal_origin_test.go`), and the argument (`component/grpc/worker_token_caller_scope_test.go`, the `route_gate_test.go` analogue #2934 established).

**Follow-up, not blocking:** `patIdentitiesForUser` and `badgesForUser` remain `@public` on the same unresolved decision -- whether `userId==actor.userId` breaks an admin surface. Both project `identitySummary` with no credential material, which is why they could wait and this one could not. That decision is still open and still needs a human ruling; the in-tree notes now describe the group accurately.

**(3)**

**Reopened -- I closed this in error, minutes ago.** #3072 landed one of the three queries this issue covers, not all three.

`workerTokensForUser` is done: `@serverOnly`, off the wire, out of the SDK, with the exposure pinned at three layers. The other two are untouched and still exactly as this issue describes them:

| query | state after #3072 |
|---|---|
| `patIdentitiesForUser` | still `@public`, still caller-supplied `userId`, no caller check |
| `workerTokensForUser` | **done** -- `@serverOnly` (#3072) |
| `badgesForUser` | still `@public`, still caller-supplied `userId`, no caller check |

`workerTokensForUser` could be resolved alone precisely because it was the one item that did NOT fit this issue's stated blocker. This issue reasons that `@serverOnly` is the wrong tool because all three back a settings UI over the wire -- true of the other two, false of this one: it had no wire consumer at all (verified during the #3072 review -- no `.memql` caller, no MCP surface, no `/me/workers` route, and zero cockpit references). It was also the only one projecting `identityFull`, so it was the only one putting `keyHash` and `lastConnectedFromIP` on the wire rather than metadata. That is why the operator ruled it out of the group during the #3064 landing, and why it took a different fix from the one this issue outlines.

The decision this issue is actually blocked on is untouched and still needs a human: **is every Go call site of `pat.ListForUser` and `badge.ListForUser` the owner acting on themselves, or is one an admin listing somebody else's?** `userId==actor.userId` is right if all of them are and empties an admin surface if any is not. Both remain a wire change for the settings UI, so both still want their own review.

The `@serverOnly` route is also still closed to them for the reason stated above -- but note one input has changed: `component/identity/workertoken` is now in `call_origin_conformance_test.go`'s allowlist, as the second REQUEST-DERIVED exception, carrying `component/grpc/worker_token_caller_scope_test.go` as its precondition. So the "would need an allowlist entry plus the argument for one" cost is now a known, worked example rather than an unknown. It does not make `@serverOnly` correct for a query the settings UI reads -- that objection stands on its own.

**(4)**

**The blocker this issue is parked on is answered, from the code.** Walked every call site of the two remaining queries:

| call site | userId passed | acting as |
|---|---|---|
| `component/identity/web/me_tokens.go:78,141` (`/me/tokens`) | `claims.Subject` | **the owner, themselves** |
| `subcommand_pat.go:161` (`memql pat list --user-id X`) | an explicit CLI flag | **an operator listing someone else's** |
| `pat.Store.ListAll` -> `ListForUser` (`memql pat list`, no flag) | every active user id | same, cluster-wide roll-up |
| `badge.Store.ListForUser` | — | **no production caller at all** |

So the answer is **"one IS an admin path"** — the CLI — and this issue's own DoD says what that means: `requiresOwnerOrAdmin` rather than a bare `userId==actor.userId`.

Two facts that unblock it, neither of which was available when it was deferred:

1. **The CLI already satisfies `requiresOwnerOrAdmin`.** `identity.ContextWithSystemActor` (`component/identity/middleware.go:19`) stamps `"role": "owner"`. So a self-OR-admin filter keeps `memql pat list --user-id X` working with no allowlist entry and no second query.
2. **The "MemQL has no OR operator" note in this very file (`queries.memql:218`, dated 2026-05) is stale.** `||` is live — `dsl/telephony/queries.memql:48` uses it in a shipped filter, and `dsl/agents/queries.memql:224` records `(predefined || row.createdBy == actor.userId)` as loading and pushing down. That note is part of why this looked harder than it is; fixing it is in scope here.

Intended filter, both queries:
```
userId==args.userId && (userId==actor.userId || requiresOwnerOrAdmin)
```
`/me/tokens` passes its own subject (true), the CLI is role=owner (true), any other authenticated caller passing a stranger's id gets **zero rows**.

Load-bearing unknown I am about to test rather than assume: `requiresOwnerOrAdmin` is an `@actor`-bound CONTEXT-spec, and the `searchUsers` precedent only uses it as a top-level AND conjunct. Whether the filter compiler handles one inside a disjunction with a row predicate is an engine question — testing it end-to-end, and I will say so plainly if it does not compile.

Blast radius: `dsl/identity/queries.memql`, regenerated SDK, tests.

**WIRE CHANGE — frontend ping.** Both queries narrow: a client passing another user's id now receives zero rows instead of that user's credential metadata. `/me/tokens` is unaffected.

**(5)**

**The blocked question is answered. A different one took its place, and that one is genuinely yours.**

## 1. The call sites, walked (the thing this issue was parked on)

| call site | userId passed | acting as |
|---|---|---|
| `component/identity/web/me_tokens.go:78,141` (`/me/tokens`) | `claims.Subject` | the owner, themselves |
| `subcommand_pat.go:161` (`memql pat list --user-id X`) | an operator CLI flag | **an admin, listing someone else's** |
| `pat.Store.ListAll` -> `ListForUser` (`memql pat list`) | every active user id | same, cluster-wide roll-up |
| `badge.Store.ListForUser` | — | **no production caller in this repo** |

So: **not all owner-acting-on-self.** A bare `userId==actor.userId` would empty the operator CLI, exactly as the deferral feared.

## 2. Two facts that were not available when this was deferred

**`ContextWithSystemActor` stamps `"role": "owner"`** (`component/identity/middleware.go:23-27`), so the CLI already satisfies `requiresOwnerOrAdmin` — no allowlist entry, no internal-origin stamp needed for the admin arm.

**The "MemQL has no OR operator in its filter grammar (verified … as of 2026-05)" note at `dsl/identity/queries.memql:218` is STALE.** `||` is live and shipped — `dsl/telephony/queries.memql:48` uses it in a real filter. That note is load-bearing in this issue's reasoning and should be deleted by whoever resolves this.

## 3. Why the obvious fix is nevertheless not available

I wrote it and measured it:

```
filter userId==args.userId && identityIsApiKey && (userId==actor.userId || requiresOwnerOrAdmin)
```

It **lints clean and the engine loads the full tree with it** (`TestEngineInitLoadsFullDSL` passes), so a context-spec inside a disjunction does compile — worth recording, since that was unknown.

But it is **refused by `dsl/admin_gate_test.go`'s `TestAdminGateIsATopLevelConjunct`**, deliberately:

> the admin gate does not hold on every path through its filter -- a false gate would NOT zero the result set. […] A gate inside a disjunction is switched off by the other arm.

That is a hard conformance rule covering 13 existing clauses, and it is right in general. I am not weakening it for this.

**So self-OR-admin is not expressible, and this issue's own prescribed remedy for the mixed case — `requiresOwnerOrAdmin` as a top-level conjunct — cannot coexist with `/me/tokens` in one query.** Both call sites share exactly one query today (`pat/store.go:202` and `:234`). The only compliant shape left is the DoD's third option: **its own query**.

## 4. The decision that is actually yours

Splitting PAT is well-determined and maps 1:1 onto the Go methods that already exist — `ListForUserPage` (self, `/me/tokens`) and `ListForUser` (admin, CLI). What I cannot settle from this repo:

- **Does anything outside this repo call these queries directly?** Both are generated into the SDK (`sdk/go/client/generated_queries.go`), so the cockpit could be calling either with an arbitrary userId. Splitting or scoping changes that wire contract, and the cockpit lives in another repo I cannot read.
- **`badgesForUser` has no in-repo caller at all**, so there is no evidence for self-scoped vs admin-gated. Guessing self-scoped could break an admin badge view; guessing admin-gated could break a self-service one. Both are invisible from here.

**Recommendation:** split `patIdentitiesForUser` into a self-scoped query for `/me/tokens` and an admin-gated one (`requiresOwnerOrAdmin` top-level) for the CLI; hold `badgesForUser` until someone greps the cockpit for both names. If the cockpit calls neither, `badgesForUser` can simply follow whichever shape PAT's self half takes.

Happy to build the PAT split immediately on a yes — it is an afternoon's work and every in-repo call site is verified. I stopped short of it because it is a wire change this issue itself said "wants its own review", and I would be guessing about the cockpit either way.


---

## #3067 -- seeder.go writes straight to the store, bypassing executeWrite and every write guard (empty input set today)

**Classification:** `engine`, `reliability`, `security`

Found by the authorization lens during the #3061 landing review. **Latent** — the input set is empty today — but it is live code on the boot path, and it is the one write route that skips every guard at once.

## The path

`component/database/memory-nodes/seeder/seeder.go` calls `concept.Create(seedCtx, r.store, ...)` **directly against the store**. That skips `executeWrite`, and therefore skips every guard hooked there:

- `validateAgentRolePredefinedLock` (#2985 / #3061)
- `validateRbacBaseRoleImmutable` and `validateRbacCustomRoleRankBound`
- the healing-override guard
- `validateAgentKindActorScope` and the agent-lock validation
- the identity credential actor-scope guard (#2513)

It is wired in production — `app/database.go` constructs and runs it during database bring-up.

## Why nothing is broken today

Verified, and this is the whole reason it is filed as latent rather than as a defect:

- it ingests only files literally named `seed.memql` (`seeder.go`),
- from `database.Concepts`, which is a typed-**empty** `fstest.MapFS{}` (`component/database/embed.go`),
- and `find . -name seed.memql` returns **zero** files.

So the loop body never executes. The hazard is entirely in what happens when someone adds the first one.

## Why that is a realistic thing to happen

`seed.memql` reads like the obvious name for a seed file, and the repo has an active seeding concept (`SeedMaterializer`, `dsl/agents/roles/*.memql`, `dsl/rbac/seeds.memql`) that goes through the **guarded** path. A contributor adding `seed.memql` for `agentRole` or `rbac:role` would get catalog rows written behind every lock, with nothing failing and nothing logged — and the guards would still look present and green.

## Options

1. **Route the seeder through `executeWrite`**, so it inherits the guards and the system-actor carve-out the SeedMaterializer already relies on. Most consistent; the carve-out means it would still pass.
2. **Delete it**, if `SeedMaterializer` has superseded it. An unused boot path with an empty input set and no guards is worth removing rather than documenting.
3. **Fail loudly if it ever finds input** — a stopgap that converts a silent bypass into a boot error naming the file, if neither of the above is wanted now.

Option 2 looks most likely correct given the empty input set, but that turns on whether this predates `SeedMaterializer` and was left behind, which I did not establish.

Related: #3059 records the other bypass of the template-level protections (raw `insert(`), which — unlike this one — still reaches `executeWrite` and therefore still hits these guards. This is the stricter case: it reaches neither.

Refs #3061 #3059 #2985

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

The deletion is correct and four lenses could not fault it: the input set was empty BY CONSTRUCTION (a compile-time fstest.MapFS{} literal never rebound; MEMQL_DSL_PATH mounts a different FS), the deleted package had ZERO test files, and the architecture model regenerates byte-identical by SHA256.

My review also found and fixed a real defect: the new guard missed the exact call it was written to stop. Restoring the seeder verbatim left it green, and git mv-ing it left BOTH tests green with the whole legacy seeder present. Fixed by dropping an argument predicate that bought nothing -- exactly one three-arg X.Create exists tree-wide -- and both defeats now fail as they should.

It is parked because MY commit message claims provenance.System() "has zero non-test callers". There are three (skill_migration.go:201,224; assistant_skill_reconcile.go:221). That claim drove me to DELETE a doc example rather than replace it, when a truthful replacement existed.

Cause, stated exactly: my grep ended in `| head -6` and I read a truncated list as complete. Fourth false absolute in this session; the counting rule I adopted did not catch it because I did count -- I counted a truncated set.


---

## #3075 -- Two workflows both publish a required check named `scan`, so the required set does not say what it means

**Classification:** `hygiene`

Found while landing #3074 (#3019). Filed rather than fixed there, to keep that PR to its story — the same way #3019 itself was filed off #3015.

## What

Branch protection (ruleset `16630577`) requires three checks by name: `ci-required`, `scan`, `Analyze (go)`.

**Two different workflows publish a check named `scan`:**

| workflow | job | `on:` triggers |
|---|---|---|
| `.github/workflows/gitleaks.yml` | `scan` | `pull_request`, `push`, `merge_group`, `schedule` |
| `.github/workflows/govulncheck.yml` | `scan` | `push`, `merge_group`, `schedule` — **no `pull_request`** |

Branch protection matches a required check by name, so it cannot distinguish them.

## Measured

On PR #3074, exactly one check named `scan` reports, and it is gitleaks:

```
$ gh pr view 3074 -R znasllc-io/memql --json statusCheckRollup \
    --jq '[.statusCheckRollup[]|select(.name|test("scan|Analyze|ci-required"))|"\(.name)\t\(.conclusion)\t\(.workflowName)"]|unique[]'
Analyze (go)                      SUCCESS  CodeQL
Analyze (javascript-typescript)   SUCCESS  CodeQL
Analyze (python)                  SUCCESS  CodeQL
ci-required                       SUCCESS  CI
scan                              SUCCESS  gitleaks
```

## Why it matters

This is the #3019 ladder one rung over. The previous rungs were about a lane that runs but stops gating. This one is about **a required check name that is satisfied by a different workflow than the reader believes**.

Someone auditing "what gates a PR here?" reads the required list, sees `scan`, and concludes secret-scanning *and* vulnerability-scanning both gate. Only gitleaks does at PR time. govulncheck gates the merge queue and pushes to `main`, which may well be the intended design — deferring an expensive scan to the queue is reasonable — but the name collision makes the distinction invisible, and no one can tell the deliberate half from the accidental half by looking.

The sharper hazard is the collision itself: two workflows contributing check-runs under one required context name is ambiguous, and if gitleaks' trigger set were ever narrowed, the required `scan` could be satisfied by whichever run happened to report — or by none, with the failure mode depending on GitHub's matching rather than on anything stated in the repo.

## What to do

Give them distinct names so the required set says what it means. Something like:

- `gitleaks.yml`: job `scan` -> `name: gitleaks`
- `govulncheck.yml`: job `scan` -> `name: govulncheck`

Then decide explicitly which of the two is required at PR time and update ruleset `16630577` to match. If govulncheck is deliberately queue-only, that is fine — but it should be visible as a deliberate choice rather than as a name that happens to collide.

A guard in the shape of the existing static CI tests (`scripts/dev/*_scope_test.go`) should assert that no two workflow jobs publish the same check-run name, since that is the property that makes the required set unreadable.

## Definition of done

- [ ] No two jobs across `.github/workflows/*.yml` publish the same check-run name.
- [ ] The ruleset's required contexts each map to exactly one workflow job.
- [ ] A test fails if a future workflow reintroduces a duplicate name.
- [ ] Whether govulncheck gates PRs is a stated decision, not a side effect of its trigger list.
- [ ] Demonstrated to fail — show the red.

Refs #3019 #2792 #2923

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

**Everything in the diagnosis holds. But there is no half of this that lands green on its own, and the missing half is a branch-protection change on a repo with 11 PRs in flight — so it needs you.**

## Confirmed, measured just now

```
$ gh api repos/znasllc-io/memql/rulesets/16630577 --jq '…required_status_checks[].context'
ci-required
scan
Analyze (go)
```

| workflow | job | `name:` | triggers |
|---|---|---|---|
| `gitleaks.yml` | `scan` | `scan` | pull_request, push, merge_group, schedule |
| `govulncheck.yml` | `scan` | `scan` | push, merge_group, schedule — **no pull_request** |

## Why there is no safe code-only change

I looked for a rename that closes the collision without touching the ruleset. There isn't one — **both directions break something**, and the second breaks it silently:

- **Rename gitleaks -> `gitleaks`.** Nothing publishes `scan` at PR time any more, so every open PR blocks forever on a required check that cannot report. Loud and catastrophic.
- **Rename govulncheck -> `govulncheck`.** PR time is unaffected (gitleaks keeps publishing `scan`), which is why this looked like the safe half. It is not: **in the merge queue both currently publish `scan`, so renaming govulncheck drops it out of the required set entirely.** It would still run, and it would stop gating. That is precisely the "a lane that runs but stops gating" failure this issue's own ladder (#3019, #2792, #2923) exists to catch — I would be building the next rung while closing this one.

And the guard test (DoD item 3) cannot land ahead of the rename either: it asserts no two jobs share a check-run name, which is **red on `main` today**. Correct, and unmergeable, until the rename it is guarding lands with it.

So the change is necessarily: rename both jobs **and** update ruleset `16630577` in the same operation.

## The part that is genuinely yours

1. **Does govulncheck gate PRs, or only the merge queue?** DoD item 4 says this must be a stated decision rather than a side effect of a trigger list — and it is a decision, not something I can read out of the repo. Deferring an expensive vulnerability scan to the queue is a defensible design; so is running it on every PR. The required contexts differ depending on which you want.
2. **Authorising the ruleset edit.** Required-context names are branch protection. Changing them is outward-facing, affects every open PR at once, and there are **11 sitting in `in-review`** right now.
3. **The cutover window.** There is no zero-downtime ordering. Land the rename first and PRs block until the ruleset catches up; change the ruleset first and they block until the rename lands. The window is short but real, and it is widest exactly now.

## Recommendation

Do it when the review queue is drained, not now — the cost of the window scales with how many PRs are in flight, and it is at its worst today.

Then, in one operation:
- `gitleaks.yml`: `name: scan` -> `name: gitleaks`
- `govulncheck.yml`: `name: scan` -> `name: govulncheck`
- ruleset `16630577` required contexts -> `ci-required`, `gitleaks`, `Analyze (go)`, **plus `govulncheck` if and only if you want it gating PRs**
- add the duplicate-name guard alongside, in the shape of `scripts/dev/*_scope_test.go`

I will build all of it on a yes, including the guard and the red-first demonstration the DoD asks for — it is maybe an hour. I stopped because the code half is worthless without the ruleset half, and shipping the code half alone hands the merge process a PR that either blocks the repo or silently un-gates govulncheck.


---

## #3076 -- rowAuthz Phase 3: enforce the declared tier on the read path (measured no-op: 0 would-narrow, 0 undecidable)

**Classification:** `auth`, `dsl`, `engine`, `security`

Phase 3 of #2803, ruled on 2026-08-05. The owner's deferral precondition -- *"Phase 3 does not land while any declared owner tier sits in the #2982 gate's exemption map"* -- is met: `ownerGateExemptions` is `map[string]string{}` at `origin/main`, emptied by `92403f69` (PR #3055, #2989).

## What lands

Enforce the declared row-authz tier on the **read path**: for a query whose bound concept declares `@rowAuthz`, the engine ANDs the injected predicate into the filter, instead of computing it and discarding it as shadow mode does today.

The mechanism already exists and is already correct. `component/memql/rowauthz_shadow.go` computes the predicate (`InjectedPredicate`) and decides implication (`AnalyzeShadow`); `component/memql/executor.go` already calls `recordShadow` at the two hook sites. This story changes what happens to that value, not how it is derived -- the one-detector constraint Phases 1 and 2 were built under stays intact.

## The blast radius is measured, not estimated

Regenerated at `origin/main` (`a43800a5`) immediately before filing:

```
go test ./component/memql/ -run TestRowAuthzShadowReport -v

query constructs in the tree      201
  measured (concept declares)      33
  not measurable (undeclared)     168
  no resolvable bound concept       0

verdicts over the measured set:
  already-implied                  33
  would-narrow                      0
  undecidable                       0
```

**Zero rows change anywhere.** All 33 measured accesses already imply the predicate that would be injected. There is no would-narrow list to adjudicate and no undecidable access that enforcement would change blindly.

## Do not trust that number at land time -- re-derive it

Today's measurement is evidence for the ruling, not a licence. Queries get written between now and the merge, and one new query over a declared concept that omits the conjunct turns this from a no-op into a silent result-set change.

**So the enforcement commit must carry its own gate**: a test that runs the analyzer over the loaded tree and **fails if any measured access is `would-narrow` or `undecidable`**. That is the difference between "enforcement was safe when we ruled" and "enforcement is safe now". If it fails, the right response is to adjudicate the entry -- latent authorization bug, or legitimate admin-tier read needing an explicit declaration -- not to widen the gate.

## `TestRowAuthzIsInert` is retired here, deliberately

`component/database/memory-nodes/concept_rowauthz_test.go:273` fails if the execution path reads `Concept.RowAuthz` outside its allow-list, and its own comment states the exit condition:

> the allow-list "GREW in #2921, and that growth is the mechanism working rather than the gate weakening ... **Phase 3 lands the same way**"

This is that commit. Replace the inert gate with the would-narrow/undecidable gate above -- the tree must never be left with neither, or enforcement can drift with no mechanical statement of what it is allowed to do.

## Scope boundary: read path only

This does not close the write path. #3059 (raw `insert()` bypasses accept/stamp, so an owner tier can still be forged on most concepts) is the write-side counterpart and is independently open. Enforcement here means "you cannot READ a row whose declared owner is not you"; it does not mean "nobody can WRITE a row claiming you own it". Say so in the commit message, because the tier reading as a complete guarantee is the failure mode this epic keeps circling.

## Coverage, stated honestly

13 of 101 concepts declare a tier. The 168 unmeasured constructs -- **48 of them `v1:identity:`** -- are untouched by this story and are the subject of its sibling. This story buys a **ratchet**, not a fix: it makes the next filter edit that silently drops `&& ownerUserId==actor.userId` fail instead of quietly widening visibility, which is the recurrence class #2799 identified and the one thing review demonstrably cannot catch.

## Definition of done

- [ ] The injected predicate is ANDed into the filter for declared concepts, on every read path shadow mode already hooks (including graph expansion -- `ShadowRecord.Path` exists so coverage can be shown rather than asserted).
- [ ] A gate fails the build if any measured access is `would-narrow` or `undecidable`.
- [ ] `TestRowAuthzIsInert` retired in the same commit, not before and not after.
- [ ] A regression test proving enforcement bites: a query over a declared concept with the ownership conjunct removed returns a narrowed set, and the gate above catches it.
- [ ] The `public` tier still injects nothing, and `clusterOwner` behaves as declared.
- [ ] Commit message states the read-path-only boundary and references #3059.

**No frontend ping expected** -- with 0 would-narrow, no wire response changes. If the land-time measurement is non-zero, that conclusion is void and the change needs a ping.

Ruled with its sibling (the undeclared-population gate), because enforcement over 16% of the tree without a mechanism that shrinks the other 84% buys a ratchet and pays for it in false assurance -- *"it reads as safe, so an auditor seeing the tier stops looking"*, in the words of `rowauthz_owner_gate_test.go`.

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

**Not landing this. It ships a security control with a caller-controlled off switch, and it introduces a cross-user data leak that does not exist on `main`.**

Everything below I reproduced myself, with a working control. These are not test-quality findings.

### 1. The gate is bypassable by choice of filter spelling (client-reachable)

`enforceRowAuthzFilter` decides *whether to enforce at all* from the caller's own filter:

```go
decl := rowAuthzDeclFor(extractConceptFromExpression(expr))
if decl == nil { return expr, nil }   // pass through, UNENFORCED
```

`extractConceptFromExpression` returns `""` for anything that is not a top-level `concept==<id>` equality. Measured (concepts loaded, so the control is meaningful):

| filter | enforced |
|---|---|
| `concept=="v1:notes:note"` | **true** (control) |
| `id=="v1:notes:note:<victim-shortid>"` | **false** |
| `concept=="v1:notes:note" \|\| payload.x=="1"` | **false** |
| `concept!="v1:other:thing"` | **false** |

`v1:notes:note` declares `@rowAuthz(owner="ownerUserId")`. `handleExecuteQuery` (`component/grpc/server.go:1582`) passes a **raw client-supplied query string** to the engine, so an authenticated caller reads another user's row by naming it by id.

Note this is *faithful to the issue*, which is the problem: the story says "the mechanism already exists and is already correct ... this changes what happens to that value, not how it is derived". That premise holds for **shadow mode**, where a missed concept means "not measured" -- harmless. Under enforcement the same miss means "not enforced".

A struct query `filter a || b` lowers to `(concept==X && a) || b` -- a top-level OR -- so this is reachable from authored DSL too, not only raw queries.

### 2. NEW cross-user leak via the result cache (a regression, not an unmet promise)

`engine.go:739` folds caller identity into the cache key only when `planReferencesActor(plan.Root)` is true. `plan.Root` is the **author-written** expression; enforcement injects the actor term later, into a local, and never mutates `plan.Root`. Measured:

```
planReferencesActor(plan.Root)  = false   <- decides the CACHE KEY
planReferencesActor(enforced)   = true    <- what actually runs
plan.Root mutated by enforce?   = false
```

Caching is default-on, 60s, and the denylist is `v1:identity:` only -- so `v1:notes:note` is cached. Caller A runs `concept=="v1:notes:note"`, gets their notes, primes the entry; caller B issues the identical string within 60s, hits the cache, and is served **A's rows**. On `main` this query returns all notes to everyone, so the cache is consistent and there is no leak. Enforcement is what creates the divergence.

That is exactly the collision the comment at `engine.go:730-738` says the actor fold exists to prevent.

### 3. DoD item 1 is unmet and unacknowledged: graph expansion

Two `recordShadow` sites, one enforcement site. `expandGraph` calls `builder.addNode(apiNode)` (`executor.go:969`) with the **full payload** before the depth guard, and reaches rows through `resolveChildOf` / `resolveAliasOrEquals`, which call `executeFilterQuery` directly -- `evaluateExpressionSet` is never entered, so enforcement is structurally unreachable there. The existing comment already states the consequence: a traversal "reaches a row the tier would exclude". Not exploitable today (no declared concept is a relationship target), but the DoD named it explicitly and neither the PR body nor the commit mentions it.

### 4. Empty `actor.userId` degrades rather than refuses

With no `AccessContext`, the injected term compiles to `ownerUserId = ''`, matching rows whose owner is the empty string, instead of refusing. The self-owned form (`owner="id"`) *does* fail closed, so the two spellings disagree.

### Verified sound, stated plainly

- **Precedence is correct.** Enforcement builds the conjunction at the root: `(((a) OR (b)) AND (authz))`. I checked the emitted SQL. The OR hole above is the gate being skipped, not mis-parenthesisation.
- **Unknown tier / `owned` with no owner / `granted` with no spec all genuinely refuse the read** in the production path, not just in the unit test.
- **The unrecognised-AST-node default is the safe one** -- it ANDs rather than skipping, unlike the #2982 analyzer.
- **The land-time gate is real**, and the measurement re-derives at PR head: 33 measured, 33 already-implied, 0 would-narrow, 0 undecidable.

### Why I am asking rather than fixing

Findings 2 and 4 I can fix inside this PR. Finding 1 I cannot, without doing the thing the issue explicitly rules out: resolving the tier from the **declared binding** (`fn.BoundConcept`, the source `TestRowAuthzShadowReport` already uses) instead of from caller filter text. The issue states "the one-detector constraint Phases 1 and 2 were built under stays intact", and overriding an explicit scope decision on a security boundary is the owner's call, not mine.


---

## #3077 -- rowAuthz: gate the undeclared population with a shrink-only list, so 168 unmeasured constructs (48 of them identity) cannot stay invisible

**Classification:** `auth`, `dsl`, `engine`, `security`

The sibling of #2803's Phase 3 enforcement story, and the half that makes enforcement honest.

## The problem this exists to close

Phase 3 enforces the declared tier. Measured at `origin/main` (`a43800a5`), that is **13 of 101 concepts** and **33 of 201 query constructs**. The other **168 constructs are not "safe" and not "unchanged" -- they are unmeasured**, and shadow mode's own report says so in those words:

```
--- NOT MEASURABLE: concept declares no tier (168 constructs) ---
  Shadow mode computes a predicate only where a tier is declared.
  These are not 'no change' -- they are 'not measured'.
```

**The coverage is inverted against risk.** The declared 13 are notes, todos, calendar, library, actions, authoring, healing, telephony. The undeclared 168 break down by namespace as:

| namespace | undeclared constructs |
|---|---|
| **`v1:identity:`** | **48** |
| `v1:planner:` | 17 |
| `v1:agents:` | 17 |
| `v1:cognition:` | 14 |
| `v1:cluster:` | 11 |
| `v1:platform:` | 10 |
| (12 more) | 51 |

Among the 48: `badgesForUser`, `authSessionsForSubject`, `authSessionByRefreshTokenHash`, `activeUsers`, `agentAuthorizationsForUser`, `auditEventsByActor`, `auditEventsByTarget`. The credential surface is exactly the population the tier does not reach.

So the state after Phase 3, absent this story, is the failure mode `rowauthz_owner_gate_test.go` names outright: *"the declaration records a guarantee nothing provides -- and it reads as safe, so an auditor seeing the tier stops looking."* An enforced tier over 16% of the tree, with the 84% invisible and no pressure on the number, is worse than no tier at all for exactly that reason.

Today the only signal is a boot-time warning. A warning that has been true for 168 constructs for months is not a signal, it is wallpaper.

## What lands

A **shrink-only gate** over the undeclared population, in the idiom this repo already uses twice (`ownerGateExemptions` in `component/memql/rowauthz_owner_gate_test.go`, and `TestRowAuthzIsInert`'s allow-list):

1. A test enumerates every query construct whose bound concept declares no `@rowAuthz` tier -- the same walk `TestRowAuthzShadowReport` already does, which is where the 168 comes from.
2. It compares that set against a checked-in list seeded with exactly today's 168 entries.
3. **A construct not on the list fails the build.** A new query over an undeclared concept must either declare a tier on the concept, or be added to the list with an issue number recording the decision.
4. **A stale entry fails the build.** An entry that no longer applies -- the concept now declares a tier, or the construct is gone -- must be deleted. This is what makes the list shrink-only, and it is lifted directly from `ownerGateExemptions`'s own staleness check, which exists because *"a stale exemption is as bad as a missing gate: it names a concept that may no longer exist, and it suppresses that concept forever."*

The number becomes a visible, monotonically decreasing debt with a name, instead of a warning nobody reads.

## What this story is NOT

**It does not declare a tier on anything.** Each of the 48 identity declarations is a real authorization judgment -- `activeUsers` and `badgesForUser` are not obviously owner-scoped, and several are legitimately cross-user admin reads. Making those calls is downstream work this gate makes visible and sequenceable; doing it here would bury 48 security decisions inside a tooling change.

Related and deliberately not folded in:
- **#3029** -- `owner="id"` form, needed before `v1:identity:user` can declare a tier at all. Several of the 48 are blocked on it.
- **#2991** -- the update/delete-by-id counterpart on the write side.
- **#3059** -- raw `insert()` forgeability, the write-path hole.

## Definition of done

- [ ] The gate enumerates undeclared-concept query constructs from the loaded registry, not from a hand-maintained duplicate of the tree.
- [ ] Seeded with exactly the current set; the count is asserted nowhere as a magic number, only the membership.
- [ ] A new query over an undeclared concept fails until it is declared or listed with an issue reference.
- [ ] A stale entry fails with a message naming which of the two reasons applies.
- [ ] The failure message tells an author the two ways out, in the style of the existing gate: declare the tier, or file the decision. Not "add it to the list".
- [ ] README-level note (or the file header, as the existing gates do it) stating the list may only shrink, and why.

No wire contract changes. No frontend ping.

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

The ratchet is sound: pinned list matches the derived set exactly (168/168, zero either-side-only), it fails when new undeclared debt appears, and it enforces sync in BOTH directions. Gate 1 green.

My review found and fixed a real defect: the stale classifier reported a MISSING concept as "debt paid -- DELETE them", which would empty the whole 168-entry ratchet in one commit. Proven by mutation before and after. That fix stands.

It is parked because I introduced a false "Measured:" claim -- in a comment and in the shipped failure message -- asserting a skipped concept would surface as "debt paid". It would not: the concept drop takes its queries with it, so the run fatals on the pre-existing function-skip guard. I wrote "Measured:" about something a review lens measured, without running it myself.

Five parks in this session, five prose defects, zero code defects. Operating change: I add no justification prose to a PR at all -- fix code, delete false claims, reasoning goes in the thread.

Also filed: memql#3135.


---

## #3079 -- rowAuthz Phase 5: enforce the declared owner tier on update/delete in the engine (120 of 215 mutations take a caller-supplied target)

**Classification:** `auth`, `dsl`, `engine`, `security`

Phase 5 of #2803, ruled on 2026-08-05. The write-side counterpart to #3076 (Phase 3, read side), and the unblocker for two of #2991's three remaining parts.

## The gap

Measured at `origin/main` (`a43800a5`):

```
mutate declarations in the tree                      215
  update { id: args.X }  (caller-supplied target)    120
mutations carrying @serverOnly today                   3   (all identity)
```

**120 of 215 mutations take a caller-supplied target id with nothing relating it to `actor.userId`.** The mutation grammar cannot express such a relation -- zero mutations in the tree carry a filter, and the `update` block takes `id:` plus field assignments and nothing else. `dsl/update_by_id_gate_2991_test.go` states this outright: *"That is memql#2803 Phase 5, not a thing this issue could fix."*

Every layer that looks like it should catch this does not, and each reason is load-bearing elsewhere:

- `validateMutationCallerArgs` walks the payload's **named** keys, so a splat (`args.payload`) names none and the sensitive-field gate is structurally blind to it.
- Row-authz enforcement is inert by construction until #3076.
- `@serverOnly` is the only working gate today, and it is not general -- see below.

## The ruling

> **Enforce ownership on the write path in the engine, driven by the same declared tier Phase 3 reads.**

For an `update` or `delete` over a concept declaring `@rowAuthz(owner="F")`, the engine resolves the target row and **refuses the write when the row's `F` is not the actor**, rather than requiring each mutation to say so.

**Why this shape and not the alternatives.**

- *Not a DSL guard clause.* Extending the grammar so authors write a predicate per mutation is 120 separate authorization judgments, nothing forces a mutation to carry one, and a missing guard is indistinguishable from a deliberate omission. The declaration already exists at the concept; asking every mutation to restate it is where drift enters.
- *Not case-by-case `@serverOnly`.* It worked for `updateUser` (#3021) because that mutation had exactly one production caller. `@serverOnly` removes a construct from the generated SDK, and most of the 120 back client UIs -- so applying it 120 times is 120 frontend breaks, not a policy. It stays the right tool where there is genuinely no client caller; it is not the general mechanism.

**The property this buys:** one declaration governs both directions. A concept that declares a tier is enforced on read (#3076) and on write (this story) together, so #3077's shrink-only list drives coverage for both sides at once instead of needing a second campaign.

## Constraints, all load-bearing

**1. The internal-origin escape must be narrow, and the loose version is already refuted.** #2989 built and **refuted** the route of stamping internal origin on a request-derived context: it opens every `@serverOnly` construct for the rest of that request. Whatever escape this story uses must be scoped to the single write it authorizes, not to the request. Reuse the shape that landed in #3072 (`workertoken.Store.ListForUser` stamps internal origin on a context scoped to one query), not the shape that was rejected.

**2. `clusterOwner` is an escape; `admin` is not, without a decision.** State which roles bypass and why, in the commit. Do not infer it from the read side.

**3. A missing target row must not read as authorized.** Refuse or report not-found -- never fall through. This is the fail-open direction, and #2982's analyzer already had to be fixed once for exactly that (`e486d0f5`, *"the analyzer failed OPEN on lowered AST nodes"*).

**4. Coverage must be stated, not implied.** Today 13 of 101 concepts declare a tier, so this guard reaches 13 concepts' mutations on day one. Say so in the commit message. The remaining reach comes from #3077, not from this story.

## Scope boundary

This is the **update/delete** path. It does **not** close #3059 (raw `insert()` bypasses accept/stamp entirely, so an owner tier is still forgeable on most concepts) -- an insert has no target row, so its problem is stamping rather than guarding. #3059 stays independently open, and the two together are what make a declared tier mean something end to end.

## Definition of done

- [ ] `update` and `delete` over a concept with a declared owner tier refuse when the target row's owner field is not the actor.
- [ ] The escape hatches are enumerated in one place, scoped per-write rather than per-request, with a test proving a stamped escape does not leak to the next construct in the same request.
- [ ] A missing target row does not authorize.
- [ ] Regression tests: a caller updating another user's row is refused; the legitimate owner is unaffected; a `clusterOwner` behaves as declared.
- [ ] `dsl/update_by_id_gate_2991_test.go`'s comment updated -- it currently says this cannot be fixed, and that stops being true here.
- [ ] Commit states the day-one coverage (13 concepts) and references #3059 as the insert-side hole.

## Unblocks

**#2991**, parts 2 and 3 -- `updateCalendarEvent`/`deleteCalendarEvent` (the targeting half; #2988 already closed the forging half) and `updateIdentity`/`updateAgent`. Both were blocked on this ruling by name. #2991's part 1 (`agentAuthorization`) is blocked on a separate wire-contract approval and is **not** unblocked by this.

**Frontend ping:** expected but bounded. Any client today updating a row it does not own on one of the 13 declared concepts starts receiving a refusal. That is the intended behaviour change and the reason this is a ruling rather than a refactor -- unlike #3076, this one is NOT a measured no-op.

---

## #3082 -- dsl/deployment has no live canonicalId call, so the remapped-ambient path is only fixture-tested (#3026 DoD item 5, second clause)

**Classification:** `bug`, `dsl`, `engine`

Split out of #3026 during the landing review of #3080, because closing it needs a decision rather than a fix.

#3026 DoD item 5 is a conjunction:

> - [ ] The scanner skips comments, **and** `dsl/deployment` carries a real `canonicalId` call so the path is live-tested.

The scanner half shipped in #3080 (both `//` and `/* */`). This is the second clause.

## Why it was not done in #3080

There is no live `canonicalId` call anywhere in `dsl/deployment` -- `grep -rn canonicalId dsl/deployment/` returns one line, and it is a comment. Neither of the two candidate call sites can take one without a decision above the issue:

- `createDeploymentNodeSpec` and `updateDeploymentNodeSpec` (`dsl/deployment/mutations.memql`) both derive the row id as `hash(concat(shortId(args.deploymentId), ":", args.nodeType))`. `shortId` is bare-out (#1859); `canonicalId` produces the prefixed `v1:cluster:deployment:<short>` form. Substituting it changes the string fed to `hash()`, so **every derived id changes** -- re-keying live rows, against a design that says "No id changes".
- The only alternative is adding a field to a shipped concept, which is a schema decision.

Verified during the #3080 review by an independent reproduction, not taken on faith.

## Why it still matters

`dsl/deployment` is the tree's **only** pinned domain (`find dsl -name namespace.pin` returns one file, contents `cluster`). The 14 live `canonicalId` calls all sit in `dsl/cognition`, which is unpinned -- so for them the declared namespace equals the directory and the rule #3026 shipped is indistinguishable from the one it replaced.

That means the **remapped**-ambient path -- directory differing from declared namespace, which is the entire subject of #2976 / #3017 / #3026 -- has no live caller in the tree. It is covered by unit fixtures and by a composition test through the production entry point against the real pin (`TestCanonicalId_InDomainComposesTheRealPin`, added in #3080's landing review), but not by the corpus itself. A future regression of the pin lookup would be caught by those tests and by nothing in the DSL.

## The decision

Pick one:

1. **Re-key.** Move a `deploymentNodeSpec` id derivation to `canonicalId` and accept the row re-key, with whatever migration that implies.
2. **Add a field.** Introduce a `canonicalId`-derived field on a `dsl/deployment` concept, leaving existing ids untouched. A schema change to a shipped concept.
3. **Accept the fixture coverage** and close this, on the grounds that the composition test through the real pin is sufficient and the corpus is the wrong place to force the shape.

3 is the cheapest and is defensible; 1 and 2 both buy a live regression guard at a real cost. Whichever is chosen, `dsl/deployment` should stop being the only place where a pinned domain's ambient path is untested by anything the loader actually walks.

Refs #3026 #3080 #2976 #3017 #1859

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

Q: Do you want option **3** (accept the fixture + composition-test coverage, close the DoD clause), or does the remapped-ambient path need a live corpus caller badly enough to pay option 1's row re-key or option 2's schema change?

## Why this is an ASK and not a build

The issue says it outright — *"closing it needs a decision rather than a fix"* — and the three options are not equivalent work:

- **1 (re-key)** changes every derived `deploymentNodeSpec` id, against a design that says "No id changes", and implies a migration for live rows.
- **2 (add a field)** is a schema change to a shipped concept.
- **3 (accept)** produces no diff at all; it is a decision to close a DoD clause.

Options 1 and 2 are yours by any reading. Option 3's *deliverable* is a close, which is not mine either. So there is nothing here I can build without first knowing which world I am in.

## Verified before asking — and the issue understates its own case

Re-derived at `origin/main`, because the whole issue is about counting coverage accurately:

| claim | measured |
|---|---|
| no live `canonicalId` in `dsl/deployment` | **confirmed** — one occurrence, and it is a `//` comment at `mutations.memql:154` |
| `dsl/deployment` is the tree's only pinned domain | **confirmed** — one `namespace.pin` in the whole tree, contents `cluster` |
| "the 14 live `canonicalId` calls all sit in `dsl/cognition`" | **off, in the direction that strengthens the issue**: there are **19**, in `cognition` (17: 15 in mutations, 2 in queries) and **`library`** (1, in automations) |

`dsl/library` is **unpinned**, so its call does not exercise the remapped path either. The conclusion holds and is slightly stronger than filed: **19 live calls, none of them in a pinned domain, so zero exercise the remapped-ambient path.**

## Recommendation: option 3, plus a cheap gate that answers the issue's real worry

The issue's closing sentence is the part I would not drop:

> Whichever is chosen, `dsl/deployment` should stop being the only place where a pinned domain's ambient path is untested by anything the loader actually walks.

Option 3 alone leaves that true. But it can be answered without paying 1 or 2's cost, by making the coverage relationship **explicit and enforced** rather than incidental:

A corpus gate asserting: *for every pinned domain in the tree, either it carries a live `canonicalId` call, or the composition test covers its pin.* Today `dsl/deployment` satisfies the second arm via `TestCanonicalId_InDomainComposesTheRealPin`. The gate fires the moment a **second** pin appears with neither — which is the actual regression risk, and the one nobody would otherwise notice.

That buys the durable guard the issue wants, costs no re-key and no schema change, and is honest about which arm is providing the coverage. **I can build it immediately if you take option 3** — it is small.

If you prefer 1 or 2, say which and I will build that instead; both are real work but well-specified.

## Blocked / state

The measurement above is the part worth keeping regardless of which option wins — in particular that the live-call count is 19 across two domains, not 14 in one.


---

## #3084 -- The signature-concept ambient path still uses the LAST path segment and has no scope check, so a nested file can bind a foreign namespace

**Classification:** `bug`, `dsl`, `engine`

Found during the landing review of #3080 (memql#3026). Pre-existing on `main`, untouched by that PR, and out of its scope -- filing rather than widening it.

## What

#3026 fixed the ambient rule for `canonicalId`. The **signature-concept** resolution path in the same file was not part of that issue and still has both of the defects #3026 removed from its neighbour:

- `component/memql/function_loader.go:247` and `:304` pass `DomainFromFilePath(origin)` -- the **LAST** path segment -- as the namespace hint. Boot assembles a concept id from the **FIRST** segment (`unified_loader.go`: `dir := firstPathSegment(p)`), so for `agents/tools/askSpecialist.memql` the hint is `tools` where assembly used `agents`.
- Line 304 (`nsHint := DomainFromFilePath(origin) // ambient same-domain scope (#2617)`) then feeds that hint to `resolveBareConceptNameWithNamespace` with **no scope check at all** -- there is no equivalent of the `idIsInDomainAmbientScope` gate the `canonicalId` path now carries.

Consequence: a nested `dsl/agents/tools/*.memql` carrying a signature like `mutate widget ...` can bind a foreign `v1:tools:widget` with no import. Same shape as the widening #3026 closed on the `canonicalId` path, one construct over.

## Reachability

Latent in-tree. The 23 nested `.memql` files live under `dsl/agents/{roles,skills,tools}` and no concept in the tree assembles under `v1:tools:*`, `v1:roles:*` or `v1:skills:*`. It becomes live the moment a `MEMQL_DSL_PATH` bundle claims a domain named `tools`, `roles` or `skills` -- none of which is a core domain, so `RegisterTree` permits it. That is the same cross-repo scenario #3026 was about.

## What the fix probably looks like

#3080 already added the pieces:

- `dslfs.RootDomainFromFilePath(path)` -- the FIRST segment, with the loader-origin decoration stripped (`component/memql/dslfs/domain.go`, tested).
- `declaredNamespaceForOrigin(origin)` -- that domain's `namespace.pin`, else the directory (`component/memql/canonical_id_refs.go`).
- `idIsInDomainAmbientScope(id, dir, declaredNS)` -- the inverse of `AssembleConceptIdFromDeclInDir`: admits the directory, a colon-extension of it, or the pin, and nothing else.

So the shape is: hand `:247`/`:304` the assembly directory instead of the last segment, and gate the ambient bind through `idIsInDomainAmbientScope` the way the `canonicalId` path does.

**Check before assuming it is a straight copy:** signature binding is what `resolveBareConceptNameWithNamespace` was originally written for, and #3017's argument was that `canonicalId` had been *stricter* than signature binding for no stated reason. Tightening this path is the opposite direction, so it needs its own decision about whether ambient signature binding across namespaces was ever intended -- and a regression sweep, since every `.memql` file with a signature concept goes through it.

## Definition of done

- [ ] `function_loader.go:247` and `:304` derive the hint from the origin's ROOT domain (pin-aware), not the last path segment.
- [ ] The ambient signature bind is gated by `idIsInDomainAmbientScope`, so a foreign namespace requires an explicit import.
- [ ] A test with a NESTED origin under a domain whose subdirectory name collides with a foreign namespace, failing before and passing after.
- [ ] A regression sweep over the tree: the full-tree load test plus `go test ./...`, since every signature-bound construct goes through this path.

Refs #3026 #3080 #3017 #2976 #2617 #2852

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

The hint change is correct and bites -- a nested agents/tools/ file is no longer steered toward `tools`. Gate 1 green, full go test ./... clean (131 ok, 0 FAIL).

But the new scope gate does not gate the bind. There are two resolution sites: :253 resolves the concept and rewrites the AST (setting a mutation write target and a query `concept ==` filter) and is UNGATED; :337 gates only the downstream BoundConcept field. Measured with a registry holding only the foreign concept: BoundConcept="" while MutationTemplate.Concept="v1:tools:widget" and the query still filters on it. This issue defect survives.

Worse, the refused case is now worse than main: an empty BoundConcept makes markSecretArgsFields no-op (disabling @secret argument redaction) and skips ensureBoundConceptFilter, while the foreign write still happens. The sibling canonicalId path ERRORS on the same refusal; the predicate was copied, the refusal semantics were not.

Also: the PR empirical basis is false. It says 17 nested files "therefore take this exact path"; all 17 import their signature concept, so 0 of 17 take the ambient path. The honest measurement is "0 of 528 signature-bound constructs bind ambiently across namespaces" -- same conclusion, without the false step.

This parks rather than being fixed in review because the remainder is the design the issue asked for, including a decision I should not take from the repo: whether refusal is a hard error (can refuse boot for an out-of-repo bundle) or a silent blank (silently disables secret redaction).


---

## #3089 -- GrammarVersion has not been bumped since 2026-07-21 despite repeated grammar narrowings, so the durable-rehydration stamp guard never fires

**Classification:** `bug`, `dsl`, `engine`

Found during the landing review of #3085 (memql#3028), and confirmed by independent reproduction. Filing rather than fixing there: the contract is systemically dead, not broken by that PR.

## The contract

`component/language/parser/grammar_version.go:9-13` states it plainly:

> `GrammarVersion` is bumped ON EVERY GRAMMAR EPIC -- a change that retires or reshapes an authored form (new invocation syntax, retired annotation, payload-binding change). Each bump MUST ship a `memqlmigrate --rewrite=<epic>` mode.

## What is actually true

`GrammarVersion` last changed in **cb62512c, 2026-07-21** (`"2026.08-doc-comment-descriptions"`). Since then, every one of these retired or reshaped an authored form and **none** bumped it or shipped a rewrite mode:

- `83574995` (2026-08-03) -- reject a repeated annotation argument instead of collapsing it (#2968). A previously-parsing form now errors.
- `d53bad46` / `489a414b` / `0d13dd96` / `93b365ed` (2026-07-21) -- bury `@role`, bury `@permission`, retire `@internal`, hard-retire eight expression builtins.
- `6e7d09ac` (2026-08-02, #3025) -- **added** the `asOf args.X` grammar.
- #3028 / #3085 -- removes the bare `asOf args.X` form again.

So the constant has been a dead letter for roughly six weeks, across at least six grammar moves in both directions.

## Why it matters -- the guard it disables

Durably-promoted authored constructs are stored as **source** and recompiled at boot (`component/memql/authoring_promote_durable.go`, `recompileAndPromoteRow`). Promotion stamps the row with `languageParser.GrammarVersion`. The guard reads:

```go
if row.GrammarVersion != "" && row.GrammarVersion != languageParser.GrammarVersion {
```

Its whole purpose is to catch "this row predates a grammar move" and quarantine it **with the migration command named, instead of whatever downstream parse error the stale source would produce**.

Because the stamp never moves, that comparison is always equal, and the guard never fires. Reproduced end to end during the #3085 review -- a row promoted with bare-`asOf` source, stamped with the current version:

```
GrammarVersion = "2026.08-doc-comment-descriptions"
recompile error = recompile query "skepticQ": parse error at line 7, column 71:
  asOf: `args.at` requires the `?? latest` fallback ... (memql#3028)
CONFIRMED: stamp guard did NOT fire -- raw parse error surfaced
CONFIRMED: quarantine does NOT fail boot (HasProblems=false)
```

And quarantines do not fail boot (`component/memql/engine.go:80-85`), so the construct simply **disappears on the next restart**, with an ERROR log and nothing else. That is precisely the failure mode the guard was built to prevent.

## Blast radius today

Low, and that is luck rather than design. The bare `asOf args.X` form existed only between #3025 (2026-08-02) and #3085, and no in-tree DSL uses it. The exposure is durably-promoted rows in a running deployment authored during that window, and rows authored before any of the other five narrowings.

## Definition of done

- [ ] Decide what `GrammarVersion` is for. Either it gates durable rehydration and must be bumped on every narrowing, or it does not and the comment plus the guard should be deleted rather than left looking load-bearing.
- [ ] If it stays: bump it, and record the narrowings since 2026-07-21 that it should have covered.
- [ ] If it stays: either ship the `memqlmigrate --rewrite=<epic>` modes the contract requires, or amend the contract to say when a rewrite mode is and is not required (a narrowing with no in-tree usage arguably needs none).
- [ ] A test that fails when a grammar narrowing lands without a bump, or an explicit written statement of why that cannot be automated. Without one this recurs -- it already has, six times.
- [ ] A test that the stamp guard fires: promote a row under version A, change the version to B, confirm the quarantine names the migration command rather than surfacing a raw parse error.

Refs #3085 #3028 #3025 #2992 #2968

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

PR #3134 parked and returned. Gate 1 green; Gate 2 found two blocking defects in the PR itself.

1. **The guard does not bite.** A real new struct-query clause (`project`) added to
   `parseStructQueryBody` ships past every guard in the PR with `GrammarVersion` untouched — the
   converse drift check iterates a hardcoded four-word denylist, so it can only catch four words
   nobody will add. The comment at `rewriter.go:833-835` claims the test catches this; it does not.
   Re-pinning the fingerprint literal alone also restores green unbumped, so DoD item 4 is unmet.

2. **The bump quarantines every stored authored construct.** `authoring_promote_durable.go:433`
   compares stamps and returns *before* attempting a compile, so every durable
   `v1:authoring:construct` row carrying the prior stamp is unregistered on first boot even though
   its source is still valid. The `memqlmigrate` mode the error names does not exist, and
   `grammarVersion` has no update path — only `createAuthoringConstruct` accepts it.

Both need a design decision with production consequences, which is why this is a human's call
rather than a review fix: does the stamp guard compare against a list of epochs that genuinely
require migration, or does this bump ship a restamp pass?

Full detail with the reproductions: https://github.com/znasllc-io/memql/pull/3134#issuecomment-5206098104


---

## #3093 -- The call-graph automation-condition rules are unreachable from CheckTree, so 33 authored automations are analysed by nothing

**Classification:** `dsl`, `engine`

Found while shipping #3043 (DoD item 5: "confirm zero findings is 'the tree is clean' rather than a second blind spot"). It is the same class as #3043, one construct over.

## The defect

`ConstructFindings` carries a live `case "automation"` arm — the P4/memql#2371 condition rules, `automation-condition-builtin` and `automation-condition-vocabulary` (`component/memql/callgraph/callgraph.go:421-438`).

`CheckFile` can never reach it:

```go
// component/memql/callgraph/tree.go
var restrictedKinds = map[string]string{
    "logic": "Logic", "query": "Query", "mutation": "Mutation", "action": "Action",
}   // <- no "automation"

func CheckFile(...) []Finding {
    kind := singular(base)
    if _, restricted := restrictedKinds[kind]; !restricted {
        return nil          // <- automations.memql exits here, before splitting
    }
```

So `CheckTree` — the whole-tree CI gate in `dsl/callgraph_contract_test.go` — analyses **0** of the tree's **33 automations across 14 `automations.memql` files**.

## Measured

Post-#3043, constructs actually split by the tree walk:

```
action        9
logic        33
mutation    215
query       201
automation    0     <- 33 declarations in the tree, none analysed
```

## Scope: a whole-tree-gate gap, not a total one

The rules ARE live on the authoring sandbox's define->promote path — `component/memql/authoring_sandbox_crossref.go:459` calls `ConstructFindings(c.Kind, ...)` directly, and its local `singular` maps `automations` -> `automation`. So a **newly authored** automation is checked; the **33 already in the tree** never were, and a direct edit to one is not checked either.

## Why nothing caught it

Identical to #3043: the tests bypass the entry point. `component/memql/callgraph/automation_conditions_test.go:11` calls `ConstructFindings("automation", ...)` directly, so the rules are proven correct against a code path the whole-tree gate never takes.

The `restrictedKinds` comment still says automations *"are the permissive composing construct (they may call anything), so they need no per-construct analysis"*. That was true when written; memql#2371 added condition rules that make it stale. The exclusion is now load-bearing in a way nobody intended.

Note the new `TestCallGraphCoverage` (shipped in #3043) does **not** catch this — it asserts coverage per *restricted* kind, and automation is not one. That is deliberate: adding it to `restrictedKinds` is the fix, not something a gate should assert before the decision below is made.

## The decision this needs

Turning the arm on may surface findings across 33 automations. Per the #3043 triage note, whether a real violation gets fixed, exempted or baselined is a separate decision from making the checker reachable — so this issue is the reachability fix plus a **reported** finding list, not a silent cleanup.

## Definition of done

- [ ] `automation` is reachable from `CheckFile`/`CheckTree` (it needs a header matcher and a `restrictedKinds` entry; note `dslspec` carries the `automation` keyword with `ConceptInSignature: false`).
- [ ] The stale `restrictedKinds` comment is corrected — automations are unrestricted in *what they may call*, which is not the same as being exempt from the condition rules.
- [ ] At least one test drives an automation-condition finding through `CheckFile`, not through `ConstructFindings` directly. That is the assertion whose absence hid this.
- [ ] Run `CheckTree("dsl/")` with the arm reachable and **report the findings in the thread**; do not silently fix or baseline them.
- [ ] `TestCallGraphCoverage` extended to automation once the decision above is made.

Refs #3043 #2371 #2041

---

## #3095 -- scripts/cidb: the db-tests coverage gate is one-directional -- deleting a TestMain leaves it green

**Classification:** `area:infra`, `reliability`

Found during the landing review of #3091 (issue #3030), by the tests lens of the
adversarial gate. Routed here rather than built into #3091 because it is new
behaviour #3030 never asked for, and that PR was already five review rounds deep.

## The gap

`scripts/cidb`'s coverage gate is **one-directional**. `coverageFindings`
(`scripts/cidb/dbgate_test.go:663`) asserts **provisioned -> in selector**: a package
with a `TestMain` calling `dbtest.EnsureSchema` must appear in the `db-tests` job's
selector. Nothing asserts the inverse -- **in selector -> provisioned**.

That inverse is exactly the memql#2551 lane-safety invariant the `TestMain` files
exist to satisfy.

## Proved, not argued

Deleting **all four** `main_dbschema_test.go` files added by #3091, while leaving
`.github/workflows/ci.yml`'s selector listing all four packages, leaves the gate
fully green:

```
ok  	github.com/znasllc-io/memql/scripts/cidb	0.227s
```

The central artifact of that PR can be removed wholesale and the gate it tightens
says nothing.

## Why it matters

Nothing else catches it either. Without a `TestMain`, the per-package binaries race
the shared migration, which fails *intermittently* in the lane with
`relation "MemoryNodes" does not exist` -- the exact flake `component/database/dbtest`
was written to kill (memql#2551). An intermittent red is the worst shape of failure
this lane can produce.

Note this is a **gap, not a false claim**: `scripts/cidb/doc.go:42-46` is explicit
that the rule is one-way ("having one and not being in the selector is always
drift"). Nothing in the tree lies about it.

## Suggested shape

`coverageFindings` already receives both `all` and `provisioned`, so the new rule is
a few lines: a selector-covered directory carrying db-gated tests but absent from
`provisioned`. It is unit-testable in `TestCoverageFindings`
(`scripts/cidb/dbgate_unit_test.go:1099`) with no new machinery, and the existing
`selfPkg` exemption (`dbgate_test.go:49`) must apply to it too.

## Acceptance

- [ ] `coverageFindings` gains the in-selector -> provisioned rule, honouring `selfPkg`.
- [ ] Unit test in `TestCoverageFindings` covering it.
- [ ] Proven to bite: deleting any one `main_dbschema_test.go` from a selector-covered
      package reds `go test ./scripts/cidb/...`, naming that package and the remedy.
- [ ] Restoring it goes green again.

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

The PR CLOSES this issue in substance, verified twice: with all seven main_dbschema_test.go deleted and the selector untouched, the old gate stays green (reproduced against origin/main) and the new one emits one finding per package. Deleting any one of the seven, individually: seven for seven. Zero false positives across the 18 packages the selector expands to.

It is parked because a comment I added during review cites :637-641 for a standard that lives at :648. I typed a line range from memory instead of reading it -- the seventh time this session I have asserted a value without fetching it, and the first time it landed in the very file whose purpose is making CI claims resolve.


---

## #3096 -- dbtest.EnsureSchema poisons MEMQL_DATABASE_DSN before pinging, turning its own failure into every downstream test's

**Classification:** `reliability`

Found during the landing review of #3091 (issue #3030). The concrete defect it caused
was fixed in that PR (commit 0ca597ca); this issue is the **structural** half, which
is why it is filed separately rather than widened into a PR already five rounds deep.

## The hazard

`dbtest.EnsureSchema` writes `MEMQL_DATABASE_DSN` into the process environment
**before** it pings, and unconditionally when the variable is empty
(`component/database/dbtest/dbtest.go:62-69`):

```go
dsn := strings.TrimSpace(os.Getenv("MEMQL_DATABASE_DSN"))
if dsn == "" {
    dsn = defaultDSN
    _ = os.Setenv("MEMQL_DATABASE_DSN", dsn)   // <- before any ping
}
```

Every db-gated test file reads the env first and falls back to its own literal only
when the env is **empty**. So the moment `EnsureSchema` writes a `defaultDSN` that
cannot connect, every downstream test in the package inherits that failure and its
own perfectly good fallback becomes unreachable dead code.

**`EnsureSchema` converts its own failure into every downstream test's failure.**

## What it already cost

`defaultDSN` carried password `memql_local_dev`, which matches nothing in this
project; the documented password is `memql_dev` (CLAUDE.md, README, quickstart,
`Makefile`, `scripts/k3d/seed-secrets.sh:71`, `.github/workflows/ci.yml:451`).

Measured on one throwaway `timescale/timescaledb-ha:pg16` as `memql/memql_dev` on
`:5432`, `MEMQL_DATABASE_DSN` unset, same tree, only the credential differing:

| `defaultDSN` password | result |
|---|---|
| `memql_local_dev` | **19 SKIP** (`SASL: FATAL: password authentication failed`, SQLSTATE 28P01) |
| `memql_dev` | **0 SKIP, 929 PASS** |

Under `MEMQL_REQUIRE_DB=1` the unfixed tree did not skip -- it **failed the package
outright**. CI could not catch any of it: `ci.yml` sets the DSN at job level, so the
`defaultDSN` branch never executes in the lane. The credential is fixed; the
amplifier is not.

## Suggested fix

Move the `os.Setenv` to **after** a successful ping. A `defaultDSN` that cannot
connect should not overwrite an empty env, so each package's own fallback survives
future drift. The Setenv's stated purpose -- aligning `NewMemoryNodesDatabase`
(which reads the env) with the DSN the tests use -- is only meaningful on the
reachable path anyway, which is the path that would keep it.

## Acceptance

- [ ] `EnsureSchema` sets `MEMQL_DATABASE_DSN` only after confirming the DSN connects.
- [ ] A test proving that an unreachable `defaultDSN` leaves the env untouched, so a
      package's own fallback is still reachable.
- [ ] Proven to bite: with `defaultDSN` pointed at a bad credential and a package
      fallback pointed at a good one, the package's tests still RUN.

---

## #3099 -- Several MemQL statement builders still render strings with %q, which the lexer refuses (the #3035 defect, elsewhere)

**Classification:** `bug`, `engine`

Found while landing #3092 (memql#3035), which fixed this defect in `component/outbound` only. The same defect is live on at least one other path that reaches the engine, and the escape-set definition is duplicated across several more.

## The defect

Go's `%q` and the MemQL lexer do not agree on the escape set, and the disagreement is a **hard error rather than a fallback**. `component/language/parser/lexer.go`'s `readString` implements the JSON escapes and only those (`" \ / b f n r t u`); `%q` emits `\x00`, `\a` and `\v`, none of which it knows. Verified against the real lexer:

```
%q("boom \x00 \a \v end")  ->  "boom \x00 \a \v end"
lexer: invalid escape character 'x' at position 7

langparser.QuoteString(same)  ->  "boom   \^G  end"
lexer: <nil>
```

So a single control byte in an interpolated string makes the whole statement fail to parse.

## Where it is still live

**Confirmed reachable — renders `%q` into a statement that is then executed:**

- `component/automations/steps/function.go` — `renderMemQLValue`'s string case is `fmt.Sprintf("%q", v)`; the result flows through `renderFunctionArgs` into the query string, which goes to `stepCtx.Engine.Execute`. The values are automation step function args resolved from prior step results and event payloads, i.e. exactly the arbitrary-text shape #3035 is about. **This is the closest analogue to the original bug** — a control byte in a string logic arg breaks the automation the same way it stuck the outbound row.
- `component/automations/steps/mutation.go` — `buildInsertQuery` uses `%q` for the concept, id, parent and aliasOf.
- `component/memql/liveknowledge/memql_connector.go` — `memqlQuote`'s string case is `%q`, substituted into a query template and executed.
- `sdk/go/client` — `support.go` plus the generated builders (`generated_mutations.go`, `generated_queries.go`, ...), emitted by `sdk/gen/emit_go.go`. Client-side, so the failure is a rejected query rather than a stuck row, but it is the same disagreement and there are on the order of a thousand call sites.

**Duplicate definitions, not currently defects** (they survive because the lexer accepts a *raw* control byte inside a literal, and `json.Marshal` escapes correctly — but they are extra definitions of one escape set, which is what #3035 showed is expensive):

- `integrations/knowledge/capabilities.go` — a hand-rolled escape table, used at roughly 45 call sites.
- `component/automations/steps/steps.go` — `jsonString`, `json.Marshal` with HTML escaping left on.
- `component/memql/tool_execution.go` — `encodeForMemqlSubstitution`.
- `component/memql/authoring_dryrun.go` — `renderDryRunMemQLValue`.

That list is **not asserted to be exhaustive** — two successive attempts to enumerate it during #3092's review were both incomplete. `grep -rn 'Sprintf("%q"' --include=*.go` over the statement-building paths is the reliable way to find them.

## The fix

`component/language/parser.QuoteString` already exists and is the single correct definition — it lives beside the lexer whose escape set it targets, which is the point. The reachable sites should call it.

Two things worth deciding rather than assuming:

1. **Do not simply port outbound's NUL substitution.** `QuoteString` renders NUL as an escape the lexer accepts but PostgreSQL's `jsonb` cannot store, so any of these paths that ends in a JSONB payload has the same second-layer failure (see the sibling issue for the inbound case). Whether to reject or substitute depends on whether the bytes are diagnostic or load-bearing.
2. **The SDK is a generator, not a file.** Fixing `sdk/go/client` means fixing `sdk/gen/emit_go.go` and regenerating, not editing generated output.

## Definition of done

- [ ] `component/automations/steps/function.go` renders string args through `QuoteString`, with a test driving a control byte through the real lexer that fails against `%q`.
- [ ] The other confirmed-reachable sites above are either converted or explicitly recorded as unreachable, with the reason.
- [ ] The SDK emitter is fixed at `sdk/gen/emit_go.go` and the client regenerated.
- [ ] Each converted site is checked for the JSONB/NUL second layer rather than assumed fixed by quoting alone.

Refs #3035, #3092

---

## #3101 -- event_payload_args: the block-structure scan still runs on raw source, so a `}` in a literal truncates an automation and a commented-out automation is rewritten

**Classification:** `bug`, `dsl`, `engine`

Found while building memql#3045, which fixed the two string-scan sites *inside* `scrub` / `mapCodeSegments`. Those two now share one literal- and comment-aware scanner. The **outer** block-structure scan in `scripts/migrations/event_payload_args/main.go` was not in that issue's scope and still runs against raw source, so the tool now disagrees with itself: the inner scans know what a literal is, the outer ones do not.

Both defects below are verified against `main` as of the #3045 fix, with the repros inline.

## 1. `matchingBrace` counts braces inside string literals

`matchingBrace` (main.go) is a bare depth counter with no literal or comment awareness. A `}` inside a string literal decrements the depth, so the automation block ends early and everything after it is invisible to the migration.

```memql
automation a {
  step s {
    label: "close } brace"
    field: event.payload.status
  }
  step t {
    other: event.payload.kind
  }
}
```

Actual output (`migrateFile`):

```memql
automation a {
  args {
    status any
  }

  step s {
    label: "close } brace"
    field: status
  }
  step t {
    other: event.payload.kind      <-- NOT migrated, NOT in args
  }
}
```

`kind` is neither collected into `args { }` nor rewritten. This is the same consequence as the #3045 defect and the same severity: a **silently missed rewrite in a tool documented to be run with `-write`**, whose output then fails the G5 gate for a read the tool was invoked to remove.

## 2. `automationHeader` matches inside a block comment

`automationHeader` is applied to raw `src` in `migrateFile`, so a commented-out automation is treated as a real one and its interior is rewritten.

```memql
/*
automation ghost {
  x: event.payload.gone
}
*/
automation real {
  y: event.payload.status
}
```

Actual output — the tool edits the inside of the comment and reports `automation ghost` as migrated:

```memql
/*
automation ghost {
  args {
    gone any
  }

  x: gone
}
*/
```

`argsHeader` has the same exposure inside `ensureArgs`.

## Why this was not folded into #3045

#3045's definition of done named the two scan sites in `scrub` and `mapCodeSegments`, the missing `/*` arm in those two, and a test file. Rewiring the block-structure scan is a different change with a different blast radius — it changes which spans of a file the tool considers an automation at all — so it was filed rather than smuggled in.

## Notes

- `scanSegments` (added by #3045, same file) already produces exactly the code/prose segmentation both sites need; it agrees with the G5 gate's own `scrubSourceForPayloadScan`. The fix is plumbing the existing scanner into the outer scans, not writing a third one.
- The real `dsl/` tree is already migrated and a dry run over it reports no changes, so neither defect is live against the current corpus today. Both are live for any tree the gate's error message sends an author to run this tool against.

## Definition of done

- [ ] `matchingBrace` ignores braces inside string literals and comments (drive it from `scanSegments`).
- [ ] `automationHeader` / `argsHeader` do not match inside a literal or comment.
- [ ] Tests covering both repros above, failing before and passing after.

---

## #3105 -- declared_usage_validator's struct-form keyword loop is dead code carrying the retired 'mutation' keyword

**Classification:** `bug`, `engine`

Found while landing #3094 (memql#3043). Deliberately **not** fixed there, because it is not the same defect and fixing it changes nothing observable — but it is a trap worth closing, and the framing matters, so the measurement is recorded here rather than lost in a review thread.

## What is there

`component/memql/declared_usage_validator.go` carries a struct-form header scan:

```go
for _, kw := range []string{"query ", "mutation ", "automation ", "spec ", "trait "} {
```

Its own doc comment, four lines above, says the recognised form is:

```
//   - `query NAME {` / `mutate NAME {` / `automation NAME {`
```

The comment says `mutate`; the code checks the retired `mutation`. `logic ` and `action ` appear in neither.

## It is NOT memql#3043 — the loop is unreachable

This is the important part, and it is why the issue is low priority rather than a repeat of #3043. In #3043 the broken matcher ran against 215 real declarations and enforced nothing. Here the arm guards a case that **cannot occur**, and the case that does occur is handled correctly by a different branch.

Traced and reproduced:

- `precededByBodyOpener` is reached only from `extractFunctionBody`, whose four callers are all validators fed `rawSourceForUsage`.
- `rawSourceForUsage` is assigned in `component/memql/function_loader.go` **after** `NormaliseAll` runs, so the snapshot is post-rewrite text, not authored text.
- The struct-form rewriter turns `query` / `mutate` / `logic` / `automation` into `func (Receiver) ...{`, and those keywords have no native top-level parser entry — an un-rewritten one cannot parse at all. `spec` / `trait` / `action` are native but produce `SpecDef` / `ActionDef`, never a `*FunctionDef`, so they never reach this validator either.

Empirically, on the real tree (81 construct files):

```
funcHits=46   keyword-arm hits=0   breakdown=map[]
```

And the decisive check — replacing the loop body with a panic and running the real suites:

```
ok  github.com/znasllc-io/memql/component/memql  53.9s
ok  github.com/znasllc-io/memql/dsl               1.7s
```

Green. The `dsl` suite loads the entire embedded tree. **The loop never matches once, anywhere.** Changing `"mutation "` to `"mutate "`, or deleting the loop outright, is a provable no-op today.

## Why it is still worth closing

The naming invites the exact change that would make it load-bearing. The file says it "runs on the RAW source", and `rawSourceForUsage` reads as authored text — but it is assigned below `NormaliseAll`. If anyone ever moves that snapshot above the rewriter (the natural reading of the name), the loop becomes live and **fails open on every mutation**: a header miss makes `extractFunctionBody` return `""`, and each caller then silently skips validation. That would disable `validateDeclaredUsage`, `validateLogicEventBinding`, `validateActorBinding` and `validateLogicEventFields` at once, quietly.

So: dead code carrying a retired keyword, one refactor away from being #3043 with a wider blast radius.

## Definition of done

Either is acceptable; the second matches the direction #3094 set.

- [ ] Delete the struct-form keyword loop entirely (proven a no-op above) and fix the contradictory doc comment, OR
- [ ] Source the keywords from the rewriter's own `StructFormKeywords` rather than a hardcoded list, so the set cannot drift from the rewriter again — the same single-sourcing #3094 applied to the call-graph checker.
- [ ] Fix the doc comment either way. It is internally contradictory: one line says these are "the parser-emitted forms **after** the struct rewriters run", and three lines later it lists the struct forms as recognised "pre-rewrite". The first is accurate.
- [ ] If the loop is kept, add a test that fails when it stops matching — its current unreachability is invisible, which is what let the retired keyword sit there.

Refs #3043, #3094, #2041

---

## #3108 -- The OpenAI embedding client puts the verbatim upstream response body into its error, and that error is logged at Warn by two engine call sites

**Classification:** `engine`

Found while researching memql#3052's definition-of-done item 1 ("determine whether `err` on the embed-failure log lines can carry embedded content"). This is the answer to that question, filed separately because it is a provider-level defect affecting every consumer of the embedding client, not the four CodeQL alerts #3052 is scoped to.

## The chain

`component/memql/openai_embedding.go:93` folds the **entire, unbounded, verbatim upstream response body** into the returned error:

```go
if resp.StatusCode != http.StatusOK {
    return nil, fmt.Errorf("embedding API error %d: %s", resp.StatusCode, string(respBody))
}
```

It propagates unbroken to a log sink:

| step | file:line | wrapping |
|---|---|---|
| body -> error | `component/memql/openai_embedding.go:93` | `"embedding API error %d: %s"` |
| `Embed` wraps | `integrations/embedding/embedding.go:134` | `"embed: compute embedding: %w"` |
| `storeHandler` wraps | `integrations/embedding/embedding.go:289` | `"store: %w"` |
| logged verbatim | `component/memql/executor_mutation.go:1086` | `e.Logger.Warn(…, "id", id, "error", err)` |
| logged verbatim | `component/memql/executor_mutation.go:1127` | `e.Logger.Warn(…, "id", id, "error", err)` |

The text submitted for embedding at those two sites is **user content**: `v1:harness:observation.content` and `v1:actions:action.intent`.

## Why this is worth recording

memql#2957 ruled on structurally the same shape and fixed it: the inbound handler stopped logging its engine error because the parser quotes source text in its diagnostics (`unexpected token after expression: %q`). This is the same pattern one layer out — an error that carries data the caller never intended to log.

**What I verified:** the response body reaches the log verbatim and unbounded. That is certain from the code above.

**What I did NOT verify, and what decides severity:** whether OpenAI's embeddings endpoint ever echoes the submitted `input` back in an error body. I could not establish that from this repo, and I did not want to assert it. Note the argument does not fully depend on it — logging an unbounded third-party response body verbatim is worth avoiding regardless of whether this particular vendor echoes input today, because that is a vendor behaviour memQL neither controls nor is notified about when it changes.

## Not the same thing as memql#3052

#3052 concerns CodeQL alerts 422-425 on this file. Those report taint from **`SiOpenaiApiKey`**, not from the response body, and they are 4 of ~492 `go/clear-text-logging` alerts on `main` in one systemic family. This issue is a hand-found defect on a different path; fixing it would not clear those alerts, and dismissing those alerts would not fix this.

## Suggested shape

Bound it at the source rather than at each sink, so no consumer can leak it: keep the status code, and either drop the body or truncate it to a short, fixed budget. One change covers every caller of the embedding client instead of one log line at a time. Testable directly with an `httptest` server returning a non-200 with a body, asserting the body does not survive into `err.Error()`.

## Definition of done

- [ ] A non-200 from the embeddings endpoint does not put the verbatim response body into the returned error.
- [ ] A test pins it (httptest server, non-200 + body, assert the body is absent from the error).
- [ ] Decide whether the same treatment is owed to the sibling error paths in that file (`parse embedding response`, `read embedding response`).

---

## #3111 -- @secret is not redacted by the automation args binder, which also writes the value to a WARN log

**Classification:** `engine`, `security`

Found during the landing review of #3097 (issue #3036). That PR added `@secret` redaction to the
**function-args validator** and now names this surface as explicitly unenforced; this issue carries
the enforcement.

## The gap

`component/automations/args_binding.go` is a second args validator. Its own comment says it mirrors
"the memql function validator's rule set (presence / type / enum / maxLength / pattern)". It quotes
rejected values into error messages at three sites:

- `args_binding.go:115` — `value %v is not one of the allowed values %v`
- `args_binding.go:119` — `value too long (%d runes, max %d)`
- `args_binding.go:125` — `value %q does not match pattern %q`

`component/automations.ArgsField` (`component/automations/types.go:160-188`) has no `Secret` member
and the automation contract carries no concept binding, so nothing there *can* be redacted.

## Why a concept row's secret reaches it

`component/memql/executor_mutation.go:805-819` flattens the stored row payload into the
`graph.node.created.<concept>` event payload (`maps.Copy(eventPayload, payloadMap)`).
`bindEventArgs` reads `event.Payload` by declared field name and `validateAutomationArg` quotes the
value.

Reproduced during review, with a payload shaped exactly as `executor_mutation.go` builds one:

```
ENUM PATH:    arg "apiKey": value sk-live-SUPERSECRET-abc123 is not one of the allowed values [tok_a tok_b]
PATTERN PATH: arg "token": value "sk-live-SUPERSECRET-abc123" does not match pattern "^tok_[a-z]+$"
```

**And it reaches a structured log**, which matters because #3036's original ruling assumed no log
site carries a row payload. `args_binding.go:285` writes the same reason string:

```
{"level":"WARN","msg":"automation refused to fire: event payload violates its declared args contract ...",
 "automation":"rotateCredential","topic":"graph.node.created.v1:identity:credential","field":"token",
 "reason":"value \"sk-live-SUPERSECRET-abc123\" does not match pattern \"^tok_[a-z]+$\""}
```

## Latent, not live

Two things gate it today: no concept in `dsl/` carries `@secret` (all hits are in the `_`-prefixed
reference skeleton the loader skips), and it additionally needs an automation whose args field name
matches the secret concept field and carries `@enum`/`@pattern`. So this is a real path with no
current traveller — worth closing before one appears rather than after.

## Why it was not fixed in #3097

`args_binding.go` predates it (#2363) and the fix is a genuine scope expansion: the automations
package needs a `Secret` flag plus a concept binding it does not have, and it deliberately avoids
importing `component/memql` to dodge an import cycle. #3097 corrected the *promise* — all three
documents and the `SecretFields` doc comment now name this surface as unenforced.

## Acceptance

- [ ] An automation args field derived from a concept field annotated `@secret` has its rejected
      value replaced in all three messages at `args_binding.go:115/:119/:125`.
- [ ] The WARN log at `:285` carries the redacted reason, not the raw one.
- [ ] A test driving a real `graph.node.created` event payload through `bindEventArgs`, proven to
      bite by removing the redaction.
- [ ] The "NOT REDACTED BY THE AUTOMATION ARGS BINDER" paragraphs added by #3097 to
      `dsl/_reference/_concept.memql`, `docs/public/language/attribute-matrix.md` and
      `docs/public/language/reserved.md` are updated, and the corresponding assertion in
      `TestSecretEnforcementIsRealAndScoped` is moved from "must name as unenforced" to enforced.

---

## #3112 -- @secret is not redacted by concept payload JSON-schema validation (@minimum / @maximum / @format)

**Classification:** `engine`, `security`

Found during the landing review of #3097 (issue #3036). That PR added `@secret` redaction to the
**function-args validator** and now names this surface as explicitly unenforced; this issue carries
the enforcement.

## The gap

Concept payload validation runs the concept's JSON schema over the row
(`Concept.Create`, `component/database/memory-nodes/concept.go:205`, wrapping as
`concept payload validation failed: %w`). The `santhosh-tekuri/jsonschema/v5` messages interpolate
the **instance value** for the keywords `concept_parser.go` emits from `@minimum` / `@maximum` /
`@format`.

Reproduced during review:

```
MINIMUM VIOLATION: jsonschema: '/secretPin' does not validate with v1:identity:credential#/properties/secretPin/minimum: must be >= 100000 but found 4242
FORMAT VIOLATION:  jsonschema: '/rotatedAt' does not validate with .../rotatedAt/oneOf/0/format: 'sk-live-SUPERSECRET' is not valid 'date-time'
Create() VERBATIM: concept payload validation failed: jsonschema: '/secretPin' does not validate with .../minimum: must be >= 100000 but found 4242
```

`x-secret: true` sits in the **same schema object** the validator is reading — the information is
present and unused.

## Why this is the largest of the uncovered surfaces

#3097's redaction only covers constraints the **args block** declares. `@minimum`, `@maximum`,
`@format` and `@minLength` are declared on the **concept** and enforced *only* here. So for any
constraint a concept declares that its mutation's args block does not mirror, the redaction is
bypassed entirely and the raw value is quoted — and unlike the automation path
(#see the args-binder issue), this needs no automation and no matching arg name.

## Latent, not live

No concept in `dsl/` carries `@secret` today (all hits are in the `_`-prefixed reference skeleton
the loader skips), so nothing currently travels this path.

## Why it was not fixed in #3097

The fix is real work in a different package on pre-existing behaviour: walk the
`*jsonschema.ValidationError` tree, map each `InstanceLocation` back to an `x-secret` property in
the schema being validated, and rewrite the leaf message. #3097 corrected the *promise* instead —
all three documents and the `SecretFields` doc comment now name this surface as unenforced.

## Acceptance

- [ ] A `@secret` concept field's value is replaced in `concept payload validation failed: ...`
      messages for the value-interpolating keywords (`minimum`, `maximum`, `format`, and any other
      keyword whose jsonschema message carries the instance).
- [ ] Non-secret fields' messages are byte-identical to today.
- [ ] A test driving `Concept.Create` with a `@secret` numeric field violating `@minimum`, proven to
      bite by removing the redaction.
- [ ] The "NOT REDACTED BY CONCEPT PAYLOAD VALIDATION" paragraphs added by #3097 to the three
      documents are updated, and the corresponding assertion in `TestSecretEnforcementIsRealAndScoped`
      is moved from "must name as unenforced" to enforced.

---

## #3113 -- Nothing in the DSL tree carries @secret, so #3036's redaction protects zero fields

**Classification:** `dsl`, `security`

Found during the landing review of #3097 (issue #3036), by two independent review lenses. Filed
because it is a product decision rather than a defect, and because #3036 shipped enforcement that
currently protects nothing.

## The observation

`grep -rn "@secret" dsl/ --include=*.memql` returns hits in **exactly one file**:
`dsl/_reference/_concept.memql`. That directory is `_`-prefixed and skipped by the loader
(`component/memql/dslfs/walker.go:40,:49`; `dsl/runtime_mount.go:62`) — it is an authoring skeleton,
never loaded.

So `markSecretArgsFields` never stamps anything on this tree and #3036's redaction branch never
fires in production today.

## Why that is worth a decision rather than a shrug

The concepts that actually hold credentials are **not** annotated:

- `dsl/platform/concepts.memql` — `globalSecret.encryptedValue` and `partitionSecret.encryptedValue`,
  both described in-schema as *"Opaque -- never rendered to humans"*, carry no `@secret`.
- the `v1:identity:identity` credential family is likewise unannotated.

That cuts both ways, and both directions are worth stating plainly:

- **#3097 is near-zero-risk to ship** — no live concept changes behaviour, which is part of why it
  landed.
- **#3036's stated motivation is not yet served.** That issue argues from failure asymmetry: a wrong
  belief about `@secret` produces a credential in an operator-visible string, and that is
  unrecoverable because you may never learn you needed to rotate. The annotation now does something
  — on zero fields.

## What this issue is asking

Decide, deliberately, which concept fields should carry `@secret`, and annotate them. The candidates
above are the obvious starting set, but the decision is genuinely a product one: annotating a field
changes what appears in operator diagnostics, and the reviewer of that change should be someone who
knows what those diagnostics are used for.

Note the scope caveat #3097 documented: matching is by **argument name**, not by write target, so
annotating a concept field only redacts a mutation arg that happens to share its name. Two further
surfaces stay unredacted regardless — #3111 (the automation args binder, which also logs the value)
and #3112 (concept payload JSON-schema validation). Annotating a field is therefore worth doing
*and* is not by itself sufficient.

## Acceptance

- [ ] A decision recorded on this issue naming which fields get `@secret` and which deliberately do
      not, with the reason.
- [ ] Those fields annotated.
- [ ] For at least one annotated field, a test showing a rejected value is redacted through the real
      load path (the pattern in `component/memql/function_secret_loader_test.go`).
- [ ] The interaction with #3111 / #3112 acknowledged so nobody reads the annotation as blanket
      protection.

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

Building. The decision turned out to be answerable **from the repo** rather than from intent — and the research overturned the issue's central worry.

**The worry is void.** The issue asks for a reviewer "who knows what those diagnostics are used for", because annotating changes them. It does not: every redaction site in `function_validator.go` needs a constraint to reject against (`@enum`, bounds, `@format("date-time")`, `@pattern`), and both `encryptedValue` args are unconstrained `string!`. **Zero diagnostics change.** There is no trade-off to weigh.

**A field the issue's candidate list missed** — `v1:identity:authCode.code`, a **cleartext** one-time OAuth code, top-level, with a direct arg-name match on `createAuthCode`. It is the only stored-plaintext credential in the tree; the named candidates are ciphertext.

**Two candidates are already decided in-schema, against annotating:**
- `fingerprint` — *"For UI display only; lets operators tell rotated secrets apart"*. It exists to be shown.
- the `v1:identity:identity` credential family — every field is a SHA-256 digest (*"Plaintext never stored"*), **and** nested under `credentials`, which `SecretFields()` (top-level only) structurally cannot see.

files: `dsl/platform/concepts.memql`, `dsl/identity/concepts.memql`, `component/memql/platform_secret_annotation_test.go`


---

## #3114 -- agentRole slug is documented as canonical but nothing enforces uniqueness, so the shadow row can still be minted

**Classification:** `dsl`, `engine`, `security`

Split from #3066, which took the narrow half. That fix makes the agent factory prefer a predefined row when a slug is shadowed, which closes the branding / prompt / policy substitution. It does **not** stop the shadow row from being created, and the concept's own documentation still says something untrue.

## What is still true after #3066

`v1:agents:agentRole.slug` documents itself as *"Canonical slug... stable, never renamed"*, and `createAgentRole` opens:

```
insert {
  id: args.agentRoleId ?? args.slug
  ...
}
```

So a caller passing an explicit `agentRoleId` alongside a slug that a seeded row already owns still mints a second active row carrying that slug. Nothing rejects it:

- `validateAgentRolePredefinedLock` (#3061) keys on the row **id**, and there is no prior row at the new id;
- the merged payload's `predefined` is `false`, so it is an ordinary user-role write by that guard's own contract;
- `activeAgentRoles` is `@public` and unscoped, so both rows are in every catalog read.

#3066 makes the factory resolve that collision safely. It leaves the collision.

## Why that remainder is worth closing

**The doc is false.** "Canonical" and "never renamed" describe a unique key, and the schema does not have one. An author reading that field will reasonably assume slug identifies a role.

**Other slug consumers are not covered.** #3066 hardened exactly one lookup, `integrations/agents/factory.go findRoleBySlug`. The preference lives in that function, so any other code path resolving a role by slug — existing or future — inherits none of it. `agentRoleBySlug` is a `@public` query and its own doc says it is used by the role-catalog seeder's idempotency check and the agent-creation path.

**The role picker shows both.** `activeAgentRoles` returns two rows with one slug, so a user-facing catalog can display a duplicate whose name is attacker-chosen.

## Options

1. **Write-time uniqueness on `(slug, active)`** in `createAgentRole` / `updateAgentRole` — rejects the second row at the source. Needs a read-before-write in the mutation path or a Go-side validator alongside `validateAgentRolePredefinedLock`, which is where the sibling rule already lives.
2. **Load-time uniqueness check** over the seeded catalog only — cheap, but it catches seed-vs-seed collisions rather than the user-minted case that #3066 is about.
3. **Derive the id from the slug unconditionally** — drop the `args.agentRoleId ??` opener so one slug can only ever be one row. Smallest schema-level change and it makes the doc true by construction; needs a check that nothing depends on supplying an explicit id (the seeder does pass ids today).

Option 3 is the one that makes `slug` genuinely canonical. Option 1 is the most conservative.

## Definition of done

- [ ] A recorded decision among the above.
- [ ] A test that minting a second **active** row on an existing slug is refused, driven through the mutation path rather than by asserting a validator in isolation.
- [ ] The seeder still works — it is the caller that legitimately passes explicit ids, and it re-runs on every startup, so an idempotent re-seed must not trip the new rule.
- [ ] `dsl/agents/concepts.memql`'s `slug` description is either true or corrected to say what is actually enforced.
- [ ] #3066's preference in `findRoleBySlug` stays — defence in depth, not replaced. Note in its comment that uniqueness now backs it.

Refs #3066 #3061 #2985

---

## #3116 -- blankCommentsAndStrings ends a string at a newline and claims the lexer does too; the lexer accepts multi-line literals

**Classification:** `bug`, `dsl`, `engine`

Found while landing #3100 (memql#3046), where a draft of the fix delegated argument splitting to this function and inherited the bug. That delegation was reverted, so **#3100 is not affected** — but the underlying claim is false for every other caller, and they were not examined.

## The false claim

`component/language/parser/rowauthz_binding.go`, in `blankCommentsAndStrings`:

```go
case c == '\n':
    // An unterminated string ends at the newline, the same recovery the
    // lexer performs.
    state = code
```

The lexer performs no such recovery. Measured through the real `Lexer`:

```
src = "\"line one\nline two\""          (a real newline, not an escape)

LEXER: token type=string literal="line one\nline two"     <- ONE token, spans the line
blankCommentsAndStrings(src) = "\"        \nline two\""   <- string closed at the newline
```

The lexer **accepts** a string literal spanning lines. memql#3047 is a separate open bug about the line *counter* not advancing through exactly such a literal — that bug only exists because these literals are accepted and tokenized.

So the comment describes behaviour the lexer does not have, and the function diverges from the language it is scanning.

## Why it matters

A multi-line string literal makes the blanker fall out of string state early. Everything after the newline is then scanned as **code**, and the literal's real closing quote **opens** a new string. From there the blanker's view of the file is inverted — code is treated as string and string as code — until the next quote resynchronises it.

Consequences by caller, none of which I have verified in depth (that is the work this issue asks for):

- `rowauthz_binding.go:287` and `:318` — the row-authz binder decides what it is looking at from the blanked text. A multi-line literal anywhere earlier in the file could shift its view of every subsequent construct.
- `rowauthz_binding.go:459` `BlankCommentsAndStrings` (exported) — used by the `memqlmigrate` codemod. A codemod that mistakes string content for code can **rewrite inside a string literal**, which is a silent source corruption rather than a failed run.

Measured on the current tree the risk may be zero — but "no authored file triggers it today" is exactly the standard #3046 was filed under, and it armed on the first Windows path.

## What is probably right

Two options, and the choice needs someone who knows the binder's intent:

1. **Match the lexer** — let a string span newlines, and treat a genuinely unterminated literal as running to EOF. This makes the function agree with the language, and makes the comment true.
2. **Keep the recovery deliberately**, if the binder wants line-bounded resynchronisation for error tolerance — but then say so honestly, state that it diverges from the lexer, and name what breaks when it does.

Either way the comment must stop claiming parity with the lexer, because that claim is what would stop the next reader checking.

## Definition of done

- [ ] The comment states what the function actually does, and whether it agrees with the lexer.
- [ ] A decision recorded for option 1 or 2 above, with the reason.
- [ ] A test drives `blankCommentsAndStrings` over a multi-line string literal and pins the chosen behaviour — there is currently no such case (`rowauthz_binding_test.go`'s offset-preservation fixtures are all single-line).
- [ ] Assess the two consumer paths: whether a multi-line literal can shift the row-authz binder's view of later constructs, and whether the `memqlmigrate` codemod can rewrite inside one. Those are the outcomes that would be silent.

Refs #3046, #3047, #3100

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

Report confirmed, and it **understates** the divergence. Read from `scanString` directly (`component/language/parser/lexer.go:475`):

- a newline inside a literal is written to the builder as an ordinary rune — multi-line literals are **accepted**;
- an unterminated literal is a **hard error** (`unterminated string starting at position %d`), not a recovery.

So the comment is wrong twice: the lexer neither ends the string at a newline **nor** recovers at all. #3047's fix — the line counter that now advances through such literals — exists precisely because they are legal.

## The consequence is not hypothetical

`ConceptHeaders` documents this guarantee: *"the word 'concept' inside a doc comment or a @description string can never be mistaken for a declaration."*

Measured against a **file that lexes cleanly**:

```
@description("a description that wraps
concept phantom { evil string }")
concept real { ... }

HEADER name="phantom" start=39   <- inside the string literal
  HEADER name="real"    start=73
  LEX: clean
```

The blanker desyncs in **both** directions at once: string content is exposed as code, and the real `)` after the literal is swallowed as string content. `memqlmigrate`'s codemod inserts `@rowAuthz` relative to a header's `PreambleStart` — so a phantom header is an **insertion inside a string literal**. That is the silent source corruption the issue predicted, now with a repro.

## Decision: option 1 (match the lexer)

Not an intent judgement in the end. Option 2 trades correctness on **valid** input for tolerance of input that **does not lex** — and since an unterminated literal is a lexer error, no consumer meaningfully runs on such a file anyway. There is nothing to be tolerant of.

files: `component/language/parser/rowauthz_binding.go`, plus tests


---

## #3117 -- @secret is not redacted by validateToolArgs, which WARN-logs the entire args map and runs before the covered validator

**Classification:** `engine`, `security`

Found during the landing review of #3097 (issue #3036) — by the audit of the *review fix*, after
four review lenses and three skeptic passes had all enumerated the engine's validators and all
missed this one. It is the reason #3097 is parked.

## The gap

`MemQLEngine.validateToolArgs` (`component/memql/tool_execution.go:66`) is a **fourth** args
validator, and it is not a separate schema — it is compiled from the *same* `ArgsSchema` that
carries the `Secret` flag #3036 added:

```
component/memql/function_tools.go:55:  inputSchema, err := toolInputSchemaFromArgs(fn.ArgsSchema)
component/memql/engine_bootstrap.go:243: registerFunctionTools(e.Logger, functionRegistry, toolRegistry)
```

`registerFunctionTools` auto-generates a tool for every enabled query and mutation, and
`jsonSchemaForArgsFieldBase` carries `enum`, `minimum`, `maximum` and `format` across. So every
function whose args #3036 redacts has a tool twin whose args are **not** redacted.

## Why it is the worst of the four surfaces

**It logs the entire args map, not just the rejected field** —
`component/memql/tool_execution.go:113-116`:

```go
argsJSON, _ := json.Marshal(args)
e.Logger.Warn("tool args validation failed",
    "tool", tool.Name,
    "args", string(argsJSON),
```

Every other known surface quotes one rejected value. This one serializes the whole payload,
including arguments that did not fail validation.

**It runs FIRST on the agent path.** `tool_execution.go:366` calls it before `handler :=
tool.Handler`, so on an LLM tool call an enum violation on a `@secret` argument is rejected and
logged in full **before** `function_validator.go:182` — the site #3036 redacts — is ever reached.

**Its message goes to the model.** `formatToolValidationError` (`tool_execution.go:256`) performs no
redaction, and the result is returned with `IsError: true` so the streaming tool loop hands the text
to the LLM as the tool's response. The jsonschema messages interpolate the instance value for the
same keywords as #3112 (`schema.go:598` `must be >= %v but found %v`, `:311` `%v is not valid %s`).

## Latent, not live

No concept in `dsl/` carries `@secret` today (see #3113), so nothing currently travels this path.

## Why #3097 is parked rather than extended

#3097's non-negotiable DoD is that the documentation state the enforced surface and name the
unenforced ones so a reader cannot infer blanket coverage. My review fix asserted "the engine has
THREE validation surfaces and @secret reaches ONE" in four places. That is false — there are four,
and the fourth is this one. Correcting the count is not a word change: the scope story needs
re-deriving from an exhaustive enumeration of validators rather than from an incremental sweep,
which is how three rounds of this documentation each ended up incomplete.

## Acceptance

- [ ] A `@secret` argument's value is redacted from `validateToolArgs`' returned message.
- [ ] The WARN at `tool_execution.go:113-116` does not serialize secret argument values — either
      redact per-key from the `ArgsSchema`, or drop the `args` attribute.
- [ ] A test driving a real auto-registered function tool with a `@secret` arg through
      `validateToolArgs`, proven to bite.
- [ ] The scope documentation in `dsl/_reference/_concept.memql`,
      `docs/public/language/attribute-matrix.md`, `docs/public/language/reserved.md` and the
      `Concept.SecretFields` doc comment states the correct number of surfaces and names this one.
- [ ] `TestSecretEnforcementIsRealAndScoped` gains an axis for it.

---

## #3120 -- The one-byte-lookback string-scan bug survives in 11 more places after #3045; nothing stops a twelfth

**Classification:** `engine`, `reliability`

Found during the landing review of #3102 (issue #3045). That PR fixed the last two copies inside
`scripts/migrations/event_payload_args`. **Eleven more survive**, and the enumeration is the point:
this bug has now been fixed one site at a time in memql#2949, memql#2872 and memql#3045, and each
fix left the rest of the tree untouched because nobody listed them.

## The bug

Deciding whether a `"` closes a string literal by looking **one byte back** for a backslash:

```go
if c == '"' && (i == 0 || line[i-1] != '\\') { ... }
```

That cannot distinguish an escaped quote (`\"`) from a quote following a **completed** escape
(`\\"`). On a literal ending in `\\` the scanner runs past the real closing quote and treats the
following code as literal interior — so a scanner skips code, a validator misses a violation, or a
rewriter corrupts a file, depending on what it feeds.

## The eleven sites

Enumerated with `grep -rnE "\[[a-z]+-1\] *[=!]= *'\\\\'" --include=*.go`, verified at each line:

| file:line | what it scans |
|---|---|
| `component/language/pagination/checker.go:326` | `stripComment` |
| `component/automations/evaluator.go:620` | quote split (its own comment says "Basic handling") |
| `component/automations/steps/shape.go:511` | quoted-string scan |
| `component/automations/steps/shape.go:596` | paren scan |
| `component/automations/steps/shape.go:644` | brace scan |
| `component/automations/steps/shape.go:680` | bracket scan |
| `component/language/parser/rewriter.go:2606` | depth scanner |
| `component/memql/dependency_validator.go:334` | `stripDepLineComment` |
| `component/memql/declared_usage_validator.go:120` | brace scanner |
| `component/memql/sense/tokenize.go:355` | `isInsideString` (self-described "simple heuristic") |
| `dsl/conformance_test.go:1760` | `stripLineComment` |

Four of them are in `component/automations/steps/shape.go` alone.

**`component/language/parser/rewriter.go:2606` additionally has no `i == 0` guard** — `s[i-1]` at
`i == 0` would panic. It is safe today only because the surrounding state machine cannot have
`inStr` true at index 0. That is an invariant nothing asserts.

## Two correct implementations already exist

Neither uses a lookback; both are proper state machines:

- `component/language/parser/rowauthz_binding.go:459` — `BlankCommentsAndStrings` (consumes the
  escaped byte, handles `/* */`)
- `component/memql/baseparser/comment_blank.go:36` — `BlankComments`
  (`stateString` / `stateBacktick` / `stateBlockComment`, `if c == '\\' { i++ }`)

`#3102` also added a third, `scanSegments`, deliberately — its doc block records why delegation did
not fit that caller (both existing helpers return a *blanked copy*, which serves a scrubber but not
a segment mapper that must splice original bytes).

## Why this is filed rather than swept

Each of these has a different caller, a different notion of what a "segment" is, and a different
blast radius — `sense/tokenize.go` feeds editor highlighting, `dsl/conformance_test.go` gates CI,
`shape.go` parses live automation steps. A blanket find-and-replace across eleven call sites with
different semantics is exactly the change that should not be made in one pass without per-site
tests.

Note also that `#3102`'s doc comment says "TWO CORRECT implementations exist", which reads as though
the tree is otherwise clean. It is not — that phrasing is accurate about the *correct* ones and
silent about the eleven buggy ones.

## Acceptance

- [ ] Each of the eleven sites either delegates to a shared correct scanner or tracks escape state
      itself, decided per site with the reason recorded.
- [ ] `rewriter.go:2606` gets an `i == 0` guard or an assertion of the invariant that makes it safe.
- [ ] Each converted site gets a test covering a literal ending in a completed `\\` escape, proven
      to bite.
- [ ] A gate (a test, or a lint) that fails on a NEW one-byte-lookback quote scan, so the twelfth
      copy cannot be written. Without this the issue recurs — it already has, three times.

### Later findings

_Investigation recorded after the issue was written. Where it contradicts the description above, it is the more recent assessment._

**(1)**

## Correction: it is **nine**, not eleven

Two of the listed sites are **already fixed** — the table was written against a pre-#3046 tree and #3046 has since merged:

- `component/automations/evaluator.go:620` (`splitCoalesceArgs`) — tracks escape state
- `component/language/parser/rewriter.go:2606` (`splitTopLevelArgs`) — tracks escape state

That also makes the second acceptance item **moot**: the `i == 0` guard was wanted for a lookback that no longer exists — the code is a state machine now.

Verified by reading both, not by trusting the grep: the remaining hits at those paths are the doc comments describing the fix.

## The nine, grouped by decision

**A. Three byte-identical `strip*Comment` copies** — `pagination/checker.go:326`, `memql/dependency_validator.go:334`, `dsl/conformance_test.go:1760`. Not similar, **identical**. Consolidating into one `baseparser.StripLineComment`: three copies of one function is the mechanism that produced this issue three times, and `component/language/parser` already imports `baseparser`, so the layering precedent exists.

**B. `shape.go` ×4** (511 / 596 / 644 / 680) — track locally, **cannot** delegate: this parser accepts BOTH `"` and `'`, and the shared blankers handle `"` only. Routing single-quoted literals through one would silently stop treating them as strings — the same reason `splitCoalesceArgs` recorded for staying local. The three depth scanners (paren/brace/bracket) are structurally identical and get one local helper.

**C. `declared_usage_validator.go:120`** — track locally; it produces offsets into the original and carries a body-opener heuristic a blanked copy cannot serve.

**D. `sense/tokenize.go:355`** — track locally.

**Plus the gate**, which is the item that stops a twelfth.

files: the nine above, `component/memql/baseparser/`, plus tests

**(2)**

Work is complete and pushed (`ac8e39ea`); **holding the claim because CI cannot currently produce a verdict.**

## This is a GitHub Actions outage, not the diff

Every failing job dies in **`Set up job`**, before any repo code runs:

```
Getting action download info
Failed to resolve action download info. Error: Service Unavailable
##[error]Service Unavailable
```

Evidence it is infrastructure:

- It hits **other sessions' PRs** in the same window — #3150 and #3137, on unrelated branches.
- `build node tags (cognition)` **passed** while `agent` / `planner` / `workbench` failed on the identical commit. Same code, different runner.
- Several jobs fail at exactly `15m2s` — a timeout, not an assertion.
- Locally, **every** node tag builds: `bff cognition agent planner identity workbench mcp` all clean (`voice` needs `soxr`, absent from this machine and unrelated).
- `go test ./...` is **exit 0** on this branch.

Earlier the `changes` gate itself failed, which is why `go-checks` / `db-tests` / `mcp-conformance` showed as *skipping* — they never ran at all.

I pushed an empty commit to force a fresh run rather than re-running stale jobs; that got the real lanes to start, and the outage then took most of them again.


---

## #3123 -- A branch-less @variant(discriminator=...) is silently dropped at every depth

**Classification:** `dsl`, `engine`

Found during the landing review of #3104 (issue #3049), by two independent lenses. Not fixed there
because it is depth-independent and predates that guard.

## The gap

`@variant(discriminator="kind")` written WITHOUT its branch block is silently dropped — the
discriminator never reaches the schema.

`valueAnnotationNames` (`component/database/memory-nodes/concept_parser.go`) keys `@variant` off
`len(prop.variants) > 0`, i.e. off the branch block rather than the attribute. And the discriminator
itself is only harvested inside `if len(prop.Variants) > 0` (`concept_parser.go:441-450`). So a
branch-less `@variant` populates neither, and #3049's new composite guard does not see it either.

Measured through the real parser:

```
[][]object @variant(discriminator="kind")   -> {"items":{"items":{"type":"object"},"type":"array"},"type":"array"}
[]object   @variant(discriminator="kind")   -> discriminator absent
object     @variant(discriminator="kind")   -> discriminator absent
```

Same outcome at every wrapping depth, which is why this is not a regression of #3049 — that issue
made the composite case *expressible enough to notice*, and this is the one spelling its guard
still lets through.

## Why it is worth closing

`@variant` is the annotation #3049 called its worst case, precisely because a dropped union means a
row validates against nothing. A branch-less `@variant` is a plausible authoring mistake — the
author has said "this field is a discriminated union" and gets a schema that asserts only
`type: object`, with no error.

## Options

1. **Reject a `@variant` with no branch block at load**, at any depth. Unambiguous: an author who
   wrote the attribute meant a union, and a union with no branches is not one.
2. Make `valueAnnotationNames` key off the attribute rather than the branch list, so at least the
   composite guard catches it. Narrower — leaves the single-wrap and scalar cases silent.

Recommend 1.

## Acceptance

- [ ] `@variant(discriminator="…")` with no branch block is refused at load, with a diagnostic
      naming the property.
- [ ] Behaviour is the same at every depth (`object`, `[]object`, `[][]object`).
- [ ] A test per depth, proven to fail before the change.
- [ ] No regression for a well-formed `@variant` with branches, including on `map[string]object`,
      where the union currently rides on `additionalProperties` with `x-discriminator`.

---

## #3124 -- Value constraints that are inert for the element's type (@pattern on []int, @minLength on []object) build silently

**Classification:** `dsl`, `engine`

Found during the landing review of #3104 (issue #3049), by two independent lenses. Outside that
issue's stated scope, which names composite elements only.

## The gap

#3049 established the principle that a schema which contradicts its declaration in silence is the
wrong outcome, and #3104 enforced it for a *composite* element. The same silence remains one level
up, where the element type simply cannot carry the constraint.

All measured as building at `b8179774`, each emitting a keyword the validator ignores for that type:

```
[]object @minLength(3)  -> {"items":{"minLength":3,"type":"object"},"type":"array"}
[]object @pattern("^a") -> {"items":{"pattern":"^a","type":"object"},"type":"array"}
[]int    @pattern("^a") -> {"items":{"pattern":"^a","type":"integer"},"type":"array"}
[]string @minimum(3)    -> {"items":{"minimum":3,"type":"string"},"type":"array"}
[]bool   @maximum(3)    -> {"items":{"maximum":3,"type":"boolean"},"type":"array"}
```

It is not limited to wrapped fields — a plain scalar does it too (`object @minLength(3)` builds), so
this predates #2951 and #3049 both.

## Why it is worth closing

It is the same failure mode, with the same argument #3049 made against it: the author has written a
constraint, the engine has built a schema that ignores it, and nothing says so. An author who writes
`[]int @pattern("^[0-9]+$")` — a plausible attempt to constrain digits — gets no enforcement and no
error.

Note the interaction with #3104's new documentation: it now tells authors "the loader refuses" a
value constraint the element cannot carry. That is true for a *composite* element and not true for a
type-mismatched one, which is a distinction a reader is unlikely to draw unaided.

## Suggested shape

Reject a value-constraining annotation whose keyword is meaningless for the element's JSON Schema
type — `pattern`/`minLength`/`maxLength` on a non-string, `minimum`/`maximum` on a non-number — at
any depth, reusing the diagnostic shape #3104 introduced.

Worth checking first whether the tree or any product bundle carries such a pairing today; #3104's
review found zero *composite* fields, and this set was not counted.

## Acceptance

- [ ] A type-mismatched value constraint is refused at load, at any depth, with a diagnostic naming
      the annotation, the declaration and the element type.
- [ ] Well-matched constraints are unaffected (`[]string @pattern`, `[]int @minimum`).
- [ ] A test per mismatch class, proven to fail before the change.
- [ ] The corpus is swept first and the count recorded on this issue, so the blast radius is known
      rather than assumed.

---

## #3128 -- A trailing slash exempts /inbound/{source}/ from the verifier but does not match the mux route

**Classification:** `auth`, `bug`, `engine`, `security`

Found while landing #3110 (memql#3062). Deliberately not fixed there: it is pre-existing on `main`, in a file that PR does not touch, and the fix is one line that deserves its own change.

## The divergence

`component/identity/verifier/middleware.go`'s `normalizePath` strips a trailing slash before matching, so `/inbound/shopify/` reduces to a single segment under `/inbound/` and **is exempted** by `isSelfAuthenticated`. But `POST /inbound/{source}` does not match that path, so the mux routes it somewhere else.

The middleware and the mux disagree about what the request is.

## Why it is currently harmless, and why that is not good enough

Today no binary registers a `/` catch-all — grepped across `app/` and `component/server/` — so `/inbound/shopify/` 404s and nothing is exposed.

That is a property of the current route table, not of the matcher. And `isSelfAuthenticated` **already rejects exactly this reasoning** for the sibling case, in its own comment:

> Today that 404s because no binary registers a "/" catch-all — but that is a property of the current route table, not of this function, and nobody re-checks it when the next route lands. Closing it here makes the exemption depend on nothing external.

That argument was applied to the `%2F` case and closed it. The trailing-slash case has the identical external dependence and was left open. Either the argument is right for both or it is right for neither.

## Reproduction

Measured during #3110's review, driving the real middleware over a socket with hand-written request lines (so nothing normalises the target before the middleware sees it), against a mux carrying `POST /inbound/{source}` plus a `/` catch-all:

```
/inbound/shopify      -> exempt, routed to the inbound handler   (intended)
/inbound/shopify/     -> EXEMPT, routed to the catch-all         (the divergence)
/inbound/shopify/evil -> gated                                    (correct)
/inbound/             -> gated                                    (correct)
```

With no catch-all registered the third line 404s instead — which is the current tree, and the point.

## The fix

One line in `isSelfAuthenticated`: refuse the match when `r.URL.Path` ends in `/`. An exempted path should be one the mux will actually route to the handler whose self-authentication justifies the exemption.

## Definition of done

- [ ] `/inbound/shopify/` is no longer exempted by the middleware.
- [ ] A test asserting it, alongside the existing `%2F` and one-segment-bound cases in `component/identity/verifier/self_authenticated_test.go` — driven through the assembled `HTTPMiddleware`, not through a re-implementation of its predicate (see #3062's review for why that distinction matters: the tier could be deleted entirely with every predicate test still green).
- [ ] Confirm no legitimate caller sends the trailing-slash form. A webhook sender posting to `/inbound/shopify/` would start receiving 401 instead of 404 — a change in failure mode, not a regression, but worth knowing before it lands.

Refs #3062, #3110

---

## #3129 -- revoke/updateAgentAuthorization take a caller-supplied target with no owner check, and the concept declares no tier to enforce one

**Classification:** `auth`, `dsl`, `security`

Split from #3081, which stamped `createAgentAuthorization.userId` from the actor. That closes the **attribution** hole on the create path. It does not close the **targeting** hole on the other two mutations, and it deliberately did not declare the concept's row-authz tier — because doing so fails a gate today, measured below.

## What #3081 left open

Both remaining mutations on `v1:agents:agentAuthorization` take a caller-supplied target id with nothing relating it to the actor:

```memql
mutate agentAuthorization revokeAgentAuthorization {
  args { authId string! }
  update { id: args.authId; active: false }
}

mutate agentAuthorization updateAgentAuthorization {
  args { authId string!; payload object! }
  update { id: args.authId; args.payload }
}
```

`updateAgentAuthorization`'s own doc comment says:

> Per the agentAuthorization concept's revocation contract this is an OWNED write: only the granting user (the row's payload.userId) may update their own authorization grant.

**That is documented, not enforced.** Nothing relates `args.authId` to the actor. And the payload SPLAT means `validateMutationCallerArgs` — which walks the payload's *named* keys — sees nothing, the same structural blindness #2991 recorded.

So today: any authenticated caller who knows an `authId` can revoke another user's standing grant, or write arbitrary fields into it (including `computerUseScope` and `skillTierAllowlist`).

## The fix is already built — it just needs the declaration

#3079 (Phase 5) guards `update`/`delete` by resolving the target row and refusing when its owner field is not the actor. It is driven by the concept's `@rowAuthz` declaration. `agentAuthorization` declares none, so the guard has nothing to enforce here.

Declaring `@rowAuthz(owner="userId")` would make **both** mutations covered by machinery that already exists, with no per-mutation predicate.

## Why #3081 did not just declare it — measured, not assumed

Adding the declaration and running the shadow analyzer over the tree:

```
verdicts over the measured set:
  already-implied  33
  would-narrow      0
  undecidable       1

--- UNDECIDABLE ---
  agentAuthorizationsForUser
      concept   v1:agents:agentAuthorization (owned)
      because   filter contains spec "isActiveRecord", which this analyzer
                cannot expand, so implication is not decidable
```

`agentAuthorizationsForUser` filters `userId==args.userId && isActiveRecord` — comparing the owner field to a **caller-supplied arg**, not to `actor.userId`. The analyzer reports undecidable rather than would-narrow because the opaque trait could itself carry the term; the substance is the same either way.

**#3076's Phase 3 gate fails on `undecidable`**, by design: an undecidable access is one enforcement changes blindly. So the declaration cannot land alone.

## Definition of done

- [ ] `agentAuthorizationsForUser` gates on the actor rather than a caller-supplied `userId` — the same shape as #3063's three credential lists, and the reason the read side does not currently compensate for the write side.
- [ ] `@rowAuthz(owner="userId")` declared on `v1:agents:agentAuthorization`.
- [ ] The shadow report comes back with zero would-narrow and zero undecidable for the concept — re-derived, not assumed.
- [ ] A test proving a non-owner is refused on **both** `revokeAgentAuthorization` and `updateAgentAuthorization`, driven through the engine rather than by asserting the declaration exists.
- [ ] `updateAgentAuthorization`'s doc comment stops describing an unenforced rule, or becomes true.

## Ordering

Needs **#3079** merged (the guard) and benefits from **#3076** (the read side + its gate). Both were open at filing.

Refs #3081 #3079 #3076 #3077 #3063 #2991

---

## #3131 -- LoadFromRows keys the specialist registry by roleSlug last-wins over an unscoped agent set

**Classification:** `engine`, `security`

Found by the security lens while landing #3115 (memql#3066). Deliberately not fixed there: it is the same shadowing *shape* on a **different concept**, so it sits outside that issue's path and that PR's remit.

## The shape

`component/memql/agents.go`'s `LoadFromRows` builds a registry keyed by role slug:

```
map[roleSlug] -> *AgentDefinition
```

and it is **last-wins** — a later row silently displaces an earlier one under the same key. The registry is consumed by `askSpecialist` (`integrations/agents/integration.go`).

The rows come from `allAgents` (`dsl/agents/queries.memql`), which filters only `isNotDeleted`, sorts `row.createdAt` descending and paginates 50. It is **not caller-scoped**, and the query's own comment says so, justifying it on the grounds that both call sites are system-actor paths.

## Why this is not memql#3066

#3066 was about `v1:agents:agentRole` — the *catalog* of roles — where `createAgentRole` opens `id: args.agentRoleId ?? args.slug`, letting a caller mint a second row carrying a seeded slug. That is fixed: `findRoleBySlug` now prefers the predefined row and tie-breaks on row id, so resolution is order-independent.

This is `v1:agents:agent` — the *agents themselves* — keyed by the `roleSlug` they carry. Different concept, different write path, different query. The #3066 fix does not touch it and should not be read as covering it.

## What is and is not established

**Established:** the registry is last-wins over a set that is not caller-scoped, and it is ordered (`createdAt` desc) and bounded (50), so unlike the catalog it is not order-dependent in the "whatever the database returned first" sense. Ordering makes it *predictable*, which is better than the catalog was.

**Not established, and this is the work:** whether a **user-minted agent** can reach that registry at all. If every row reaching `LoadFromRows` is system-created, there is nothing here. If a user-created agent carrying a chosen `roleSlug` can enter the set, then "last-wins, ordered by createdAt desc" means the *newest* row under a slug wins — which a user could influence by creating an agent.

That trace is the point of this issue. It was explicitly **not** traced during #3115's review, and no claim is being made either way.

## Definition of done

- [ ] Determine whether a user-minted `v1:agents:agent` row can enter `LoadFromRows`' input set. Answer it from the write path and the query, not from the call sites' current intent — "both callers are system actors today" is a property of the callers, not of the registry.
- [ ] If it can: decide the same way #3066 did — prefer the authoritative row, tie-break intrinsically, or scope the read — and pin it with a test that PERMUTES the input rather than calling the function repeatedly on one fixture. (#3115's review found exactly that tautology: a pure function called 20× on the same slice literal, which cannot fail for any implementation.)
- [ ] If it cannot: record why in a comment on `LoadFromRows`, naming what would make it reachable, so the next person changing `allAgents`' scoping knows this registry depends on it. #3115 hit the same pattern — its new preference silently depends on `activeAgentRoles` filtering `isActiveRecord` in SQL.
- [ ] Either way, state whether `paginate 50` is a bound anyone should rely on: an agent set exceeding 50 rows silently truncates the registry.

Refs #3066, #3115, #2985

---

## #3135 -- undeclaredRowAuthzConstructs has no slot for the issue reference its own failure message demands

**Classification:** `engine`, `security`

Found during the landing review of #3125 (issue #3077), by both review lenses independently. Not
fixed there because it is a design change to the map's value type, not a review fix.

## The gap

`undeclaredRowAuthzConstructs` (`component/memql/rowauthz_undeclared_gate_test.go`) grandfathers 168
query constructs whose bound concept declares no `@rowAuthz` tier. The gate's failure message told
authors to add an entry **"WITH AN ISSUE NUMBER recording the decision to defer"**, and the header
repeated it.

Two problems, measured:

1. **Nothing enforces it.** The map value is the bound concept id, so there is no slot for an issue
   reference. No assertion reads one.
2. **Zero of the 168 seeded entries carry one.** Counted over the full map range with no truncation:
   `grep -cE '#[0-9]+'` → `0`.

So the convention that is supposed to make an appended entry auditable was violated by 100% of the
seed data on day one, and a future appender following the file's own example would not add one
either. #3125's review weakened the text to say a reviewer has to hold the convention rather than
claiming the gate does — this issue is about making it real.

## Why it matters

The gate is a bidirectional sync, not a monotone counter: it fails when the list and the derived set
disagree in **either** direction, which is genuinely strong. But growing the list costs exactly one
line, and the whole safeguard against that is a reviewer noticing. Today a reviewer has no signal to
demand justification, because no entry has ever carried one.

The sibling gate already solves this: `ownerGateExemptions`
(`component/memql/rowauthz_owner_gate_test.go:121-124`) makes the value the tracking reason and
renders it as `tracked: %s`. #3125 repurposed that slot for the concept id — gaining a real re-bind
check in exchange — and lost the reason field.

## Suggested shape

```go
undeclaredRowAuthzConstructs = map[string]struct {
    concept string
    reason  string // "#3077 seed" or a filed issue number
}{...}
```

with an assertion that `reason` is non-empty and matches `#\d+` for any entry added after the seed.
The 168 seed entries can carry a single shared marker (`#3077 seed`), which is honest — they were
grandfathered as a population, not individually triaged — and makes any *new* entry visibly
different.

## Acceptance

- [ ] The map value carries a reason alongside the concept.
- [ ] The gate asserts every entry has a non-empty reason.
- [ ] The 168 seed entries are marked as the #3077 grandfather population.
- [ ] A test proving an entry added without a reason fails the gate, verified to bite.
- [ ] The re-bind check #3125 added (entry listed against one concept, construct now bound to
      another) is preserved.

---

## #3138 -- updateAgentAuthorization splats a caller payload, so userId is still caller-writable after #3081

**Classification:** `auth`, `dsl`, `security`

Found during the landing review of #3130 (issue #3081), by both review lenses independently and
confirmed by the repo's own #2982 analyzer. #3130 closes the INSERT path; this is the UPDATE path,
which leaves the same field caller-writable.

## The path

`dsl/agents/mutations.memql:143-152`, unchanged by #3130:

```memql
mutate agentAuthorization updateAgentAuthorization {
  args {
    authId   string!
    payload  object!
  }
  update {
    id: args.authId
    args.payload
  }
}
```

A bare splat with no overlay. `component/memql/function_loader.go:583-590` documents the rule:
memql#401's overlay-wins protection is populated **only** from explicit block fields, so a bare
`args.payload` lands verbatim.

## Verified by the repo's own analyzer

`OwnerFieldProvenance` (the #2982 analyzer) run at #3130's head:

```
ServerStamped=false
Reason=updateAgentAuthorization splats a caller-supplied payload with no overlay re-stamping the
       owner field, so a caller can set it directly (memql#401's overlay-wins protection only
       covers explicit block fields)
StampedBy=[createAgentAuthorization]
WritableBy=[updateAgentAuthorization]
```

That analyzer only *runs* on concepts declaring an owner tier, and `v1:agents:agentAuthorization`
declares none — so nothing currently reports this in CI.

## The attack, after #3130

Both mutations are on the client surface via the generated SDK
(`sdk/go/client/generated_mutations.go:10928`):

1. `createAgentAuthorization(agentId, planKind:"*", spaceScope:"*", computerUseScope:"full")`
   → row correctly stamped `userId: <attacker>`.
2. `updateAgentAuthorization(authId:<that row>, payload:{userId:"<victim>"})`
   → read-merge keeps `computerUseScope:"full"` and `active:true`; `userId` becomes the victim.
3. `agentAuthorizationsForUser(userId:<victim>)` returns `full`.

That is the escalation #3081 describes, reached in two calls instead of one.

## Why #3130 does not cover it

#3130's body says the update paths are covered because "#3079's guard already covers them — as soon
as the concept declares a tier". `grep -n "rowAuthz" dsl/agents/concepts.memql` returns **nothing**,
and declaring the tier was itself deferred (to #3129). So the two deferrals lean on each other and
the guard is inert at that head. The claim is present-tense in the PR body and should be read as
future-tense.

## Options

1. **Re-stamp in the update block.** `updateNote` (`dsl/notes/mutations.memql:38-47`) is the safe
   in-repo idiom: it re-stamps `ownerUserId: actor.userId` alongside `args.payload`. One line plus
   `@actor`. Note this makes the mutation self-service only, which needs confirming — see below.
2. **Declare `@rowAuthz(owner="userId")` on the concept** so #3079's write guard and the #2982
   analyzer both engage. This is #3129's work.
3. **`@serverOnly`**, the precedent `updateUser` set under #2991 — but this mutation is reachable
   from the SPA's "Approve & always allow this tier" flow, so it would need a server-side path first.

**A decision is needed before option 1:** is `updateAgentAuthorization` meant to be self-service
only, or can an operator update another user's authorization? The concept's field doc says "Only the
granting user can revoke", which points to self-service, but "revoke" is not "update". Nothing in
the codebase settles it. Option 1 silently makes it self-service.

There are **zero** hand-written in-repo callers (full grep: the DSL definition and the generated SDK
only), so the regression risk of option 1 inside this repo is nil; the risk is entirely out-of-repo.

## Adjacent, same class

`updateAgentAuthScope` (`dsl/worker/mutations.memql:257`) sets `computerUseScope` by row id with no
owner predicate. Today it can only widen the caller's own row, but it is the second half of the
escalation once a userId rewrite exists.

## Acceptance

- [ ] A caller cannot set `userId` on `v1:agents:agentAuthorization` through any mutation.
- [ ] The decision above is recorded on this issue before the fix is chosen.
- [ ] A gate covering the update path, asserted against the parsed tree (#3130's
      `dsl/agentauthz_stamped_userid_test.go` is the idiom after its review), proven to bite.
- [ ] `OwnerFieldProvenance` reports `ServerStamped=true` for the concept, which requires the owner
      tier to be declared so the analyzer engages.

---

## #3143 -- Four documents claim the tool-args validator is auto-registered for EVERY enabled query and mutation; it is not, and #3127's narrowing is also false

**Classification:** `dsl`, `engine`

`docs/public/language/attribute-matrix.md`, `docs/public/language/reserved.md`,
`dsl/_reference/_concept.memql` and `component/database/memory-nodes/concept.go` all assert that the
tool-args validator "is auto-registered for **every enabled query and mutation**".

That is false, and PR #3127 -- which tried to narrow it -- is parked because its replacement is
*also* false. This issue carries the correct analysis and the fix that actually works.

## Why "every" is false

`registerFunctionTools` skips a name already taken (`component/memql/function_tools.go:51`):

```go
if tools.Has(name) || tools.IsDisabled(name) { continue }
```

and authored tools load first (`engine_bootstrap.go:236` before `:243`).

## Why the narrowed version is ALSO false

#3127 narrowed it to "an enabled query or mutation whose name no authored tool already occupies".
The name check is not the last gate — `registerFunctionTools` also skips when `ValidateTool` fails,
and `ValidateTool` requires a description (`component/memql/tool_types.go:398`).

`dsl/identity/queries.memql:989` `patIdentitiesForUser` is enabled, `@public`, its name is occupied
by no authored tool, and it carries only `//` comments (not `///`) with no `@description`. It gets no
generated tool.

## The correct counts, derived without glob narrowing

#3127's evidence used `dsl/*/tools.memql`, which misses `dsl/agents/tools/*.memql`. The loader is
filename-agnostic (`baseloader.ReadAll` walks every `.memql` via `dslfs.WalkMemqlFiles`), so all
authored tools occupy names:

```
all authored tool names in dsl/   43      (the narrowed glob saw 33)
query/mutation names             416
collisions                         3      markChunkSuperseded, searchUsers, writeKnowledgeChunk
```

| collision | tool | function | divergence |
|---|---|---|---|
| `searchUsers` | `dsl/memql/tools.memql:34` (`active`, `limit`) | `dsl/identity/queries.memql:200` (`active`) | tool has an extra field |
| `writeKnowledgeChunk` | `dsl/agents/tools/trainerTools.memql:54` | `dsl/knowledge/mutations.memql:86` | — |
| `markChunkSuperseded` | `dsl/agents/tools/trainerTools.memql:70` (`chunkId`, `supersededAt`, `reason`) | `dsl/knowledge/mutations.memql:123` (plus `supersededReason`) | function has an extra field |

## The fix: DELETE the clause, do not narrow it again

Two attempts to state the auto-registration rule exactly have both been wrong. The sentence's job is
to say the tool-args validator is an **uncovered** surface for `@secret`; how many functions get a
generated tool is not load-bearing for that.

Keep the part that is true, checkable, and the reason the surface matters: it is compiled from the
**same `ArgsSchema`** that carries the `Secret` flag.

## Acceptance

- [ ] The auto-registration clause is removed from all four documents, not re-qualified.
      Check: `grep -rn "auto-registered" component/database/memory-nodes/concept.go
      docs/public/language/attribute-matrix.md docs/public/language/reserved.md
      dsl/_reference/_concept.memql` returns nothing.
- [ ] The "compiled from the same ArgsSchema" statement is retained.
- [ ] `TestSecretEnforcementIsRealAndScoped` still passes (it matches on "tool-args validator" /
      "validatetoolargs" / "tool_execution.go", none of which this touches).
- [ ] `component/database/memory-nodes/concept.go:589` is re-wrapped to the block's ~80 columns.

Refs #3036 #3117 #3127

---

## #3145 -- v1:identity:authCode stores the OAuth code in cleartext when codeHash already serves the lookup

**Classification:** `security`

Found while annotating `@secret` for #3113. Filed as a design question, not a defect — the current behaviour is deliberate and documented; whether it should be is the question.

## What is stored

`v1:identity:authCode` (`dsl/identity/concepts.memql`) carries **both**:

- `codeHash` — *"SHA-256 hex digest of the plaintext code. Primary lookup key in the token-exchange path -- avoids equality lookups on the plaintext."*
- `code` — *"Plaintext one-time auth code returned to the OAuth client via redirectURI. Held server-side so the token-exchange handler can verify the presented code value matches; never logged, never surfaced in audit detail. **Hash is the lookup key, plaintext is the equality check on redemption.**"*

This is the **only field in the DSL tree that stores a credential in cleartext**. A sweep of every concept-field description for stored-plaintext language returns this one and nothing else; every other credential in the tree (`api_key`, `worker_token`, `node_token`, `voice_agent_token`, `badge`) is a digest, with schemas that say so explicitly — *"Plaintext never stored"*.

## Why it is worth a look

The stated reason for keeping the plaintext is the **equality check on redemption**. But the hash is already computed and already the lookup key, so the redemption check can be `sha256(presented) == codeHash` — which is what the rest of the credential families in this repo do, and what the neighbouring `magiclink` row does.

If that is right, `code` is retained for no property that `codeHash` does not already provide, and the tree's single cleartext credential could simply stop existing.

Countervailing consideration worth stating: the code is single-use with a ~60s TTL, redirect-URI bound and client-ID bound, so the exposure window is small and the blast radius of one leaked row is one aborted OAuth exchange. That is a real mitigation and may well be why it was accepted. It is not the same as the value not being there.

## Not blocked on this

#3113 annotates `code` with `@secret`, which is correct either way — while the field exists it should be marked. This issue is about whether it needs to exist.

## Acceptance

- [ ] A decision on whether the plaintext `code` is load-bearing, given `codeHash` is already the lookup key.
- [ ] If it is not: remove the field, move redemption to a hash comparison, and confirm nothing reads `code` (`component/identity`, the `/oauth/token` handler).
- [ ] If it is: record in the field description **what** the plaintext provides that the digest does not, so the next sweep does not re-raise this.

---

## #3148 -- EnsureSchema still amplifies its own failure under MEMQL_REQUIRE_DB, which is what the db-tests lane sets

**Classification:** `reliability`

Found reviewing #3137 (issue #3096). That PR fixes the amplifier on the **default** path; this is the half it does not reach, and the db-tests CI lane is on the wrong side of it.

## The gap

#3096's fix defers `os.Setenv("MEMQL_DATABASE_DSN", dsn)` until after a successful ping, so an unreachable `defaultDSN` no longer overwrites an empty env and kill every downstream package's own fallback.

But `RequireDB()` is checked at `component/database/dbtest/dbtest.go:90`, and the deferred `Setenv` is at `:111` — so on the `MEMQL_REQUIRE_DB=1` path `EnsureSchema` returns an error at the ping and never reaches the fix at all:

```go
if perr := lockDB.PingContext(pingCtx); perr != nil {
    ...
    if RequireDB() {
        return false, fmt.Errorf("dbtest: %s=1 but Postgres is UNREACHABLE at %s: %w", ...)   // :90-91
    }
    return false, nil                                                                          // :93
}
...
if usedDefault {
    _ = os.Setenv("MEMQL_DATABASE_DSN", dsn)                                                    // :111-113
}
```

Every `main_dbschema_test.go` `os.Exit(1)`s on that error before `m.Run()`, so the package dies and its own good fallback is never consulted.

## Measured

Two independent reviewers reproduced this against a throwaway TimescaleDB, with `defaultDSN` pointed at a bad credential and the package fallback left correct:

| tree | `MEMQL_REQUIRE_DB` | result |
|---|---|---|
| as patched (#3137) | unset | tests RUN (77 DB-attributable skips -> 0 across the 6 db-gated package trees) |
| fix reverted | unset | 77 skips, all `SQLSTATE=28P01` |
| **as patched** | **1** | **0 tests ran, package FAIL** |
| fix reverted | 1 | 0 tests ran, package FAIL — **identical** |

So under `MEMQL_REQUIRE_DB` the patched tree behaves exactly like the unpatched one.

## Why it matters

`.github/workflows/ci.yml` sets `MEMQL_REQUIRE_DB: '1'` for the db-tests lane. That is the lane where a stale `defaultDSN` would do the most damage, and it is the one the fix does not cover. #3096's own narrative cites this case ("Under `MEMQL_REQUIRE_DB` the unfixed tree did not skip -- it **failed the package outright**"), but its acceptance list did not require covering it, which is why #3137 landed without it.

## The decision this needs

Not obviously a bug — it is a genuine choice, which is why this is filed rather than fixed in #3137:

- **Leave it.** Under `MEMQL_REQUIRE_DB` the operator demanded a reachable database; hard-failing is arguably the correct, loud answer, and silently falling through to a different DSN than the one reported could be worse.
- **Gate the error on `!usedDefault`.** A run that supplied no DSN and merely hit the helper's stale fallback is a different situation from one where the operator named a DSN that does not work. The package's own fallback would then get its chance, and the per-test gate would still fail loudly under `REQUIRE_DB` if that DSN is also dead.

## Definition of done

- [ ] An explicit decision, recorded at `dbtest.go:90` rather than only in a PR thread.
- [ ] If falling through: a test proving that under `MEMQL_REQUIRE_DB=1`, an unreachable `defaultDSN` with a reachable package fallback still RUNS the package's tests.
- [ ] If leaving it: the limitation stated where a reader will hit it, so the next person does not assume #3096 covered this path.

Refs #3096, #3137

---

## #3149 -- 14 test files hardcode the database DSN while dbtest.DSN() exists to resolve it, so credential drift stays possible

**Classification:** `reliability`

Found reviewing #3137 (issue #3096). #3096 hardened `EnsureSchema` so a stale `defaultDSN` no longer takes the whole suite down with it. This is the root cause that made that hardening necessary: the DSN is written out by hand in 14 places, and the helper that would centralise it already exists and is essentially unused.

## Measured

```
$ grep -rln 'postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable' --include='*_test.go' . | wc -l
14
$ grep -rln "dbtest\.DSN()" --include='*.go' .
scripts/cidb/dbgate_unit_test.go
```

The 14 files:

```
component/automations/cluster_guard_db_test.go
component/automations/steps/account_deletion_sweep_db_test.go
component/automations/steps/dryrun_source_trust_2890_db_test.go
component/automations/steps/scheduler_ready_race_test.go
component/database/dbtest/dbtest_hygiene_test.go
component/grpc/voice_agent_real_engine_test.go
component/grpc/wire_bare_ids_test.go
component/memql/executor_mutation_readmerge_db_test.go
component/memql/skill_catalog_reconcile_db_test.go
examples/referencepack/live_e2e_test.go
integrations/cognition/dispatch_gate_test.go
integrations/planner/admission_test.go
scripts/cidb/dbgate_unit_test.go
test/conformance/harness_test.go
```

And `dbtest.DSN()` (`component/database/dbtest/dbtest.go:177-183`) already does exactly the resolution all 14 hand-roll:

```go
func DSN() string {
	if dsn := strings.TrimSpace(os.Getenv("MEMQL_DATABASE_DSN")); dsn != "" {
		return dsn
	}
	return defaultDSN
}
```

## Why this is worth doing

The #3030 / #3096 defect was a credential in `defaultDSN` (`memql_local_dev`) that matched nothing in the project and diverged from what the test files used. Divergence is only *possible* because the string is written 14 times. Today all 14 happen to agree — which is precisely why #3096's fix is currently prophylactic rather than load-bearing: with every fallback identical to `defaultDSN`, the amplifier it guards has nothing to amplify.

Routing them through `dbtest.DSN()` makes that class of drift structurally impossible instead of merely fixed-for-now, and it makes #3096's guard meaningfully load-bearing.

Note the two files that legitimately keep their own constant should be checked rather than assumed: `test/conformance/harness_test.go:44` declares its own package-local `defaultDSN`, and `component/database/dbtest/dbtest_hygiene_test.go` is inside the package under test.

## Definition of done

- [ ] Every db-gated test file resolves its DSN through `dbtest.DSN()` rather than a literal, or carries a comment saying why it cannot.
- [ ] A gate that fails when a new `postgres://` literal appears in a `_test.go` file outside `component/database/dbtest/` — otherwise the 15th copy lands the same way the first 14 did. `scripts/cidb` is the natural home; it already does this shape of structural assertion for the db-tests lane.
- [ ] `dbtest.DSN()` has at least one caller that is not the gate testing it.

Refs #3096, #3137, #3030

---


*39 entries pending transfer. Delete this file once they have all been moved into epics or dropped.*
