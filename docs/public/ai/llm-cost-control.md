---
title: LLM cost control — defense in depth
audience: public
status: stable
area: ai
sinceVersion: 0.9.0
owner: znas
---

# LLM cost control — defense in depth

This is the canonical map of how MemQL bounds LLM spend so that a runaway
loop **cannot** burn unbounded money regardless of which path triggers it
(epic [memql#1141](https://github.com/znasllc-io/memql/issues/1141)).

## Why "defense in depth"

Earlier protections were all *rate limiters that self-heal*: the
per-fingerprint loop breaker resets after its cooldown, and the rate ceiling
drains its window and re-admits. That is correct for a transient spike, but
it means a genuinely **stuck** loop — one that varies its request body and
paces just under the rate ceiling — trickles spend **forever** (you'd see
`429 ... rate ceiling ... blocked` repeating indefinitely while money keeps
leaving). Every per-path fix (per-turn breaker, produceArtifact re-delegation
cap, …) patched one loop; the runaway reappeared via another.

The fix is layered. The lower layers are *graceful* (throttle, converge,
park); the **bottom layer is a hard kill-switch** that latches and never
drains. Because the kill-switch lives at the single chokepoint every
chat/messages call passes through and is **path-agnostic** (it counts calls,
not callers), a brand-new runaway path nobody anticipated is bounded
automatically.

## The layers

| Layer | Mechanism | Scope | Self-heals? | Where |
|---|---|---|---|---|
| **0. Kill-switch** | cumulative call + est-$ **latch** → terminal 402, never drains | process (per-scope: #1144) | **No (latches)** | `component/memql/ai_guard.go` |
| 0b. Same layers, local models | the four checks above, applied to fleet calls at the provider seam | process + per-scope, **shared state** | as above | `component/memql/ai_guard_fleet.go` |
| 1. Rate ceiling | calls/window → synthetic 429 | process, per-lane | yes (drains) | `ai_guard.go` |
| 1. Loop breaker | identical-request repeats → 429 | per-fingerprint | yes (cooldown) | `ai_guard.go` |
| 2. Automation budget | executions/window → skip; bounded fail-open | process, per-automation | yes (window) | `component/automations/budget.go`, `cluster_guard.go` |
| 3. Loop terminal conditions | per-turn iteration / convergence / wallclock caps | per invocation | n/a | agent loops + the work run's ceilings |
| 4. Per-scope budget | cumulative latch per plan-lineage / work run | per plan/run | **No (latches)** | `ai_guard.go` via context (#1144) |

Layer 0 is the backstop **behind** every other layer: even when a higher
layer is generous or a loop's terminal condition is loose, the cumulative
kill-switch caps total spend.

### The selection seam is a layer -- and it is NOT the enforcement one

Since epic [memql#5127](https://github.com/znasllc-io/memql/issues/5127) every
call to a model goes through **one router seam**: a call declares a *level*
(`fast` / `strong` / `reasoning` / `embeddings`) instead of a model, a *rule*
maps the call's metadata to a *policy*, and the policy is an ordered chain that
reaches the fleet first, a signed-in subscription app second and a metered
vendor last. So the shipped default now **chooses** the cheapest sufficient door
before any of the layers above has to **refuse** an expensive one. That is
genuine cost control and it belongs in this document -- but it is a different
kind from everything in the table.

**A kill switch a policy can author around is not a kill switch.** The rules
system is authored: a DSL bundle may add rules, and an owner may add one at
runtime through `routingRuleActivate`. So the split is explicit and enforced by
where the code lives:

| Decided by rules (authored) | Decided in Go (not authorable) |
|---|---|
| which door a call tries first, and in what order | the process-wide rate ceiling and identical-request loop breaker |
| whether an exhausted chain degrades a level or parks | the cumulative call and dollar kill switch, and its per-scope latches |
| which concrete model a selector picks | the run's token and cost ceilings (`component/work/budget.go`) |
| whether a level is raised for a particular prompt or role | door classification, provider availability, and the refusal codes |

The two meet at exactly one place, unchanged by this epic: **the federation hop
asks the cost ceiling before it is taken, and only when a local door preceded it
in the chain.** A chain that starts at a vendor is a decision somebody made
rather than a fallback, and refusing it there would break every deliberately-paid
policy. See [Layer 4b](#layer-4b--the-ceiling-gates-the-federation-hop-memql5096).

Capability discovery includes complete small argument contracts, avoiding
another model call just to ask how to invoke a returned query. For an owned
lookup, the executing replica reads its durable classification and supplies a
bounded authorized shortlist before inference, avoiding a model call for initial
discovery. This supplies contracts only: current records still require execution,
and unavailable or oversized discovery leaves the ordinary tool available. Large contracts
remain available by exact name so discovery cannot fill the model’s context.
A lookup classification does not automatically lower the execution reasoning
level: choosing tools and interpreting evidence still require a capable model.
Routing policies and explicit step overrides continue to decide that route.

A reply's existing evidence check can identify an unfinished lookup instead of
only replacing unsupported claims with a disclaimer. The installed completion
recipe permits one continuation for a specific unresolved source or capability
match. The runtime records that attempt with the exact source checkpoint before
continuing, so another replica cannot restart its allowance. The continuation
and its final review spend the original run's remaining calls and deadline;
they grant no new permission or budget. A supported answer, a bounded failed
check, and an ordinary quick follow-up do not take this path.

A non-quick classifier response with missing or oversized acknowledgment prose
gets at most one `workAcknowledgement` call at `fast`, bounded by the installed recipe to 180 seconds and
the run’s remaining call ceiling. Valid acknowledgments and quick replies add
no call. Failure leaves the work visible through its real task status rather
than manufacturing a reply or retrying indefinitely.

**One call was added, and it is bounded by construction.** The work spine's
failure path now runs the deterministic symptom table first and reaches the
`classifySymptom` prompt -- at level `fast`, the cheapest tier -- **only when the
table has no opinion**. A rules-classified failure costs zero provider calls; a
novel one costs exactly one. Both figures are asserted by a proving scenario
whose negative control is the classifier being reached at all, because a counter
that never rises on any path reads as zero forever.

**A plan miss adds one more, at the `reasoning` level, and three things bound
it** (memql#5664). The planner re-plans the run from the failed step with
`replanGap`: once per failure, because the remedy is served under one claim per
wait across every planner replica and every outcome after the call moves the
run off its wait; at most the run's retry budget times, because a re-plan
spends a retry exactly as a retry and a repair do (`ceilings.maxRetries`, three
when unset), and past it the run asks a person; and inside the run's own
ceilings, because the call is made as the run and passes the model seam like
any other.

Every resolution writes one `v1:router:call` row saying which rule matched, which
policy it named, which door served, what it cost, and every entry the walk passed
over with the reason. Read them with `routerDecisionsRecent`. A cost control
nobody can audit is an assertion; see
[AI routing](../operate/ai-routing.md).

## Where the chokepoint is (and why it moved)

### Shared local inference

The stream-owning replica admits all local and forwarded model requests through
one queue. It reserves both the machine-wide slot and the advertised per-model
slot before waking a waiter. Cancelling a waiter releases any reservation, and
a disconnected machine refuses queued work. Another capability's completion
cannot admit a model call. Waiting and inference retain their operation deadlines and cancellation. A
goal has renewable progress windows rather than a fixed lifetime deadline.

Conversational file requests perform one structured evidence assessment before
materialization. The check reads bounded owner-authorized source receipts from
the conversation, distinguishes recorded evidence from current profile context,
and passes supported findings and references into the file. Missing essential
evidence returns an explanation to the conversation without an upload. The
assessment consumes the run's ordinary model-call allowance and does not retry
itself. The default Spine promotes a quick conversational file request to the
lookup estimate before execution, reserving room for classification, a possible
acknowledgment, evidence assessment and composition. Explicit user ceilings and
already-spent calls remain binding. Self-contained writing does not require
invented external research.

### Ask classification and repair limits

Ask classifies the current message with its conversation context. Follow-ups
do not reuse templates matched only to the latest words. Classification has
a recipe-selected deadline (600 seconds in the shipped Spine, including time
waiting for a shared local model); missing, invalid, or failed classification stops with
a retryable error instead of escalating into automation authoring. Only an
automation intent can send a conversation through that authoring path.

New Ask runs start with a ten-minute estimate, twelve model calls, and one
automatic retry. A conversational reply tightens the call allowance to three while retaining
the ten-minute admission allowance, preserving stricter existing ceilings.
A quick workload describes effort, not whether local hardware is cold or busy. Time
is measured from the stored run start, including classification; a retry or
move to another replica does not restart the clock. Per-operation deadlines cover in-flight prompt calls; renewable workload
windows are checked between calls. Direct agent attempts also
consume the run's model-call allowance and write attempt receipts.

A classified quick conversational reply uses the `workQuickReply` prompt at
`fast` through the normal `ai` builtin and the durable run journal. Its draft
is held until the evidence check below completes. It reads the authenticated person's profile on the execution
replica and the conversation from the saved run variables. It does not
retrieve knowledge or enter the agent tool loop. A request for a
file, research, navigation or action retains its normal execution recipe;
`quick` alone never authorizes skipping those requirements.

Before delivering a source-based reply, `workReviewReply` rereads bounded
owner-authorized receipts on the executing replica and makes one structured
`workReplyReview` call. The check compares the proposed answer with the actual
receipts, not previous assistant assertions. It has no tools and cannot start
another search. The same check serves an agent's completed answer and a quick
follow-up. Greetings and self-contained replies without retrieved evidence add
no call. Missing source reads or a failed check never publish the unchecked draft.
The check uses the original run's remaining allowance, provider restrictions
and deadline; it neither retries itself nor raises any ceiling. A normal quick
follow-up uses at most its existing three calls: classification, reply and review.
This is a model pre-filter, not a guarantee of factual correctness; live checks
must still compare its output with the retained evidence.

Automation repairs receive the complete bundle and its diagnostics. They
may repair failing members or add missing dependencies, but cannot rewrite
passing members. Repeated source, a cycle back to an earlier bundle, or two
successive attempts with unchanged diagnostics stops repair before its
attempt cap. Activity reports the stage, repair attempt, and final outcome.

Every layer below is described in terms of a single point every LLM call
passes. Until local models landed that point was the HTTP transport, and the
statement "every chat/messages completion leaves the process through one
`guardedTransport`" was complete — the guard was path-agnostic precisely
because there was exactly one way out of the process.

A **fleet call has no `*http.Client` at all**. It leaves over the
WorkerService stream to a model on one of the user's own machines
(epic [memql#4676](https://github.com/znasllc-io/memql/issues/4676)), so it
would have passed no gate whatever. A runaway loop on a free model is still a
runaway loop: it burns a laptop's battery, occupies the machine its owner is
trying to work on, and converges on nothing.

So there are now **two entry points into the same guard**, not two guards:

| Path | Entry point | State |
|---|---|---|
| Vendor HTTP (OpenAI, Anthropic) | `guardedTransport.RoundTrip` | the shared `llmGuard` |
| Fleet / local models | `GuardLocalModelCall`, called from the fleet provider's one call seam (`fleet_provider.go`) | **the same** shared `llmGuard` |

Both run the same four checks in the same order — latch, loop breaker, rate
ceiling, cumulative accounting — and both read and write the same counters.
That shared state is what stops the two paths from disagreeing about how many
calls a runaway has made. `TestTheFleetProviderItselfPassesTheGuard` holds
that a call placed through the registered fleet provider passes the guard.

**One thing differs, and only this: the dollar figure is zero.** Nobody was
billed for a local call, so charging it would park work over money that was
never spent — and the more someone leans on hardware they already own, the
sooner their plans would stop, which is exactly backwards. The **call**
tallies are unchanged, process-wide and per-scope. This is the same split
memql#4362 made for subscription spend, arriving for a second reason; see
`recordAndMaybeLatchCost` and `TokenState.SpentLocal`.

The fleet fingerprint (`FleetCallFingerprint`) keys on the model and the exact
conversation, deliberately **not** on the plan id, task id or purpose: those
vary across a genuine loop, and folding one in would make every repetition
look novel — defeating the cheap catch the breaker exists to be.

## Layer 0 — the kill-switch (`ai_guard.go`)

`guardedHTTPClient` wraps the `*http.Client` of all four chat provider
builds (OpenAI + Anthropic, stream + non-stream), so every chat/messages
completion leaves the process through one `guardedTransport`. There it is
checked, in order: (0) kill-switch latch → (1) loop breaker → (1) rate
ceiling → (3) cumulative accounting. Crossing a cumulative cap **latches the
breaker open permanently**: every further call returns a terminal **402**
(non-retryable in both vendor SDKs, so it surfaces as a clear terminal error
to the calling loop) and makes **no vendor request**. It does not drain
until the process restarts (or the guard is reset).

The $ figure is a deliberately **conservative upper bound** estimated from
the request only (size + `max_tokens`, priced at an Opus-tier rate) — the
response usage is never visible at the RoundTripper. Over-estimating is the
safe direction: it latches sooner.

Process caps default to 0 (unlimited) — a since-boot cumulative latch has no
single value safe for both a tiny local run and a long-lived production node,
so the process backstop is **opt-in**. The on-by-default conservative
protection is the per-scope latch (Layer 4). Local repro:
`MEMQL_LLM_MAX_TOTAL_CALLS=20`.

### What the guard deliberately does NOT count: the federation token exchange

Both AI vendors are reached by workload identity federation (memql#4333 for
Anthropic, epic memql#5088 for OpenAI), and that is the ONLY way either is
reached -- there is no manually entered vendor API key anywhere in the product.
So on any federated cluster one more request leaves the process through this
same `guardedTransport`: the token exchange that trades the pod's projected
Kubernetes token for a bearer of at most an hour. It is **outside the guard's
fingerprint by design** — `isGuardedLLMPath` matches only `/messages` and
`/chat/completions`, and a test holds that neither vendor's exchange path is
one of them.

That is not an oversight to be tidied up later, and it must not be "fixed":
the exchange spends no tokens and has no completion to price, so costing it
would inflate a budget meant to bound spend. Worse, it is byte-identical every
time and happens on a schedule, which is precisely the shape the loop breaker
exists to stop — guarding it would let a routine hourly refresh trip the
breaker and take the cluster's model access down, in the name of cost control
that saved nothing.

It is not unobserved, though. It has its own counter,
`memql_ai_federation_exchanges_total{vendor,outcome=ok|denied|error}`, and a
denial logs the vendor's own error body. Alert on `denied`: the last good
bearer keeps working until it expires, so a broken credential does not surface
as an outage for up to an hour after the change that caused it. Runbooks:
[../operate/auth/anthropic-federation.md](../operate/auth/anthropic-federation.md)
and [../operate/auth/openai-federation.md](../operate/auth/openai-federation.md).

## Layer 2 — automation budget

A misfiring automation (one that re-fires on its own failure, or a
plan-level loop that re-creates a plan each cycle) is a runaway *multiplier*:
each execution can drive fresh LLM calls. Storm detection used to be
log-only; now a process-global, cross-executor budget (total, per-automation and per-row
executions/window) **skips** the execution once a storm blows past it, and
the cluster guard's fail-open path is **bounded** (it admits a capped number
of unguarded executions per window during a DB outage, then fails closed).

## Layer 3 — loop terminal-condition audit

Every LLM-driving loop already carries a per-turn terminal condition; the
residual gap is *cross-turn* cumulative spend, which Layers 0 and 4 close.

| Loop | File | Per-turn cap | Cross-turn / lineage cap | Convergence guards |
|---|---|---|---|---|
| Agent tool loop | `integrations/agent/streaming.go`, `nonstreaming.go` | 120 iters (`MEMQL_TOOL_LOOP_MAX_ITERATIONS`), 180s wallclock (`MEMQL_TURN_WALLCLOCK_TIMEOUT_SECONDS`) | none → **Layer 0 / 4** | 3 repeat-failures (`MEMQL_TOOL_LOOP_MAX_REPEAT_FAILURES`), 3 all-errored rounds, 2 produceArtifact re-delegations |
| Engine AI tool loop | `component/memql/ai_tool_loop.go` | 120 iters (`MEMQL_TOOL_LOOP_MAX_ITERATIONS`), 8 tool-calls/iter | none → **Layer 0 / 4** | all-errored guard, identical-call breaker |
| Work run | `integrations/work/runceilings.go`, `component/work/budget.go` | n/a -- a run is not a turn loop | the goal's `ceilings` (tokens, cost, model calls, wall clock), checked before every call; a breach parks the run. `maxRetries` bounds the failure path's retries, repairs and re-plans (three when unset; past it the run asks). `maxEvents` is declared but not enforced: the run logs a warning that it bounds nothing | n/a |
| Authoring compile | `integrations/planner/work_compile.go`, `agent_loop_authoring_emit.go` | n/a -- a compile is a bounded pass, not a turn loop | the run's `maxModelCalls`, counted across its design, emit and repair calls (`callCapGate`) | 4 repair attempts per authoring bundle (`MEMQL_AUTHORING_MAX_REPAIRS`) |
| Suggest | `component/grpc/` AiSuggest | single call | n/a | n/a |

Verdict: no loop lacks a per-turn terminal condition. The agent / engine tool
loops have no native cross-turn cap and rely on Layer 0 (process) and Layer 4
(per plan-lineage / work run) for cumulative bounding.

### Run usage across replicas

Before admission, the run guard reads fresh, paged usage summaries and counts
an identity-based union of durable model-call receipts and local charges.
Returning to a replica that previously ran the same work does not lose calls
completed on a peer. Each completed request shares one receipt ID between its
charge and journal write; rereading that receipt does not charge it again.
Metered, subscription and local tokens keep their separate buckets.

A failed journal write and an unjournaled cache hit remain counted on the
current process. They cannot be reconstructed on another process or after
that run's in-memory entry is evicted. The guard retains at most 2,048 run
entries and 4,096 receipt identities per entry; reaching the receipt bound or
failing to read every summary page refuses further calls rather than dropping
identities. These are completed-call counters, not distributed reservations:
simultaneous calls can be admitted before either has completed.

The model-call `served` enum includes `subscription` so covered calls can be
journaled and recovered on another replica. After those rows are written,
rolling back to an engine whose schema lacks that value is unsafe: its schema
can reject writes involving those rows. Keep a compatible engine during
rollback or prepare an explicit data migration first.

### Run time limits and progress continuation

Workload estimates are checkpoints for assessing progress. At the next model
admission, successful new task-tool receipts and verified capability reads can
renew a spent time or call estimate automatically, without a lifetime extension cap.
Capability reads count newly discovered operation contracts and advertised
modalities. Repeating a search, changing its wording or timestamp, and busy/idle
changes do not count as new progress. A slow completed call may consume more
than one window to catch up. Identical tool arguments, failed calls, unverified
discovery calls, archived-history reads and user-facing narration
cannot earn another window. This is a bounded activity signal, not proof that
the result is correct. The counter and receipt fingerprints persist with the
run under a shared decision lock, so reconnecting or changing replicas cannot
restart them.

Time and call counts describe work windows, not a goal lifetime. Monetary token
and cost ceilings remain hard. Each renewal is computed in constant time, even
after a long operation. No context deadline cancels the whole goal at a fixed
age; individual provider/tool timeouts retain their separate recovery behavior.

Owned agent turns reconsider before exhausting their allowance. The installed
`agentProgressRecovery` policy requests a review at 75% of a model-call limit
of at least six, or when three calls remain. It also requests a review after
three capability-discovery rounds without a new task operation, or three
occurrences of the same tool evidence in a bounded recent window. Rereading
archived context does not erase a discovery loop; new research results count
as progress, including alongside discovery in a parallel batch.

The review uses the next ordinary reasoning call. It asks the model to use the
evidence, consider an authorized alternative, finish a verified result, or ask
a specific question. Two further stalled rounds cause a native feedback pause;
one is enough if only one model call remains. Nearing the cap alone does not
pause productive work. Context checkpoints retain a DSL-declared reserve of
three calls by archiving exact sources without another model summary when
necessary. The model-call guard still checks each work window, including context summaries
and resumed work, and requires evidence or human direction to renew it.

The recovery record is bounded and survives compaction and another replica.
New accepted human guidance permits reconsideration; old answers replayed from
history and a raised limit alone do not erase stalled progress. Guidance grants no additional access. Progress feedback may renew a window without changing monetary limits, as
described below. These initial thresholds are policy
choices to evaluate against observed tasks, not universal model guarantees.

The same durable recovery record also observes repeated read results across
changed query wording. This additional signal is observation-only: it does not
change the installed review thresholds, pause a run, require a write, or make
another model call. New result bytes, source versions and archive pages remain
new evidence. Checkpoints separately retain the next missing deliverable, its
unresolved question and exact public evidence references as derived claims.
These fields do not prove completion or authorize an operation.

Optional reviewed Training selections are recorded in the existing owned work
observation journal, with exact selected text/version digests, bounded rejected
candidate reasons and the declared query/allocation limits. The receipt covers
only the admitted ranked candidate window, not every item in the corpus.
A revoked source contributes no text or identity to a rejection receipt. Each
actual provider-attempt observation carries the receipt IDs attached to its
rendered context, including fallback attempts. Selection, delivery, adherence
and successful completion remain separate evidence. Saved receipts never
replace current source review and ownership checks on a new turn.

When new progress cannot justify another automatic window, Ask uses its
ordinary feedback question: **Keep working**, **Stop here**, or a correction
in the person's own words. An accepted answer can continue the saved work
on the same goal. It does not reset usage, completed steps or permissions.
Its decision ID makes retrying a recorded answer idempotent across replicas.
The bounded duplicate-evidence set starts a new epoch at that human answer;
only receipts newer than the epoch can support another automatic renewal.
The old exact receipts remain in the journal. Reaching the 300-fingerprint
working-set bound asks for direction rather than ending the goal. Monetary
limits and unreadable or overflowing counters still refuse further admission.
Time spent waiting for human input is excluded from active work time.

The numeric budget-approval controls and deferred **Save guidance** / **Raise
limit** sequence have been removed. Legacy budget requests remain readable and
can be closed, but cannot raise a limit or authorize checkpoint replay.
Historical workload overrides are still read within the current declared caps;
this workflow writes no new overrides and never rewrites goal limits.

If an answer is saved but releasing the run fails, Ask and Nexus offer to
continue the exact saved answer. Retrying does not record another answer or
extend the allowance twice. The owner, latest checkpoint and execution request
must still match. A changed, cancelled or no-longer-waiting execution cannot be
restarted from an old receipt.

An agent tool loop saves its completed exchanges before publishing the feedback
question. Synchronous parent and child invocations wait on that same question;
the child links and checkpoint identify the continuation on another replica.
An answer arriving before that unwind finishes is retained until the waiting
receipts are ready. Continuing grants no tool permission and does not replay
completed effects. A failed legacy step without those links, a missing or
changed checkpoint, or an uncertain in-flight tool outcome cannot use this path.

The same rule applies after a person chooses **Run again** with unchanged inputs.
That act starts a new version, but a later native question pauses that version
rather than starting it again. Its saved progress is bound to the exact rerun
request and survives the hop to another replica. A missing or changed request,
checkpoint, override, or child link is refused; an older question cannot authorize
the newer version. Prior usage and the completed prefix remain intact.

## Layer 4 — subscription spend, counted but not charged (memql#4362)

A Task delegated to a local app (epic memql#4358) spends **real tokens that
MemQL was not billed for**. That breaks a single spend counter in a way worth
being explicit about, because the two existing controls want opposite answers:

| Control | Subscription tokens | Why |
|---|---|---|
| The **dollar ceiling** (the goal's `ceilings.tokenBudget`) | **excluded** | MemQL was not charged. Counting them parks a run over money nobody paid — and the more a user leans on the subscription they already pay for, the sooner their runs stop, which is exactly backwards |
| The **loop caps** (the run's model-call count, the identical-request breaker) | **included** | A runaway loop that happened to route through a subscription is still a runaway loop. A cap blind to those calls is a hole the cheapest path walks straight through |

So the spend is split rather than summed:

- `v1:work:run.spent.tokensSubscription` accumulates covered tokens,
  separately from `tokens`. `component/work/budget.go`'s `CheckCeilings`
  compares only the metered `tokens` against the ceiling.
- `v1:router:call.billing` (`metered | subscription | unknown`) and
  `executionSurface` carry it on the ledger, so a cost reader can separate
  "what we spent" from "what ran somewhere we do not pay for" without a join.

**`unknown` is a real answer, not a rounding.** An app that reports no usage
gets `unknown` and zeroes — never "free", never "metered by omission". The
number this whole layer exists to produce (what the subscription covered) is
only trustworthy if silence stays visible as silence.

**An executor that reports NO billing is treated as metered.** Unattributed
spend counts against the ceiling rather than vanishing into the covered bucket,
where it would be invisible to the one control that stops runaway cost. That is
the fail-safe direction, and it is why `ExecutorResult.EffectiveBilling()`
defaults the empty string to `metered` rather than to `unknown`.

## Layer 4b — the ceiling gates the FEDERATION HOP (memql#5096)

The shipped policies changed shape in epic memql#5096: every one begins
`fleet:*`, then `app:*`, then the vendor entries. That makes falling back to a
**paid** provider a distinct, nameable moment in the chain walk — and it is the
moment the cumulative ceiling is consulted.

| | |
|---|---|
| **When** | the router is about to select a vendor entry AND a local door preceded it in the chain |
| **What it asks** | `memql.CostCeilingReached` — the same caps `recordAndMaybeLatchCost` enforces, asked as a **read** |
| **What it answers with** | the typed refusal `ceiling_reached`, carrying the guard's own sentence (which names the env var to change) |
| **What it does NOT do** | charge anything, or gate a chain an operator wrote to reach a vendor **first** |

Three properties are worth stating because each is a mistake somebody would
otherwise make:

- **It is a read.** Nothing has been spent at the moment it is asked, and a
  probe that incremented a tally would make merely CONSIDERING the cloud count
  against the ceiling — so a run that ended up on a local model anyway would
  still have moved the process closer to its cap.
- **It asks about the scopes the call would be charged to**, read off the same
  context the charge would use. Asking only the process-wide budget would let a
  per-scope latch be discovered one call too late.
- **A chain that STARTS at a vendor is untouched.** The ceiling governs falling
  back to paid inference, not choosing it; refusing there would break every
  cloud-quality policy in the tree.

### A ceiling refusal is not a park

They look alike from a distance — both stop the work — and they are different
answers with different resumes:

| | `every_door_shut` | `ceiling_reached` |
|---|---|---|
| What changed | nothing is reachable | a paid provider IS reachable and the cap is hit |
| Who fixes it | the world (a lid opens, somebody signs in) | a person, by raising a limit |
| Re-checked | yes, every 5 minutes by the work sweep | **no** — polling would rediscover a number nobody touched |

Both park the run as a `v1:work:approval` of kind `inferenceUnavailable`,
naming every door tried and why each did not open. That approval is where a
person says "use a paid provider for this run", and it replaces the retired
per-plan `cloudApproved` metric — a field on a concept (`v1:planner:plan`) that
went with memql#5000.

## Environment reference

### Per-goal no-paid-provider ceiling

A human API caller may set `createGoal.noPaidProviders` to true. This is a
persisted execution ceiling on the goal and its runs, inherited by child work
and restored on another replica. It covers every routed modality, including
embeddings. The router refuses federation entries before selection and checks
again during call-time fallback or cloud consent. A step's model override
cannot remove it. Leaving the field absent preserves ordinary routing.

The local goal evaluator requires this ceiling in addition to its exact fleet
source pin. It also reads and records a fleet embedding binding before admission
so asynchronous indexing has a local source. It does not treat a zero dollar
ceiling as no-spend: the ordinary numeric ceiling uses zero to mean unset.

### Layer 0 — kill-switch
| env | default | meaning |
|---|---|---|
| `MEMQL_LLM_KILL_SWITCH_ENABLED` | true | master switch |
| `MEMQL_LLM_MAX_TOTAL_CALLS` | 0 (unlimited) | cumulative admitted-call cap |
| `MEMQL_LLM_MAX_TOTAL_COST_USD` | 0 (unlimited) | cumulative est-$ cap |
| `MEMQL_LLM_COST_INPUT_PER_MILLION` | 15.0 | est. input price |
| `MEMQL_LLM_COST_OUTPUT_PER_MILLION` | 75.0 | est. output price |

### Layer 1 — rate ceiling + loop breaker
| env | default |
|---|---|
| `MEMQL_LLM_LOOP_GUARD_ENABLED` | true |
| `MEMQL_LLM_LOOP_MAX_REPEAT` | 8 |
| `MEMQL_LLM_RATE_GUARD_ENABLED` | true |
| `MEMQL_LLM_MAX_CALLS_PER_WINDOW` | 20 |
| `MEMQL_LLM_RATE_WINDOW_SECONDS` | 10 |
| `MEMQL_LLM_BG_MAX_CALLS_PER_WINDOW` | 40 |

### Layer 2 — automation budget
| env | default |
|---|---|
| `MEMQL_AUTOMATION_BUDGET_ENABLED` | true |
| `MEMQL_MAX_AUTOMATION_EXECUTIONS_PER_WINDOW` | 600 |
| `MEMQL_MAX_AUTOMATION_EXECUTIONS_PER_AUTOMATION` | 120 |
| `MEMQL_MAX_AUTOMATION_EXECUTIONS_PER_ROW` | 30 |
| `MEMQL_AUTOMATION_MAX_CHAIN_DEPTH` | 16 |
| `MEMQL_AUTOMATION_QUEUED_MODE_DEFAULT_MAX` | 10 |
| `MEMQL_AUTOMATION_BUDGET_WINDOW_SECONDS` | 60 |
| `MEMQL_MAX_UNGUARDED_AUTOMATION_EXECUTIONS_PER_WINDOW` | 50 |

### Layer 3 — loop caps
| env | default |
|---|---|
| `MEMQL_TOOL_LOOP_MAX_ITERATIONS` | 120 (clamp 200) |
| `MEMQL_TOOL_LOOP_MAX_REPEAT_FAILURES` | 3 |
| `MEMQL_TURN_WALLCLOCK_TIMEOUT_SECONDS` | 180 |
| `MEMQL_AUTHORING_MAX_REPAIRS` | 4 |

### Layer 4 — per-scope budget
| env | default | meaning |
|---|---|---|
| `MEMQL_LLM_SCOPE_GUARD_ENABLED` | true | per-(plan-lineage, work run) latch |
| `MEMQL_LLM_SCOPE_MAX_CALLS` | 600 | cumulative calls per scope |
| `MEMQL_LLM_SCOPE_MAX_COST_USD` | 20.0 | cumulative est-$ per scope |
| `MEMQL_LLM_SCOPE_IDLE_TTL_SECONDS` | 3600 | prune scopes idle this long |

The per-scope latch is **on by default** (unlike the process caps): a scope is
one plan-lineage or one work run, so a runaway within one scope is
unambiguous and latching it kills just that loop — other plans are
unaffected and no restart is needed. Caps sit above the 120-iteration per-turn
cap so a single deep turn never trips them. Scopes are stamped via
`ContextWithBudgetScope` at the streaming + non-streaming agent loops, the
work run's execution context (`app/work_execution_context.go`) and the compile
pass (`integrations/work/compile.go`).

## Reproducing a runaway safely (local)

Set tight caps in the cluster env, then trigger any loop and confirm spend is
hard-bounded and the loop terminates with a clear signal:

```bash
MEMQL_LLM_MAX_TOTAL_CALLS=3          # Layer 0 hard stop after 3 calls
MEMQL_TOOL_LOOP_MAX_REPEAT_FAILURES=1
MEMQL_MAX_AUTOMATION_EXECUTIONS_PER_WINDOW=20
```

The kill-switch alert `LLM KILL-SWITCH LATCHED` is the terminal signal; after
it, every LLM call returns a 402 and no vendor request is made.

## AI-call caching (cost *reduction*, not just bounding)

The layers above stop a runaway. Caching is the orthogonal lever: it stops
re-paying the LLM for an answer we already have. Two cache layers sit on the
`ai()` / `InvokeAI` path, checked in order:

1. **Exact-hash cache** (`component/memql/ai_cache.go`,
   [5.8](https://github.com/znasllc-io/memql/issues/1972)). Keys on
   `sha256(templateId|provider|rendered-prompt)`. Cheap, byte-exact — so
   `"thanks!"` and `"thanks"` miss each other. On by default
   (`MEMQL_SI_CACHE_DEFAULT_ENABLED`, ceiling `MEMQL_SI_CACHE_MAX_SECONDS`).

2. **Semantic (vector) cache** (`component/memql/ai_semantic_cache.go`,
   [5.9](https://github.com/znasllc-io/memql/issues/1973)). On an exact-hash
   *miss*, for an **enabled classification namespace only**, embeds the rendered
   prompt and looks up the nearest neighbour in the `semantic_ai_cache`
   pgvector table scoped to that namespace. Returns the cached result **only**
   when cosine similarity is at or above the namespace's threshold; otherwise it
   falls through to a fresh LLM call and stores the result back. This is what
   collapses `"thanks"` / `"thx"` / `"thank you"` onto one cache row.

```
ai() call
  └─ exact-hash cache  (byte-exact)            → hit? return
       └─ semantic cache (enabled namespace)    → similarity ≥ threshold? return
            └─ fresh LLM call → store into BOTH caches
```

### The wrong-answer guard (read before enabling a namespace)

A near-duplicate prompt does **not** in general imply an identical correct
answer: *"cancel the meeting"* and *"confirm the meeting"* are lexically and
semantically close yet demand opposite answers. So the semantic cache is:

- **opt-in per namespace.** A *classification namespace* is a stable
  input→label call type (failure symptom, complexity triage, domain-shape). The
  enablement registry (`ai_semantic_cache_registry.go`) is **empty by
  default** — shipping the layer changes no AI behaviour. NEVER enable it for
  free-form generation (agent prose replies): two prompts that "look similar"
  there can demand entirely different correct outputs.
- **threshold-gated.** Per-namespace cosine floor, conservative default
  **0.95**. A neighbour below the floor is rejected and counted
  (`SemanticCacheStats.BelowThreshold` — the guard firing).

### Enabling a namespace (the path)

1. Confirm the call is a classification / structured-output call with a stable
   input→label mapping.
2. Run the false-positive eval (`ai_semantic_cache_eval_test.go`) over that
   namespace's near-duplicate pairs (should hit) and lexically-close-but-opposite
   pairs (must not hit). Pick the lowest threshold whose false-positive rate
   stays under the agreed bound (0% over the curated opposite corpus at 0.95).
3. Register it in `defaultSemanticNamespaces()` **or** set
   `MEMQL_SI_SEMANTIC_CACHE_NAMESPACES=<ns>:<threshold>[:<ttlSeconds>]` (comma-
   separated). Start above the eval-chosen threshold and tune down only with
   telemetry.

The strongest first-enablement candidate is the work spine's symptom
classification (`work.classify_symptom`, the `classifySymptom` prompt)
— structured output, one of five labels plus a reason, high redundancy across
runs. It ships **not** enabled: turning it on changes a live healing decision
path and is a deliberate, eval-gated, owner-signed-off follow-up, not a risk
taken to satisfy acceptance criteria.

| Env var | Default | Meaning |
| --- | --- | --- |
| `MEMQL_SI_SEMANTIC_CACHE_NAMESPACES` | *(empty)* | enable namespaces: `ns:threshold[:ttlSeconds]`, comma-separated |
