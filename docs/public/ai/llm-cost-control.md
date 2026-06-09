---
title: LLM cost control — defense in depth
audience: public
status: stable
area: ai
sinceVersion: 0.9.0
owner: znas
---

# LLM cost control — defense in depth

This is the canonical map of how memQL bounds LLM spend so that a runaway
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
| **0. Kill-switch** | cumulative call + est-$ **latch** → terminal 402, never drains | process (per-scope: #1144) | **No (latches)** | `component/memql/si_guard.go` |
| 1. Rate ceiling | calls/window → synthetic 429 | process, per-lane | yes (drains) | `si_guard.go` |
| 1. Loop breaker | identical-request repeats → 429 | per-fingerprint | yes (cooldown) | `si_guard.go` |
| 2. Automation budget | executions/window → skip; bounded fail-open | process, per-automation | yes (window) | `component/automations/budget.go`, `cluster_guard.go` |
| 3. Loop terminal conditions | per-turn iteration / convergence / wallclock caps | per invocation | n/a | agent + planner loops |
| 4. Per-scope budget | cumulative latch per space / plan-lineage | per conversation/plan | **No (latches)** | `si_guard.go` via context (#1144) |

Layer 0 is the backstop **behind** every other layer: even when a higher
layer is generous or a loop's terminal condition is loose, the cumulative
kill-switch caps total spend.

## Layer 0 — the kill-switch (`si_guard.go`)

`guardedHTTPClient` wraps the `*http.Client` of all four chat provider
builds (OpenAI + Anthropic, stream + non-stream), so every chat/messages
completion leaves the process through one `guardedTransport`. There it is
checked, in order: (0) kill-switch latch → (1) loop breaker → (1) rate
ceiling → (3) cumulative accounting. Crossing a cumulative cap **latches the
breaker open permanently**: every further call returns a terminal **402**
(non-retryable in both vendor SDKs, so it surfaces as a clear terminal error
to the agent/planner loop) and makes **no vendor request**. It does not drain
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

## Layer 2 — automation budget

A misfiring automation (one that re-fires on its own failure, or a
plan-level loop that re-creates a plan each cycle) is a runaway *multiplier*:
each execution can drive fresh LLM calls. Storm detection used to be
log-only; now a process-global, cross-executor budget (total + per-automation
executions/window) **skips** the execution once a storm blows past it, and
the cluster guard's fail-open path is **bounded** (it admits a capped number
of unguarded executions per window during a DB outage, then fails closed).

## Layer 3 — loop terminal-condition audit

Every LLM-driving loop already carries a per-turn terminal condition; the
residual gap is *cross-turn* cumulative spend, which Layers 0 and 4 close.

| Loop | File | Per-turn cap | Cross-turn / lineage cap | Convergence guards |
|---|---|---|---|---|
| Agent tool loop | `integrations/agent/streaming.go`, `nonstreaming.go` | 120 iters (`MEMQL_TOOL_LOOP_MAX_ITERATIONS`), 180s wallclock (`MEMQL_TURN_WALLCLOCK_TIMEOUT_SECONDS`) | none → **Layer 0 / 4** | 3 repeat-failures (`MEMQL_TOOL_LOOP_MAX_REPEAT_FAILURES`), 3 all-errored rounds, 2 produceArtifact re-delegations |
| Engine SI tool loop | `component/memql/si_tool_loop.go` | 120 iters (`MEMQL_TOOL_LOOP_MAX_ITERATIONS`), 8 tool-calls/iter | none → **Layer 0 / 4** | all-errored guard, identical-call breaker |
| Planner decompose loop | `integrations/planner/agent_loop.go` | 5 iters/cycle (`MEMQL_PLANNER_MAX_ITERATIONS_PER_CYCLE`) | 8 calls + 2M tokens/plan (`MEMQL_PLANNER_MAX_INVOCATIONS_PER_PLAN`, `MEMQL_PLANNER_DEFAULT_TOKEN_BUDGET`) | 2 identical decisions (`MEMQL_PLANNER_MAX_IDENTICAL_DECISIONS`) |
| Cognition conductor | `integrations/cognition/conductor_consult.go` | single structured call (≤3 branch re-invokes) | n/a | n/a |
| Suggest | `component/grpc/` AiSuggest | single call | n/a | n/a |

Verdict: no loop lacks a per-turn terminal condition. The agent / engine tool
loops have no native cross-turn cap and rely on Layer 0 (process) and Layer 4
(per-space / plan-lineage) for cumulative bounding.

## Environment reference

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
| `MEMQL_AUTOMATION_BUDGET_WINDOW_SECONDS` | 60 |
| `MEMQL_MAX_UNGUARDED_AUTOMATION_EXECUTIONS_PER_WINDOW` | 50 |

### Layer 3 — loop caps
| env | default |
|---|---|
| `MEMQL_TOOL_LOOP_MAX_ITERATIONS` | 120 (clamp 200) |
| `MEMQL_TOOL_LOOP_MAX_REPEAT_FAILURES` | 3 |
| `MEMQL_TURN_WALLCLOCK_TIMEOUT_SECONDS` | 180 |
| `MEMQL_PLANNER_MAX_ITERATIONS_PER_CYCLE` | 5 |
| `MEMQL_PLANNER_MAX_INVOCATIONS_PER_PLAN` | 8 |
| `MEMQL_PLANNER_DEFAULT_TOKEN_BUDGET` | 2000000 |
| `MEMQL_PLANNER_MAX_IDENTICAL_DECISIONS` | 2 |

### Layer 4 — per-scope budget
| env | default | meaning |
|---|---|---|
| `MEMQL_LLM_SCOPE_GUARD_ENABLED` | true | per-(space, plan-lineage) latch |
| `MEMQL_LLM_SCOPE_MAX_CALLS` | 600 | cumulative calls per scope |
| `MEMQL_LLM_SCOPE_MAX_COST_USD` | 20.0 | cumulative est-$ per scope |
| `MEMQL_LLM_SCOPE_IDLE_TTL_SECONDS` | 3600 | prune scopes idle this long |

The per-scope latch is **on by default** (unlike the process caps): a scope is
one conversation/space or plan-lineage, so a runaway within one scope is
unambiguous and latching it kills just that loop — other conversations are
unaffected and no restart is needed. Caps sit above the 120-iteration per-turn
cap so a single deep turn never trips them. Scopes are stamped via
`ContextWithBudgetScope` at the streaming + non-streaming agent loops and the
planner decompose loop.

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
