# The result cache -- what it actually does, and the reviewed adoption table

**Date:** 2026-08-25
**Status:** approved (owner decisions D1-D4, 2026-08-25)
**Owner:** `component/memql` (the cache, the policy, the invalidation seam),
`component/node` (the broadcast rule), `component/metrics` (the numbers)

The investigation record behind epic memql#4530 (result-cache trust) and its
four tasks: memql#4531 (the write-path full clear), memql#4532 (metrics),
memql#4533 (docs truth), memql#4534 (adoption + the cross-replica gate).

---

## 1. What is actually there

Verified against the tree, not against comments:

- `@cache(N)` / `@nocache` are first-class query annotations. The parser reads
  a `ttl` keyword argument first, then a bare string, then a **positional
  number** -- so `@cache(300)` and `@cache(ttl="300")` both parse, and the
  positional form is what every live call site uses.
- **Default-on for pure reads.** A hint-free pure read gets a 60s backstop
  (`result_cache_policy.go`, `engine.go` `cacheTTLForBundle`). `v1:identity:`
  is denylisted from the default path only; an explicit hint still wins.
  `MEMQL_CACHE_MAX_TTL` bounds everything, and its default of `0` means **no
  clamp**.
- **One Ristretto instance per node**, keyed on the plan signature plus the
  keyset cursor, the projection/sort/shape signatures, and -- whenever the
  plan depends on the caller -- the resolved actor identity. Row-authz plans
  fold the actor in unconditionally, so the cross-user leak is engineered out
  rather than reviewed for.
- **Invalidation is event-driven.** A write publishes
  `cache.invalidate.<concept>`; one broadcast routing rule carries it
  mesh-wide; each node evicts through its own dependency index.
- **No external cache exists.** Redis is in `go.mod` unimported, and the
  Redis in the k8s manifests is LiveKit's. Per-node in-process, plus broadcast
  invalidation, plus the store as truth, IS the architecture.

### The invariant everything else rests on

`engine.go`'s cache-set site stores a result **only** when it can name at
least one concept the result depends on. A result whose dependencies cannot be
named is never cached, because it could never be evicted. Every cached key is
therefore reachable from the dependency index -- which is what made the full
clear removable in memql#4531 without archaeology: there was nothing for a
sledgehammer to catch that the index misses.

That invariant is now pinned by
`TestResultCacheRefusesToStoreUnnamedDependencies`, because it is load-bearing
and was previously only a comment.

---

## 2. Owner decisions

- **D1 -- default-on 60s stays.** The docs were wrong, not the engine. Pinned
  by `TestResultCachePostureIsPinned` plus a behavioural half, in the
  `TestOAuthDCRDefaultsToDisabled` mold.
- **D2 -- local writes trust the surgical path.** The full clear is gone. The
  local eviction is **synchronous**, because `events.Bus.Publish` hands each
  subscriber its own goroutine and a client re-reading immediately after its
  own write would otherwise race it.
- **D3 -- metrics are Prometheus counters, not new logs.** Exported from each
  cache's existing snapshot rather than counted a second time.
- **D4 -- adoption is a reviewed table, not a spree.** Section 4.

---

## 3. What was wrong, and what it cost

**The write-path full clear.** `executeWrite` called `invalidateCache()` --
`cache.Clear()` plus a full dependency-index wipe -- on the shared insert AND
update path. On whichever node handled a write, the entire result cache died;
the surgical machinery only earned its keep for writes arriving from other
replicas. Removing it is worth more than it looks: on a writing node the cache
previously had a useful lifetime of "until the next write of anything at all".

**No metrics.** Three caches, three 5-minute log emitters, zero Prometheus
series, and a per-query `cached` bool stamped on every query-executed event
that nothing consumed. "Is caching working" was unanswerable; "is caching
working *for this query*" was not even askable.

