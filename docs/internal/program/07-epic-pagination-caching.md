---
title: Epic 5 — Pagination & caching
audience: internal
status: historical
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Epic 5 — Pagination & caching (efficiency hardening)

Stop "pulling the universe." Make pagination the sane default for list queries
and actually turn on the caching that's already built but dormant. **Session:
S5. Starts at G1. Produces no gate other epics depend on.**

**Repos:** `memql` (engine + DSL), the product carrier repo, the product SPA repo
(frontend), the identity portal, `memql-cockpit`.
**Prior art (build on, do NOT restart):**
`docs/internal/planning/cache-audit-phase-0.md`,
`docs/internal/planning/llm-driven-decisions.md`.
**Tracking:** epic memql#1964 (children #1965–#1997).

## Approach (as shipped)

- **Pagination — enforce at authoring + backstop.** Every list-returning query
  must declare `paginate`/`sort` OR an explicit `@unbounded("reason")`; the
  engine applies a default 50-row cap as a runtime backstop for anything
  unmarked. Keyset (createdAt,id) cursors are the continuation primitive.
- **Caching — invalidation first, then adopt, then graduate.** Event-bus
  invalidation built BEFORE caching turned on; `@cache(ttl=)` adopted on hot
  reads; pure reads graduated to default-on with a `v1:identity:*` denylist,
  per-actor keying, and `@nocache` opt-out. Cross-node eviction rides a single
  broadcast `cache.invalidate.*` channel.
- **AI cache — baseline then semantic.** Exact-hash cache baselined; semantic
  vector cache added as a conservative, per-call-type-gated fast-follow
  (enabled-for-none by default).

## Children (all merged)

| # | Item | Status |
|---|------|--------|
| #1965 | 5.1 Pagination authoring enforcement + default-cap backstop | merged |
| #1985 | 5.12 Keyset cursor pagination — engine continuation primitive | merged |
| #1966 | 5.2 Paginate the hot offenders (tokens, spaces, messages) | merged |
| #1967 | 5.3 Backfill pagination across remaining list queries | merged |
| #1968 | 5.4 Cache invalidation primitive (event-bus driven) | merged |
| #1969 | 5.5 Adopt `@cache(ttl=)` on hot read queries | merged |
| #1970 | 5.6 Broadcast invalidation channel + default-on caching (denylist) | merged |
| #1971 | 5.7 Remove dead cache code | merged |
| #1972 | 5.8 Verify + baseline the exact-hash AI-call cache | merged |
| #1973 | 5.9 Semantic (vector) AI-call cache | merged |
| #1974 / #1997 | 5.10 Frontend: consume pagination + 5.10b follow-up | merged |
| #1993 | 5.13 Remove vestigial offset pagination | merged |
| #1975 | 5.11 Epic verification | this doc |

---

## Verification results (5.11 / memql#1975)

Independent fresh-sub-session verification run **2026-06-22** against
`origin/main` @ `c6ef8d06` (all 13 build issues merged). Evidence below; full
command transcripts on issue #1975.

### Summary

| Task | Result |
|------|--------|
| 1. Audit — zero unmarked unbounded list queries | **PASS** |
| 2. Whole-module + conformance green on main | **PASS** (+1 regression found & fixed) |
| 3. Bounded fetches, no full-table pulls (tokens/spaces/messages) | **PASS** (real Postgres) |
| 4. Cross-node cursor continuation + cache invalidation | **PASS** (deterministic; live cluster = operator soak) |
| 5. Cache hit ratios (result + AI exact-hash + semantic eval) | **PASS** (synthetic; live ratio = operator soak) |
| 6. No silent truncation of `@unbounded` reads | **PASS** |
| 7. Multi-tenant per-actor cache-key isolation | **PASS** |

### Task 1 — Pagination audit (zero unmarked list queries)

`go run ./scripts/audit-pagination --strict` exits **0**. 202 queries scanned:
single-row 19, aggregate 1, **bounded-list 72**, **@unbounded-marked 110**,
**unmarked-list 0**. Every `@unbounded` carries a non-empty reason. The audit
tool and the enforcing `dsl.TestPaginationAuthoringRule` share the same
classifier (`component/language/pagination`), so they cannot drift.

### Task 2 — Whole-module + conformance green

`GOWORK=off go test ./...`: **95 packages ok, 0 failures**. With a live Postgres
(`MEMQL_DATABASE_DSN`) the DB-gated suites run too — still 95 ok / 0 fail
**after** the regression fix below. Passing Epic-5 tests include
`TestPaginationAuthoringRule`, the keyset/cursor suite, `TestResultCache_*`
invalidation, `TestCacheTTLForBundle_DefaultOn*`, `TestActorCacheKeyComponent_*`,
`TestResultCacheInvalidation_CrossNode`, the AI exact-hash baseline, and the
semantic-cache eval.

