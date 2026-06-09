---
title: Planner observability follow-up
audience: internal
status: draft
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Planner observability follow-up

**Status:** scoped, not implemented. Folded into epic memql#836
(goal-resolution architecture: cost-aware routing, model tiering,
token estimate + user-approval threshold). Pick up the observability
pieces there.

> **CORRECTION (2026-06-03):** the "switched the default to
> `streamClaudeSonnet`" claim below is misleading. The prompt's
> `@defaultProvider` was flipped to Sonnet, but the **`plannerAgent`
> agent record** (`dsl/agents/plannerAgent.memql`) pins
> `providerConfig.provider: "reasoningClaudeOpus"`, and the agent-level
> provider OVERRIDES the prompt default — so the planner kept running
> Opus 4.6 + extended thinking (~10x cost, per the prompt's own
> comment). This was a direct contributor to a ~$250 runaway on a
> trivial request on 2026-06-03. The fix (cheap-by-default, escalate to
> reasoning only on need) is memql#838 under epic memql#836; the quick
> win is flipping that one `provider:` line.

**Origin:** 2026-05-17 debugging round — operator hit $35 on a single Plan
because the planner was on `reasoningClaudeOpus` (Opus + extended thinking)
and there was no in-app visibility into LLM call count or cost. That round
flipped the prompt default to `streamClaudeSonnet` and added per-call usage
logging on the Anthropic stream provider, but the integrated per-plan
token rollup that the cockpit can display is still missing. This doc
spells out the work.

---

## What ships tonight (already in `main`)

1. `dsl/agents/prompts.memql` -- `plannerAgent` default provider
   flipped from `reasoningClaudeOpus` -> `streamClaudeSonnet`.
   Per-instance flip back to Opus + thinking is still one line if a
   workload proves too ambiguous for Sonnet.
2. `component/memql/si_providers.go` --
   `anthropicStreamProvider.CallStream` now matches the
   `CallChatStreamWithTools` pattern: tallies `input_tokens`,
   `output_tokens`, `cache_creation_tokens`, `cache_read_tokens` from
   the stream events and logs a single `anthropic stream: usage` line
   per call, including `cost_usd` computed via `Pricing.CostFor`.
   Grep `docker logs memql-planner | grep "anthropic stream: usage"`
   to count calls + total spend per session.
3. `memql-cockpit/cli/planner/view.go` -- per-row `+Ns / Nm Ss / Nh Mm`
   timer for `planning / routing / running / paused / awaitingFeedback /
   needsAgent`. Terminal statuses (`succeeded / failed / cancelled`)
   and `queued` (intentional idle, waiting on user) deliberately get
   no timer.

This gets the operator out of the immediate cost-blindness hole. The
rest of this document covers what's still missing.

---

## What we still owe

### 1. Surface per-call usage through the SI provider interface

Today `SIProvider.Call(ctx, prompt) (any, error)` and the streaming
variants give the caller the model output and nothing else. The
Anthropic provider tracks usage internally (the tally we just added)
but discards it after the log line; OpenAI variants don't track usage
at all yet. The planner agent loop calls `engine.InvokeSI` and gets
back a string -- it has zero programmatic visibility into how many
tokens that call consumed.

Proposed shape:

```go
// component/memql/common (or component/memql/si_usage.go)
type CallUsage struct {
    InputTokens         int
    OutputTokens        int
    CachedInputTokens   int
    CacheCreationTokens int
    InputCostUSD        float64
    OutputCostUSD       float64
    CachedInputCostUSD  float64
    TotalCostUSD        float64
    // Filled by the runtime; providers only need to populate token
    // counts and call Pricing.CostFor on the way out.
    Provider string
    Model    string
}
```

Provider interface widens (one path):

```go
type SIProvider interface {
    Call(ctx context.Context, prompt string) (any, *CallUsage, error)
}
```

Either every provider grows the new return slot, or `CallUsage` lands
on a separate interface and providers opt in:

```go
type UsageReporter interface {
    LastUsage() *CallUsage
}
```

The opt-in route avoids a flag-day refactor; the strict route gives a
compile-time guarantee that no new provider regresses to "I forgot to
report usage." Either is workable. Recommendation: opt-in
(`UsageReporter`) for the first pass since the OpenAI + placeholder
providers (10+ surfaces) don't all track usage today.

### 2. Propagate `*CallUsage` through `siRuntime`

`component/memql/si_runtime.go` -- after `entry.Client.Call(ctx, text)`
returns, the runtime knows the resolved provider entry and can pull
`LastUsage()` off it. Two consumers:

a) **Event payload.** Stamp the existing
`ai.completion.finished` event with usage fields so anything
subscribed (audit, billing, the planner integration's accumulator)
can read it without changing call sites:

```go
r.publishEvent(events.TopicAICompletionFinished, ..., map[string]any{
    "templateId":        invocation.TemplateId,
    "provider":          providerName,
    "durationMs":        ...,
    "inputTokens":       usage.InputTokens,
    "outputTokens":      usage.OutputTokens,
    "cachedInputTokens": usage.CachedInputTokens,
    "totalCostUSD":      usage.TotalCostUSD,
})
```

b) **Direct return.** Add `InvokeSIWithUsage(ctx, templateId, data) (any, *CallUsage, error)`
to the engine surface for callers (like the planner) that prefer
synchronous capture. The plain `InvokeSI` stays as-is for callers
that don't care.

