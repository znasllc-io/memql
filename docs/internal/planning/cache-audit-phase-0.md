---
title: Cache audit — Phase 0 of the LLM-driven decisions plan
audience: internal
status: draft
area: internal
sinceVersion: 0.9.0
owner: znas
---

# Cache audit — Phase 0 of the LLM-driven decisions plan

**Status:** Audit complete; instrumentation shipped. Exact-hash AI cache
verified live + synthetically baselined (5.8, #1972). The real live-traffic
hit ratio lands in Epic-5 verification (5.11, #1975) once the emitter has
soaked over real dev usage.
**Related:** [llm-driven-decisions.md](./llm-driven-decisions.md) — this is the unblocking prerequisite.

> **Naming note (2026-06-22, #1972).** Epic 1's AI rename has since landed in
> this area: `si_cache.go` → `component/memql/ai_cache.go`, `siResponseCache`
> → `aiResponseCache`, `siCacheConfig` → `aiCacheConfig`, `SICacheStats` →
> `AICacheStats`, and the runtime moved to `ai_runtime.go` (`aiRuntime.Invoke`).
> The stats **log line is now `aiCache: stats`** (key/value slog, not the
> `{"component":"siCache"...}` JSON shown in the older example below). Two
> residual `SI`-isms are intentionally deferred per #1917/#1918 and are NOT in
> scope for 5.8: the env knobs `MEMQL_SI_CACHE_DEFAULT_ENABLED` /
> `MEMQL_SI_CACHE_MAX_SECONDS`. Aligning those to `MEMQL_AI_*` is a small,
> low-value follow-up (it churns Epic-1's deferred env-rename work) — track it
> with #1917/#1918, do not fold it into the caching epic.

## TL;DR

Two active caches in the system (a third one exists as dead code — `component/cache/cache.go` has no consumers in the build). **Both had metrics disabled.** That's now fixed (Ristretto metrics on for the result cache; manual counters on the AI cache). What you'll see going forward in BFF logs:

```
aiCache: stats  hits=42 misses=17 hitRatio=0.71 expiredOnRead=3 sets=58 skippedSetsZero=0 size=59
{"component":"resultCache","msg":"cache stats","hitRatio":0.93,"hits":...,"misses":...,"keysEvicted":...}
```

Numbers are written periodically (every 5 minutes when the cache is non-empty) and callable on-demand via each cache's new `Stats()` method.

## The three caches

### 1. AI response cache (`component/memql/ai_cache.go`)

**What it caches:** LLM call results — chat completions, structured outputs, anything returned by `aiRuntime.Invoke` (`component/memql/ai_runtime.go`).

**Confirmed live on the invocation path (5.8, #1972).** The cache is read +
written inside the real `aiRuntime.Invoke`: `engine_bootstrap.go` constructs the
runtime (`newAIRuntime(..., e.aiCacheConfig)`), `Invoke` derives the key, does a
`r.cache.get(key)` before the provider call and `r.cache.set(key, result, ttl)`
after a miss. A cache hit short-circuits the provider entirely and emits
`events.TopicAICompletionFinished` with `cached: true`. The hit/miss/expired/set
counters increment on exactly those paths.

**Key:** `buildAICacheKey(templateId, provider, renderedPrompt)` =
`cacheId(trim(templateId) + "|" + trim(provider) + "|" + renderedPrompt)`, where
`cacheId` is the engine's content-hash (`cacheIdEngine.FromString`, SHA-256-based
— `cache_id.go`). Same three-part identity as the original `sha256(templateId|provider|prompt)`
design; near-duplicate prompts ("thanks!" vs "thanks") still produce distinct keys.

**TTL:** Per-call configurable via `@cache(seconds)` (carried as
`AIInvocation.CacheSeconds`); defaults via `aiCacheConfig` (env-driven —
`MEMQL_SI_CACHE_DEFAULT_ENABLED` / `MEMQL_SI_CACHE_MAX_SECONDS`, default
enabled, default 60s); hard cap at 300 seconds (`clampAICacheSeconds`,
`maxAICacheSeconds`). `cacheTTL`: an explicit per-call `0` disables caching for
that call; otherwise the per-call value (clamped) wins, else the default max if
`DefaultEnabled`, else off.

**Storage:** In-memory `map[string]aiCacheEntry`. No size limit. Per-process (restart wipes).

**Eviction:** Lazy only — expired entries deleted when read. Cold entries (never read after expiry) accumulate forever in memory.

**Was missing:** Hit / miss / eviction counters. Hit/miss signal does emit on the event bus (`events.TopicAICompletionFinished` carries `cached: bool`) but nothing aggregated it.

**Now has:** Atomic counters wrapped around `get`/`set` returning a `Stats()` snapshot. Logs stats every 5 minutes when the cache is non-empty.

**Known issues for the LLM-driven-decisions plan:**

- **Hash-keyed = no near-duplicate hits.** "thanks!" vs "thanks" vs "thank you" all hit different keys. For classifying user-message intent (high-redundancy input space), this is the wrong shape — it'll have a tiny hit rate. **This is exactly the problem the new vector-classification cache primitive solves.**
- **Unbounded growth.** Not urgent in dev but a follow-up before this becomes the backbone of cognition decisions.
- **Per-process.** A multi-node cluster has N independent caches. Same input hits N times before any node has it cached. For now we ignore this; cross-node cache sharing is a Phase 4 concern.

### 2. Query result cache (`component/memql/result_cache.go`)

**What it caches:** Full `ExecuteResult` (graph bundle + projection) for query plans annotated with `@cache(ttl=...)`.

**Key:** Plan signature derived from query string + timestamp + limit/offset/depth + sort signature + select signature + shape signature.

**TTL:** From `@cache(ttl=...)` on the function definition; explicit `@cache(0)` means don't cache.

**Storage:** Ristretto-backed (LFU eviction, max-cost = configured `size`). Bounded.

**Eviction:** Ristretto handles LFU + TTL automatically.

**Was missing:** Ristretto's `Metrics` config field defaulted to `false` — Ristretto's own hit/miss/cost-evicted counters were not collected.

**Now has:** `Metrics: true` enabled on the `ristretto.Config`. The cache exposes a `Stats()` method returning Ristretto's `*Metrics`. Same 5-minute log emission.

**Known issues:**

- **Plan signature includes timestamp.** Most queries don't pass `@timestamp(...)`; for those the signature is fine. For time-windowed queries with explicit timestamps, every distinct timestamp = miss. Probably fine.
- **No global-vs-tenant key isolation flagged.** I scanned for cross-tenant leakage and the partition is folded into the plan signature via the executed SQL (which `WHERE partition = ...`s), so a `default` tenant's cached result can't leak into `acme`'s cache lookups. Verified — not a bug.

### 3. ~~Concept cache (`component/cache/cache.go`)~~ — DEAD CODE

This file exists in the tree but has zero consumers in the build (`grep -r '"github.com/znasllc-io/memql/component/cache"' --include='*.go'` returns nothing). The `Cache` and `CachedMemoryNodeStore` types are not referenced by anything.

It looks like an older caching layer that got designed but never wired up, or one that got replaced by the result cache + per-concept Ristretto and never removed. No instrumentation needed; flagged for cleanup as a separate follow-up (delete the file, save 600 LOC of confusion for the next reader).

## What changes in this commit

1. **`aiResponseCache`** gets atomic hit/miss/expired-on-read/set counters + a `Stats()` method returning an `AICacheStats` snapshot + a `startStatsEmitter(ctx, logger, interval)` goroutine that logs stats every 5 minutes when the cache has been touched. (Originally shipped as `siResponseCache`/`SICacheStats`; renamed by Epic 1 — see the naming note above.)
2. **`resultCache`** flips Ristretto metrics on (was `false` by default in the `ristretto.Config`); gets a `Stats()` method returning a `ResultCacheStats` snapshot from the underlying `*ristretto.Metrics`; same 5-minute log emitter.
3. **Engine bootstrap** spawns the stats emitters once each at startup, scoped to the engine's lifecycle context.
4. **No public API changes.** Existing callers continue to work.

## What does NOT change in this commit

- No new debug HTTP endpoint for stats. The 5-minute log emission is enough for Phase 0 baselining; a real `/debug/cache-stats` endpoint is a Phase 4 task once we know what dashboards we want.
- No alerting on low hit rates. Same reason — we need a baseline first to know what "low" means.
- No fix for the unbounded AI cache or the unbounded fallback map. Those are real bugs but addressing them requires picking eviction policies, which is its own design decision and shouldn't block the Phase 1+2 reference migration.

## Baseline (5.8, #1972)

The exact-hash AI cache is confirmed **live and self-consistent**. Two parts:

### Synthetic baseline (committed, deterministic)

`TestAIRuntimeCacheStatsBaseline` (`component/memql/ai_runtime_test.go`) drives
the real `aiRuntime.Invoke` path against a mock provider and asserts that the
Phase-0 `Stats()` counters move correctly:

| Step | Lookup | `Stats()` result | Provider calls |
|------|--------|------------------|----------------|
| 1. first call ("Ada"), cold cache | miss | hits=0, misses=1, sets=1, size=1 | 1 |
| 2. identical call ("Ada") | **hit** | hits=1, misses=1 | 1 (unchanged — short-circuited) |
| 3. different prompt ("Bea") | miss | hits=1, misses=2 | 2 |

Result: **hitRatio = 1/3 ≈ 0.333** over the three lookups; an identical AI call
hits and never reaches the provider, a different rendered prompt misses. This
proves the cache actually caches and that the telemetry the live baseline will
read is wired correctly. If this test ever regresses, any hit-ratio reported from
BFF logs is meaningless.

### Reading the real live-traffic baseline (procedure — number lands in 5.11)

A representative production hit ratio needs a real usage soak, which is Epic-5
verification's job (5.11, #1975). To read it:

1. Let the cache take real traffic (dev cluster or staging) for a meaningful
   window — the `startStatsEmitter` goroutine logs every 5 minutes, but only
   once the cache has been touched (silent on a quiet system).
2. Grep the BFF/agent/cognition logs for the emitter line:
   ```
   kubectl logs -n memql deploy/bff --all-containers | grep 'aiCache: stats'
   ```
   (run the same across the agent/cognition Deployments locally and on staging).
   Each line carries `hits`, `misses`, `hitRatio`, `expiredOnRead`, `sets`,
   `size`. `hitRatio` is cumulative since process start, so take the **last**
   line per process for the soak total, or subtract two snapshots for a windowed
   rate.
3. Record the observed `hitRatio` (and `size`) on #1975. A near-0 ratio
   confirms the hash-keyed shape is the bottleneck and makes the 5.9 semantic
   cache the priority; a non-trivial ratio sets the bar 5.9 must beat.

> The live number is intentionally **not** filled here — it is 5.11's
> deliverable (this issue, 5.8, delivers the verification + synthetic baseline +
> the procedure). 5.9 (semantic cache) builds on the measured exact-hash
> baseline.

## What we'll know after a week of dev usage

Once the metrics emit have soaked, we can answer:

- **AI cache hit rate.** If it's near 0%, the hash-keyed shape is killing us and the vector-classification cache primitive (Phase 1.2) is more urgent than ever.
- **Concept cache hit rate.** Should be high (>80%) if our query workload has any repetition. Low hit rate would point to either over-aggressive eviction or cache-key bugs.
- **Result cache hit rate.** Should depend heavily on query workload. Hard to predict.
- **Eviction frequency.** If high on either ristretto cache, the size limits are too tight.

These numbers feed directly into Phase 1's design (where to put the new vector cache, how to size it) and Phase 2's tuning.

---

## 5.6 — Broadcast cache-invalidation channel + default-on caching (memql#1970)

5.4 built the invalidation primitive; 5.5 adopted `@cache(ttl=)` on a few hot
reads and forwarded each cached concept's graph writes cross-node with a
per-concept routing rule. 5.6 fixes the architecture, then turns caching on by
default.

### Dedicated broadcast invalidation channel

- **Topic:** `cache.invalidate.<concept>`. Emitted on every graph write
  (`MemQLEngine.publishCacheInvalidate`, at the same commit point as
  `graph.node.created.<concept>` in `executor_mutation.go`).
- **One broadcast routing rule** — `cache.invalidate.*` → forwarded to ALL node
  types (`component/node/routing.go`). Not one rule per concept.
- **Only the result-cache evictor subscribes** (`cache.invalidate.#`, in
  `StartCacheInvalidationSubscriber`). No automations, no other consumers — so
  forwarding it everywhere has ZERO side effects. This is what removes the
  automation-double-fire risk that broadening the 5.5 per-concept *graph-write*
  forwarding would have carried (e.g. `reRouteNeedsAgentOnAgentCreate` on
  `graph.node.created.v1:agents:agent`, memql#1396).
- **Retired (superseded):** the 5.5 per-concept cache routing rules —
  `v1:agents:agentRole`, `v1:agents:skill`, `v1:router:budget`
  (created/updated/deleted) and the `deleted.v1:cognition:utterance` rule.
  Cognition's pre-existing broad `v1:cognition:*` create/update forwarding stays
  (it serves cognition's own delivery, not cache).

### Default-on caching for pure reads

A pure (no-mutation) read now caches by default, with no `@cache` annotation,
under these guards (all in `component/memql/result_cache_policy.go` + the
cache-set site in `engine.go`):

1. **Default backstop TTL: 60s** (`defaultResultCacheTTLSeconds`). A safety
   bound on worst-case staleness; invalidation normally evicts long before it
   lapses. Clamped to `CACHE_MAX_TTL` when configured.
2. **5.4 invariant kept:** a result is cached only when ≥1 dependency concept can
   be named (`dependencyConceptsForResult`). An un-nameable-dependency result is
   never cached (it could never be invalidated).
3. **Staleness denylist** (`cacheDenylistedConceptPrefixes`): a result is NOT
   default-cached if any of its dependency concepts is denylisted. Current list:
   - `v1:identity:*` — authn/authz state must read live (a revoked session /
     downgraded role / deleted credential cannot be served stale).
   The denylist gates the DEFAULT path only; an explicit `@cache(ttl=N)` still
   wins (the author opted in knowingly).
4. **Actor-scoped cache key.** The cache key is built from the plan's canonical
   signature, which renders an actor reference (`payload.ownerUserId ==
   actor.userId`, `actor.role == ...`) identically for every caller. With
   default-on this would let two users collide on one cache entry for an owned
   query. So when a plan references the actor (`planReferencesActor`, spec-aware),
   the resolved actor identity (`actorCacheKeyComponent`: userId|role|identityId)
   is folded into the key. Actor-independent reads keep one shared key across
   callers.

### Opt-out

- `@cache(ttl="0")` — explicit never-cache (unchanged).
- `@nocache` — clearer alias for `@cache(ttl="0")`; maps to `CacheTTL="0"` at
  parse time. Use it for reads that must always be live (auth, monotonic
  counters, presence).
- An explicit positive `@cache(ttl=N)` overrides the default (longer or shorter).

### Cross-node proof

`component/node/TestResultCacheInvalidation_CrossNode` (always-in-CI): two
engines on two buses, the real EventBridge forward + inbound republish, gated by
the `cache.invalidate.*` broadcast rule, with NO per-concept routing rule — a
write on replica A evicts the cached result on replica B.
`test/clustere2e/result_cache_default_on_test.go` is the live-cluster
confirmation on a no-`@cache` read (`queryActiveSpaces`).