**Docs wrong in the dangerous direction.** Four documented defects, all of
which led a reader to assume data was FRESHER than it is: caching described as
opt-in (it has been default-on since memql#1970), the same claim in the
attribute matrix, `MEMQL_CACHE_MAX_TTL` documented as defaulting to 300, and
the authoring rules insisting on a quoted `@cache(ttl="N")` form while
rejecting the positional form the whole corpus uses.

Two more were found during the sweep, both saying `0` disables caching:
`docs/public/operate/env-vars.md` ("`0` = no expiry") and the env manifest
("0 is accepted and disables caching"). Both are **inverted**: an operator who
sets `MEMQL_CACHE_MAX_TTL=0` to turn caching off gets a fully-caching engine
with no ceiling at all. That is the worst shape a config doc can have -- it
tells you the lever does the opposite of what it does.

---

## 4. The adoption table (D4)

Eleven queries carry an explicit hint today. (The epic body says thirteen; the
verified count over `dsl/**/queries.memql` is eleven, and the epic's own list
enumerates eleven -- the prose number was wrong.) Everything else rides the
60s default.

The sweep's organising question is **not** "is this read hot". It is:

> Can this read's answer change without a write to a concept it depends on?

If no, invalidation covers it and the default is right, however hot it is. If
yes, the TTL is the only freshness mechanism there is, and the read deserves a
deliberate decision. That question is what the table below is sorted by, and
it is the one an author should ask before reaching for an annotation.

| Query | Effective TTL before | Decision | Reason |
|---|---|---|---|
| the 11 existing `@cache` sites (`dsl/router`, `dsl/agents`, `dsl/rbac`, `dsl/cognition`) | 30-300s explicit | **keep** | already deliberate; catalogs and registries at 300s, the utterance stream at 30s. No change. |
| `siteByHostname` (`dsl/platform`) | 60s default, under the edge resolver's own 30s layer | **`@cache(30)`** | Two undocumented layers, now documented at both ends. Both are event-invalidated, so the TTLs are missed-invalidation backstops -- and the INNER one must not be looser than the outer, or the edge's 30s bound is an illusion on any miss. |
| `awaitingFeedbackPlansPastTimeout` (`dsl/planner`) | 60s default | **`@nocache`** | Time-boundary read: `timeoutAt < now` admits rows as the clock advances with nothing written, so invalidation structurally cannot see it. A deadline sweep that re-reads a cached "nothing overdue" is a sweep that does not run. |
| `codeMetricsInWindow` and the observability reads (`dsl/observability`) | 60s default | **keep default, documented** | **Invalidation-blind by construction** -- see below. In practice the window arguments re-key the trailing edge on every poll, and historical windows are exactly what a cache is for. |
| `expiredWorkerInvocations` (`dsl/worker`) | 60s default | **keep** | The `createdBefore` argument re-keys every run, so it effectively never hits. Nothing to decide. |
| the fleet reads -- `myWorkersWithStatus`, `allWorkersWithStatus`, `workersForUser`, `routingPolicyForOwner` (`dsl/worker`) | 60s default | **keep** | Investigated as the leading `@nocache` candidate and cleared; see below. |
| `liveAppSessionsForUser` (`dsl/worker`) | 60s default | **keep** | The concurrency-cap read. Every transition into or out of `starting`/`running` is a write to `v1:worker:appSession`, so the count the cap is checked against is evicted by the very events that change it. |
| everything under `v1:identity:` | never cached | **keep the denylist** | Confirmed correct and confirmed load-bearing: `authSessionsForUser`'s `expiresAt>now` filter is itself a time-boundary read, so it is doubly right that this prefix never default-caches. |

### The class worth naming: invalidation-blind rows

Invalidation fires from `executeWrite`. Rows that reach the store by any other
path publish **nothing**, so a cached read over them has the TTL and only the
TTL:

- `component/observe`'s `TimescaleSink` writes `code_invocation` with a direct
  `db.NewInsert()`, bypassing the engine write path entirely.
- `code_invocation_1m` / `_1h` are TimescaleDB **continuous aggregates** --
  materialized by a refresh policy, with no MemQL write anywhere in the loop.

This is not a defect to fix; observability is exactly the domain that tolerates
a minute of staleness, and routing rollup refreshes through the engine would be
absurd. It is a property to KNOW, because it is invisible at the call site and
it generalises: **any concept whose rows land by raw SQL is invalidation-blind,
and its reads are only as fresh as their TTL.** That is now written into the
authoring rules as one of the two shapes that genuinely wants `@nocache`.

### The fleet reads: the candidate that did not survive review

The strongest-looking `@nocache` case in the corpus was the Fleet router's
`myWorkersWithStatus` -- read per dispatch, carrying `lastSeenAt`, from which
`online` is derived against a **30-second** window. A 60s cache in front of a
30s liveness window looks obviously wrong.

It is not, and the reason is worth recording so the next reader does not
re-open it:

- `online` is **not projected** into the shape. It is computed in Go from
  `lastSeenAt` against the real `now` at read time, so a cached row does not
  freeze the verdict -- a machine that stops beating is correctly reported
  offline from cached data once the window lapses.
- Every state transition that matters is itself a **write to the same
  concept**: registration, the 15-second heartbeat flush, revocation, and the
  router's own `activeCount` / `lastSelectedAt` stamps. Each one publishes
  `cache.invalidate.v1:worker:registration` and evicts.

So the freshness the 30s window was chosen for is delivered by invalidation,
not by the TTL, and a `@nocache` here would buy nothing while paying on every
dispatch. Recorded as a **no**, with the reasoning, rather than left as a
plausible worry someone re-raises every six months.

---

## 5. What now gates this

| Gate | Where | Catches |
|---|---|---|
| `TestInvalidateCacheForConcept_EvictsOnlyTheWrittenConcept` | `component/memql` | a return of the full clear; a local eviction that is not synchronous |
| `TestResultCacheRefusesToStoreUnnamedDependencies` | `component/memql` | the invariant the surgical path depends on |
| `TestResultCachePostureIsPinned` + the behavioural half | `component/memql` | a silent flip of the default, the backstop, or the denylist |
| `TestResultCacheInvalidation_CrossNode` | `component/node` | the broadcast rule going missing |
| `TestResultCacheInvalidation_CrossNodeThroughTheWriteSeam` | `component/node` | the same, driven through the real write seam; over-eviction across the mesh |
| `TestResultCacheMetricNamesAreStable` | `component/metrics` | a metric rename that would silently empty a dashboard |

Both cross-node tests were verified red with the `cache.invalidate.*` rule
removed and green with it restored, and the surgical-eviction test was verified
red against a restored full clear.

**They are in-process hop tests, deliberately** -- the memql#4352 precedent. A
`clustere2e` lane is skipped on every CI machine and every developer laptop,
and a gate skipped by default cannot be what stands between a feature and the
bug it prevents.

---

## 6. Deliberately not done

- **No dashboard or portal surfacing.** The numbers exist now; deciding what
  to show is a separate piece of work with a different reviewer.
- **No new denylist entries.** The sweep found no credential-shaped or
  consent-shaped concept outside `v1:identity:` that default-caching reaches
  unsafely. Proposals belong in this table, not in a quiet edit to the slice.
- **No change to the broadcast rule or the subscriber contract.** Remote
  behaviour was correct before this epic and is untouched by it.