### 3. Planner attribution of usage to a Plan

The planner agent loop calls `InvokeSI` and knows the active `planId`
at the call site. After each invocation, it accumulates onto
`Plan.tokenSpent` (already defined on `v1:planner:plan`) and
`Plan.costSpentUSD` (new field, add to concept).

Sketch in `integrations/planner/agent_loop.go`:

```go
resp, usage, err := l.engine.InvokeSIWithUsage(ctx, "plannerAgent", data)
if err != nil { ... }
if usage != nil {
    l.recordPlanUsage(ctx, planId, usage)
}
```

```go
func (l *PlannerAgentLoop) recordPlanUsage(
    ctx context.Context, planId string, u *memql.CallUsage,
) {
    // Read current spent / cost, add, write back via
    // mutationUpdatePlanStatus. Optimistic write -- a concurrent
    // status update wins is acceptable since the next call's
    // accumulator picks up the merged value on next read.
    plan, _ := l.loadPlan(ctx, planId)
    spent := getInt(plan, "tokenSpent") + u.InputTokens + u.OutputTokens
    cost  := getFloat(plan, "costSpentUSD") + u.TotalCostUSD
    q := fmt.Sprintf(
        `mutationUpdatePlanStatus({planId:%q, tokenSpent:%d, costSpentUSD:%f})`,
        planId, spent, cost,
    )
    _, _ = l.engine.Execute(systemActorContext(ctx), q)
}
```

Concept change: add `costSpentUSD float` next to the existing
`tokenSpent int` field on `v1:planner:plan`. Update
`mutationUpdatePlanStatus`'s `args` block + `update` body and
`planFull` shape.

### 4. Cockpit display

Once `Plan.tokenSpent` / `Plan.costSpentUSD` are flowing, the cockpit
row chrome shows them inline. Suggested layout per row (matches the
operator's ask):

```
simulate a fight superman vs spiderman, who wins?
planning  2026-05-18 04:23  +47s  1.2K tok  $0.012
```

Formatting helpers in `cli/planner/view.go`:

- `formatTokens(int)` -> `"187"` / `"1.2K"` / `"4.3M"`
- `formatCost(float64)` -> `"$0.012"` / `"$1.40"` / `"$12.30"`

Bump the row from two lines to three only if cost crosses a
threshold the operator cares about, OR pack inline if there's room.
Pack-inline is the recommended default; the row is already 80+ cols
wide on most terminals.

### 5. Per-phase timers (operator's stretch ask)

Tonight's timer is per-Plan (`elapsed since createdAt`). The
operator asked for per-phase timers too -- "how long it's been
planning vs working vs whatever." That requires the planner to
stamp `phaseStartedAt` on each phase transition (Plan.phases[].kind
already has the slot, just nothing writes it today). Once that
field is populated, the cockpit's tasks pane can render a per-row
timer for the active phase the same way the plans pane does.

Stamping path: `stampPhases` in `agent_loop.go` writes phases with
`startedAt` set on whichever phase is `status=active`; transitions
update the previous phase's `completedAt` and the new phase's
`startedAt`. Cockpit reads from `plan.phases` directly -- no new
queries needed.

### 6. (Optional) Billing concept

If per-plan rollup turns out to be the entry point for actual
billing reports, mirror the per-call usage onto a dedicated
`v1:planner:llmCall` concept so each call's tokens / cost / latency
are queryable in their own right (the Plan field would still hold
the rollup for at-a-glance display). Out of scope for the first
pass; flag for the next conversation.

---

## Order of operations for the follow-up session

1. Concept change: add `costSpentUSD` to `v1:planner:plan` +
   plumb through `mutationUpdatePlanStatus` args + `planFull` shape.
2. `UsageReporter` interface in `component/memql`, optional opt-in.
3. Implement `LastUsage()` on `anthropicStreamProvider` (data already
   captured; just expose). OpenAI variants implement next; placeholders
   can return nil.
4. `siRuntime` reads `LastUsage()` after `Call`, stamps it onto the
   existing `ai.completion.finished` event, AND exposes
   `InvokeSIWithUsage(ctx, ...) (any, *CallUsage, error)`.
5. Planner integration's agent loop captures usage from
   `InvokeSIWithUsage` and writes to Plan.tokenSpent / costSpentUSD.
6. Cockpit row formatter: `formatTokens` + `formatCost`, append to
   the per-plan subtitle line.
7. Per-phase timer: `phaseStartedAt` stamping in `stampPhases` +
   cockpit task-pane render.

Items 1-6 are one PR each; #7 is a follow-on that can land independently.

---

## Why per-call logging buys us time until the integrated rollup ships

The Anthropic provider's `anthropic stream: usage` log already carries
provider, model, tokens, and cost. A grep + awk gets you a per-session
spend total today:

```
docker logs memql-planner 2>&1 \
  | grep "anthropic stream: usage" \
  | grep -oE 'cost_usd=[0-9.]+' \
  | awk -F= '{s+=$2} END {print "$" s}'
```

That's the operator-facing escape hatch while the cockpit display
lands. The integrated rollup replaces it; this stays as a fallback
when the cockpit isn't running.
