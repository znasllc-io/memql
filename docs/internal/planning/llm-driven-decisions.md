---
title: LLM-driven decisions, cached intelligently
audience: internal
status: draft
area: internal
sinceVersion: 0.9.0
owner: znas
---

# LLM-driven decisions, cached intelligently

**Status:** Proposed. Not shipped.
**Priority:** High — current state is a slow-growing bandaid pile that misfires in user-visible ways.
**Owner:** TBD.
**Related:** cognition (affirmation guard, intent classification, dispatch routing); knowledge (domain inference); planner (kind discrimination); any future decision logic.

## What's broken today

The cognition pipeline (and a handful of other places) makes runtime decisions through hardcoded string-match heuristics. Concrete examples currently in `integrations/cognition/cognition_handler.go`:

```go
func looksLikeCorrection(text string) bool {
    starters := []string{
        "sorry i meant", "sorry, i meant", "actually no,",
        "actually no ", "actually i meant", "no i meant", ...
    }
    ...
}

func previousAgentAskedQuestion(spaceId string) bool {
    // returns endsWithQuestionMark(lastAgentText)
}
```

These work until they don't. Production case (2026-05-02):

> Nova: "If you want, I can also show you how I'd summarize a book in a few lines."
> User: "yeah show me"
> *(silence — affirmation guard fired, dispatch suppressed)*

The intent classifier tagged "yeah show me" as `follow_up`. The hardcoded escape hatch (`previousAgentAskedQuestion`) only matches a literal `?` — Nova's offer ended in a period, so the guard fired and silenced the room. Same conversation, second hit:

> Nova: "If you want, I can do the same for a book you name."
> User: "the dark tower"
> *(silence again)*

The reactive fix is to add more hardcoded patterns. That's the trap: every miss spawns another `looksLike*` function with another phrase list, and the codebase accumulates a graveyard of magic strings that nobody understands and nobody can audit. Score-engine signals get overridden by the heuristics: in this exact case, the score engine returned `winner=Nova, score=35, confidence=high` — and the guard discarded it.

The conductor (`runConductor*` paths in the same file) ALREADY makes LLM-driven decisions with structured outputs — `ConductorPlan` with `Primary`, `Sequence`, `ChimeIn`, `Severity`, etc. The pattern works. The problem is partial adoption: some decisions go through it, others sit in hardcoded heuristics one layer up that override its signal.

## What we want

Migrate decision-making from hardcoded heuristics to LLM-driven structured outputs, with cached classifications backing them so we don't pay LLM cost on every turn. Concretely:

1. **Decisions return structured data, not free text.** Typed schemas (Go structs with JSON-schema validation), so callers can rely on field shape and add fields over time without breaking consumers.

2. **Cache the *classification*, not the *decision*.** A classification ("this is a follow-up that carries an action verb, addressed to the last speaker") is stable across conversations and vector-cacheable. The decision ("Nova should answer this turn") depends on per-conversation state and stays fresh per turn.

3. **Vector-keyed caches for high-redundancy classifications.** "thanks!" / "thx" / "thank you so much" should hit the same cache row. Hash-keyed caches miss every time on near-duplicates.

4. **Cache infrastructure verified before migration.** The AI cache and DSL cache are referenced in code but their actual hit / miss / eviction behaviour in production isn't measured. If caches silently don't hit, this whole strategy ships LLM-everywhere with no cost containment.

5. **Hardcoded heuristics retained ONLY for truly mechanical things** (slash commands, exact `@mention` parsing, structural shape checks). Everything inferential goes LLM + cache.

## Phased plan

### Phase 0 — Verify cache infrastructure

**Goal:** know what the existing caches actually do before migrating decisions onto them. Without this every later phase is built on sand.

**Tasks:**
- Read both cache implementations (AI cache in `component/memql/si_*.go`, DSL cache via `@cache(ttl=...)` annotations) end-to-end. Document key derivation, eviction policy, TTL handling.
- Add metrics: hit count, miss count, eviction count, age-at-hit per cache. Wire to a debug endpoint or per-component counters that show up in `make status` / BFF logs.
- Run the dev stack for a week of normal usage. Capture baseline numbers.
- Audit: are there callers whose keys include high-cardinality data (timestamps, request ids) that prevent hits? Are there obvious double-calls inside a single request that should hit cache the second time?
- Fix the bugs found.

**Deliverable:** a short report with hit rates per cache type and a list of any infrastructure bugs we have to fix before Phase 2.

**Risk gate:** if a cache turns out to be fundamentally broken (e.g. always cold, or evicting too aggressively), address it before moving on. Migrating decisions onto a broken cache makes the system slower and more expensive in production while looking fine in dev.

---

### Phase 1 — Define the patterns

**Goal:** design the conventions we're going to use repeatedly. Get them right once so the migration goes fast.

**Tasks:**

