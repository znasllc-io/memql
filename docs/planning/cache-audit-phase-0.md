# Cache audit — Phase 0 of the LLM-driven decisions plan

**Status:** Audit complete; instrumentation shipped. Baseline numbers TBD (need a week of dev usage).
**Related:** [llm-driven-decisions.md](./llm-driven-decisions.md) — this is the unblocking prerequisite.

## TL;DR

Two active caches in the system (a third one exists as dead code — `component/cache/cache.go` has no consumers in the build). **Both had metrics disabled.** That's now fixed (Ristretto metrics on for the result cache; manual counters on the SI cache). What you'll see going forward in BFF logs:

```
{"component":"siCache","msg":"cache stats","hits":42,"misses":17,"hitRatio":0.71,"size":59}
{"component":"resultCache","msg":"cache stats","hitRatio":0.93,"hits":...,"misses":...,"keysEvicted":...}
```

Numbers are written periodically (every 5 minutes when the cache is non-empty) and callable on-demand via each cache's new `Stats()` method.

## The three caches

### 1. SI response cache (`component/memql/si_cache.go`)

**What it caches:** LLM call results — chat completions, structured outputs, anything returned by `siRuntime.Invoke`.

**Key:** `sha256(templateId + "|" + provider + "|" + fully-rendered-prompt)`.

**TTL:** Per-call configurable via `@cache(seconds)` annotation in `.memql` files; defaults via `siCacheConfig` (env-driven); hard cap at 300 seconds.

**Storage:** In-memory `map[string]siCacheEntry`. No size limit. Per-process (restart wipes).

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

This file exists in the tree but has zero consumers in the build (`grep -r '"github.com/visionarys-io/memql/component/cache"' --include='*.go'` returns nothing). The `Cache` and `CachedMemoryNodeStore` types are not referenced by anything.

It looks like an older caching layer that got designed but never wired up, or one that got replaced by the result cache + per-concept Ristretto and never removed. No instrumentation needed; flagged for cleanup as a separate follow-up (delete the file, save 600 LOC of confusion for the next reader).

## What changes in this commit

1. **`siResponseCache`** gets atomic hit/miss/expired-on-read/set counters + a `Stats()` method returning a `SICacheStats` snapshot + a `startStatsEmitter(ctx, logger, interval)` goroutine that logs stats every 5 minutes when the cache has been touched.
2. **`resultCache`** flips Ristretto metrics on (was `false` by default in the `ristretto.Config`); gets a `Stats()` method returning a `ResultCacheStats` snapshot from the underlying `*ristretto.Metrics`; same 5-minute log emitter.
3. **Engine bootstrap** spawns the stats emitters once each at startup, scoped to the engine's lifecycle context.
4. **No public API changes.** Existing callers continue to work.

## What does NOT change in this commit

- No new debug HTTP endpoint for stats. The 5-minute log emission is enough for Phase 0 baselining; a real `/debug/cache-stats` endpoint is a Phase 4 task once we know what dashboards we want.
- No alerting on low hit rates. Same reason — we need a baseline first to know what "low" means.
- No fix for the unbounded SI cache or the unbounded fallback map. Those are real bugs but addressing them requires picking eviction policies, which is its own design decision and shouldn't block the Phase 1+2 reference migration.

## What we'll know after a week of dev usage

Once the metrics emit have soaked, we can answer:

- **SI cache hit rate.** If it's near 0%, the hash-keyed shape is killing us and the vector-classification cache primitive (Phase 1.2) is more urgent than ever.
- **Concept cache hit rate.** Should be high (>80%) if our query workload has any repetition. Low hit rate would point to either over-aggressive eviction or cache-key bugs.
- **Result cache hit rate.** Should depend heavily on query workload. Hard to predict.
- **Eviction frequency.** If high on either ristretto cache, the size limits are too tight.

These numbers feed directly into Phase 1's design (where to put the new vector cache, how to size it) and Phase 2's tuning.