**Regression found and fixed (memql#1975).** With a live DB,
`TestSearchUsers_ActiveAndLimitApplied` failed: the `searchUsers` MCP tool
errored `multiple paginate() directives are not supported` on **every** call.
Root cause: the 5.3 backfill (commit `8874f917`) added `sort` + `paginate 50`
to `querySearchUsers`, but the `searchUsers` tool handler in `dsl/memql/tools.memql`
already wrapped it in an outer `paginate(querySearchUsers(...), $args.limit)` —
two nested `paginate()` directives, which the engine rejects. Single-node test
runs without a DB skipped this path, so the break shipped silently. **Fix:** the
inner `querySearchUsers` now declares only `sort "createdAt","desc"` (no inner
`paginate`); the tool handler's outer `paginate($args.limit)` remains the single,
caller-driven window. Audit stays at 0 unmarked (the query is bounded-list via
`sort`); the test passes; whole module green.

### Task 3 — Bounded fetches (real Postgres, no full-table pulls)

Hot offenders confirmed paginated on main: `queryWorkerTokensForUser`,
`queryActiveSpaces`, `querySpaceUtterances` each carry `sort "createdAt","desc"`
+ `paginate 50`. Engine-level proofs against a live TimescaleDB
(`component/memql/keyset_pagination_db_test.go`), all PASS:
`WalksFullSetNoOverlapNoGap`, `StableUnderConcurrentHeadInsert`,
`PushesSQLPredicate` (keyset WHERE pushdown, no scan-and-discard),
`SortMismatchRejected`, `EqualCreatedAtTieBreak`. A realistic-scale load test
added this session (`TestKeysetPagination_LoadBoundedFirstPageWalksFullSet`):
**500 rows, pageSize 50** → first page bounded to exactly 50 (NOT a 500-row
pull), keyset walk reconstructs all 500 in createdAt-desc order across 10 pages
with no dup / no gap. Token-store keyset drain proven in
`component/identity/{pat,workertoken}/store_paging_test.go`.

### Task 4 — Cross-node (mesh) proofs

Proven **deterministically** (always-in-CI, no live cluster):
`component/node.TestResultCacheInvalidation_CrossNode` (two engines, two buses,
real EventBridge forward + inbound republish through the real routing rules) —
a write on replica A evicts a default-cached read on replica B. Routing tests
`TestEvaluateRouting_CacheInvalidateBroadcast` + `..._PerConceptCacheRulesRetired`
confirm a **single** broadcast rule `{Pattern:"cache.invalidate.*", TargetType:""}`
with **zero per-concept routing rules** (`component/node/routing.go`). Keyset
cursor codec/walk + replica-agnosticism (cursor carries only the (createdAt,id)
position + sort signature, no session state) proven in
`component/memql/{cursor,keyset_pagination}_test.go`. The live 2-replica
confirmations (`test/clustere2e/{keyset_cursor,result_cache_invalidation}_test.go`,
build tag `clustere2e`, need `MEMQL_E2E_TOKEN` + a running cluster via
`make cluster-e2e` / `make up SERVERS=2`) exercise the identical path and
remain an **operator soak** — not runnable in this environment (genesis/k3d
cluster), called out per the verification posture.

### Task 5 — Cache hit ratios

`resultCache.Stats()` and `aiResponseCache.Stats()` both expose
`Hits`/`Misses`/`HitRatio` and emit `hitRatio` every interval via
`startStatsEmitter` (the operator-readable BFF log line). Deterministic
exercises: AI exact-hash baseline (`TestAIRuntimeCacheStatsBaseline`) — cold
miss → identical-call HIT (provider call count stays 1) → different-prompt miss,
**hitRatio = 1/3**. Semantic eval (`ai_semantic_cache_eval_test.go`):
false-positive rate **0.0%** at threshold 0.95 and across the full 0.70–0.99
sweep, near-duplicate hit rate **100%**, **6 of 7** provider calls avoided
(~14 tokens saved). Live production hit-ratio is a usage soak: read the
`resultCache` / `aiCache` `hitRatio` emitters from BFF logs after real traffic —
that number is an operator task, not this session.

### Task 6 — No silent truncation of `@unbounded` reads

The `@unbounded` rewriter injects an explicit `paginate(filter, 1000000)` window
(`UnboundedPaginateWindow`, `component/language/parser/rewriter.go`), proven by
`TestQueryUnboundedInjectsExplicitPaginate`. Because the plan carries an explicit
window, the 50-row unmarked-list backstop does NOT engage; the read is bounded
only by `MaxWindow` (5000, far above any legitimate `@unbounded` set's
cardinality). Spot-checked live: an `@unbounded`-shaped query over 120 seeded
rows returned all 120 (not 50), while `paginate 50` over the same set returned
50. Bare unmarked filters ARE correctly capped at 50 (the backstop).

### Task 7 — Multi-tenant per-actor cache-key isolation (highest stakes)

`TestActorCacheKeyComponent_DistinguishesCallers`: user-A and user-B produce
**distinct** cache-key components (no cross-user leak); the same caller is
stable; no-access → `anon`. `TestPlanReferencesActor`: a
`payload.ownerUserId == actor.userId` filter is detected as actor-dependent.
Wiring confirmed in the live engine path (`component/memql/engine.go` ~L586):
when `planReferencesActor(plan.Root)` is true the cache signature is prefixed
`"actor:" + actorCacheKeyComponent(ctx)` before the key is built, so two
different users hitting an owned query key independently. Cursor isolation is
also folded into the key (page 2 cannot serve page-1's cached rows). Identity
concepts are denylisted from default-on caching entirely
(`TestCacheDenylist_IdentityNeverDefaultCached`).

### Verification environment

- `origin/main` @ `c6ef8d06`, isolated worktree, `GOWORK=off`.
- Live Postgres: `timescale/timescaledb:latest-pg16`, all `.up.sql` migrations
  applied, DSN `postgres://memql:memql_local_dev@localhost:5432/memql`.
- One reusable load test added (`keyset_pagination_db_test.go`,
  Postgres-gated/hermetic). One real regression fixed (`querySearchUsers` +
  `searchUsers` tool, above).

### Epic disposition

Acceptance met: every list query paginated or `@unbounded("reason")` (audit 0
unmarked), tokens/spaces/messages fetch bounded first pages + keyset cursors
with no full-table pulls, result-cache invalidation correct cross-node with no
stale reads, AI exact-hash baseline captured, semantic cache false-positive rate
0%, frontend consumes pages. **Epic #1964 ready to close** once this PR merges.
Remaining operator-soak items (not blockers): live 2-replica clustere2e run +
live production hit-ratio read from BFF logs.