**1.1 Structured-output convention.** Where do schemas live, how do prompts integrate with them, what does versioning look like.
- Schemas as Go structs in `component/memql/decisions/` (or per-area: `integrations/cognition/decisions.go`).
- Prompt files in `prompts/v1/<area>/<decision>.memql` use `@responseSchema(...)` (or however the existing structured-output prompt path expresses this — verify against `ChatStructuredProvider` infrastructure).
- Add fields, never remove. New consumers handle new fields; old consumers ignore them. Each schema documents its evolution rules in a comment block.

**1.2 Cache pattern — classification vs decision.** Documented split:
- *Classification* = stable, semantic-level fact about an input ("this message carries an action request", "this domain is finance-shaped"). Cached. Vector-keyed when input space is high-redundancy. TTL-able.
- *Decision* = contextual call against live state ("Nova should answer this turn"). Computed fresh per call from cached classifications + live state.
- Clear in code: classification functions return `ClassificationResult { label, confidence, ... }` with `cache: hit | miss`. Decision functions take ClassificationResults as inputs and return `Decision { ... }` with no cache layer.

**1.3 Vector classification cache primitive.** SHIPPED (Epic 5, [#1973](https://github.com/znasllc-io/memql/issues/1973)). A cache layer that takes `(input_text, classification_namespace) → cached AIResult | miss` using vector similarity. Embeds the rendered prompt via the existing embedding provider, looks up the nearest neighbour in the `semantic_ai_cache` pgvector table scoped to the namespace, and returns the cached result only at/above a per-namespace similarity threshold (conservative default 0.95). Sits AFTER the exact-hash cache on the `ai()` path: exact-hash first, then semantic lookup on exact-miss for ENABLED namespaces only, else fresh LLM call + store-back into both. Enablement is opt-in per namespace and **empty by default** (the primitive ships enabled-for-none — turning a namespace on is an eval-gated follow-up). A false-positive eval (`component/memql/ai_semantic_cache_eval_test.go`) gates the wrong-answer risk: 0% FP rate over a curated opposite-pair corpus at the default threshold. See `component/memql/ai_semantic_cache.go` + the AI-call caching section of `docs/public/ai/llm-cost-control.md`. Phase 2 (§2.3) wires `cognition.message_intent` as the first live namespace.

**1.4 TTL + invalidation strategy.**
- Long TTLs (24h+) only for context-free classifications: language detection, profanity check, broad-shape intent ("greeting in isolation").
- Short TTLs (5–15min) for context-sensitive ones: per-conversation state classifications.
- Explicit busting on known events: agent training (re-classify under new capabilities), domain content change, agent role change.
- No caching at all for genuinely contextual decisions.

**Deliverable:** a design doc covering the schemas, infrastructure primitives, and invalidation rules. Reviewable before Phase 2 starts. (This file becomes the cross-reference for Phase 2's reference implementation.)

---

### Phase 2 — Reference migration: cognition's affirmation guard

**Goal:** migrate one decision end-to-end as the template for the sweep. Pick the one that's currently broken AND small enough to ship cleanly.

**Why this one:** the affirmation guard in `cognition_handler.go:351-383`. It's actively misfiring (the Nova case), small in scope, has a clear contract (one decision: should this turn dispatch?), and has existing tests we can extend.

**Tasks:**

**2.1 Define the structured outputs.**
- `MessageClassification { intent: string, carriesAction: bool, addressing: enum, confidence: number, conversationalKind: enum, ... }` — semantic facts about the user message, conversation-context independent.
- The conductor's existing `ConductorPlan` already covers the dispatch decision. Augment if needed; don't fork.

**2.2 Write the classification prompt.**
- Inputs: user message text, last 2-3 turn summaries (for context, not full transcript).
- Output: `MessageClassification` structured.
- Provider: cheapest model that gives reliable structured output (gpt-5-nano or claude-haiku class). Per-call ~50-200ms, ~hundreds of tokens.

**2.3 Wire the classification cache.**
- Cache namespace: `cognition.message_classification`.
- Vector-keyed (so "thanks" / "thx" / "thanks!" hit the same row).
- TTL: 24h. Bust on schema version bump only.

**2.4 Replace the hardcoded guard.**
- Remove `looksLikeCorrection`, `previousAgentAskedQuestion`, `agentTextInvitesReply`, `userFollowUpCarriesAction` from cognition_handler.go.
- Replace the affirmation guard block with: classify message → check if score engine has a high-confidence winner → dispatch when classification + score engine both point to a real ask.
- The conductor stays the source of truth for "what does the agent say"; this just stops the pre-conductor suppression from misfiring.

**2.5 Test the migration.**
- Walk through all the cases the original guards were protecting against: "thanks", "ok", "got it", "lol", farewells. Verify silence.
- Walk through the cases that broke today: "yeah show me" after an offer, "the dark tower" after a prompt, action-verb follow-ups, corrections. Verify dispatch.
- Verify cache hits on repeat phrases (no LLM call on the second "thanks").
- Measure latency: cold path (cache miss + classification + dispatch) vs warm path (cache hit + dispatch). Cold path should be acceptable — it's still cheaper than the conductor itself.

**2.6 Document the pattern.**
- A short walkthrough in `docs/core/` (or wherever cognition arch lives) showing: input → classification (cached) → live decision (fresh) → action.
- This becomes the template every subsequent migration follows.

**Deliverable:** a working LLM-driven affirmation/follow-up guard, with vector caching, that doesn't silence the room when the user actually asked for something.

**Risk gate:** if the classification-vs-decision split turns out awkward in practice, revisit before sweeping the rest of the codebase.

---

### Phase 3 — Sweep the rest of the codebase

**Goal:** apply the Phase 2 pattern to every other hardcoded decision in the system.

**Tasks:**

**3.1 Inventory.** Grep + read for hardcoded decision logic. Build a list with: location, what it decides, current logic complexity, replacement priority. Likely candidates from a quick scan:

- *cognition*: intent classification fallbacks, `endsWithQuestionMark` and friends, `looksLikeFarewell`-style helpers.
- *knowledge*: domain inference (which knowledge domains apply to a query), validation status heuristics.
- *planner*: plan-kind discrimination (what kind of work is this?), task-kind selection within a plan.
- *router/conductor*: any `if intent == "x" { ... } else if intent == "y" { ... }` chain.

**3.2 Prioritize.**
- High-priority: decisions that misfire and cause UX bugs (the cognition guard pattern from Phase 2 — likely 3-5 sites in cognition alone).
- Medium-priority: decisions that work today but are brittle (knowledge domain inference, planner kind discrimination).
- Low-priority / leave-alone: truly mechanical decisions (slash command parsing, structural shape checks, exact-string operator commands). These don't benefit from LLM reasoning.

**3.3 Migrate in batches.**
- Each batch: 3-5 decisions, scoped to one area. Reduces cross-area entanglement and lets each batch ship + soak independently.
- Reuse the Phase 2 cache infrastructure; add new classification namespaces per decision type.
- Tests + verify cache hit rates per batch before moving on.

**3.4 Update CLAUDE.md.**
- Add a section: "When adding a new decision in this codebase: structured output schema, prompt under `prompts/`, vector-cached classification, fresh decision against live state. See [reference impl] for the pattern."
- Code-review checklist: any new `looksLike*` / `endsWith*` style heuristic should bounce out unless it's genuinely mechanical.

**Deliverable:** the codebase no longer has hardcoded decision strings (with rare, documented exceptions for mechanical cases).

---

### Phase 4 — Tune

**Goal:** with the pattern in place, optimize. This phase is ongoing; not a one-time sprint.

**Tasks:**

**4.1 Hit-rate analysis.**
- Per-cache hit rate. Low-hit-rate caches mean wrong key shape, wrong threshold, or input space too unique (cache isn't a fit).
- Tune similarity thresholds per namespace.
- Re-key caches whose input includes high-cardinality data.

**4.2 Cost analysis.**
- Track LLM spend on classifications (vs reply generation).
- Identify high-cost low-value decisions paying premium-model tokens for things a cheap model could do.
- Right-size models per decision.

**4.3 Stale-cache refresh.**
- For long-TTL caches, "serve cached, refresh in background" beats cache-miss latency on the next read.
- Background refresh worker for caches that opt in.

**4.4 Observability.**
- Per-decision dashboards: hit rate, p50/p99 latency, cost per decision, classification distribution.
- Alert on unusual classification patterns (sudden spike in `unknown` could mean a new shape we should learn).

---

## Out of scope for this plan

- New product features. This is infrastructure work; it should be invisible to users except as fewer bugs and more consistent latency.
- Frontend changes. Almost all of this lives in memQL.
- Migration of user-facing strings. Those are i18n, not decisions.
- The conductor itself. The conductor already does LLM-driven decisions with structured outputs; we don't re-architect it. We just stop letting hardcoded pre-stages override its signal.

## Sequencing notes

- **Phase 0 unblocks everything.** Don't skip it. If the cache silently doesn't work, every later phase makes the system slower and more expensive without anyone noticing in dev.
- **Phase 1 is design work.** Can run in parallel with Phase 0 instrumentation. Roughly 2-3 days of design + writing.
- **Phase 2 is the longest single chunk** because it includes building the cache primitive, defining schemas, and the first migration. Worth investing time so the sweep in Phase 3 is mechanical.
- **Phase 3 is the bulk of the volume** but should be fast once the reference exists — each batch is hours, not days.
- **Phase 4 is ongoing.** It's how the system gets better over time as real usage tells us where caches are missing and where we're paying for things we shouldn't.

## A note on what NOT to do

The seductive path is to start with Phase 2 (the broken cognition guard) and skip Phase 0 (verify cache works). It feels productive — you get an immediate user-visible win. But Phase 0's value is exactly that it's *not* user-visible: it's the foundation that lets Phases 2-4 pay off compounding-ly. Skip it and we ship LLM-everywhere on top of a cache that may or may not work, and we won't find out until we're on the API bill.

Equally seductive: hand-rolling another `looksLike*` to fix the immediate Nova case while "we get to the plan eventually". Don't. Every additional bandage is a future migration cost (each one has to be re-read, understood, replaced) AND a place where the system silently misfires until we get to it. The Nova case waits until Phase 2 ships.
