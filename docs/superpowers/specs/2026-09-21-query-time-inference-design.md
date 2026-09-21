# Inference at query time -- Design

- **Date:** 2026-09-21
- **Status:** SCOPED, not approved. The owner asked for a capability the platform
  does not have and asked for it to be sized before anyone builds it. D1-D16 are
  rulings recorded for the owner to overturn; each names what it rejected. The
  record deliberately answers the three questions the investigation left open
  rather than noting them, and section 10 argues against the whole feature,
  because a design that cannot say when not to use its feature is advocacy.
- **Owner areas:** `component/language` (a fourteenth tier position, one clause,
  one annotation), `component/memql` (the clause's load gate and its applier, the
  AI runtime's request, the exact-hash cache, the orphan removal),
  `component/router` (one resolution per page rather than per row),
  `component/metrics`, `test/conformance` (a corpus directory and a verdict gate),
  `component/proving` (the scenario that measures the cache), `docs/public`.
- **Depends on:** [levels, policies and
  rules](2026-09-07-levels-policies-rules-design.md) for the one seam every model
  call goes through, and for `Touches`, which this design is the first real
  consumer of. [The DSL v1 language
  freeze](2026-09-13-dsl-v1-language-freeze-program-design.md) D11 for the tier
  manifest, and for `refine`, which is the precedent this clause copies almost
  exactly.
- **Adjacent, and NOT this:** a separate in-flight change restores a BODY-level
  `ai` builtin -- a logic or automation calling a named prompt through
  `MemQLEngine.InvokeAI`. That is a construct call in a statement position and it
  is journaled as a step. This record is the per-ROW query form, which a builtin
  cannot express, and which has none of a statement's governing machinery.

---

## 1. Problem

A person wants a query whose rows each carry a field a model produced, computed
as the query runs: "the open tickets, each with a one-line note on why this one
might matter to someone deciding X". The field is not stored, it depends on an
argument supplied at read time, and it has to flow out to whatever read the
query.

MemQL once had the expression for it. `ai()` carried a projection form, it died
by accident -- the production lived only in a hand-written parser its replacement
never re-implemented, and was later swept as unused parser machinery -- and `ai(`
is in no retired-forms list, so nobody decided to remove it. **No `.memql` file
ever used the projection form**, in this tree's history or in any bundle, so
nothing regressed and nothing complained.

What survives is worse than a clean absence: an AST node with no parser
(`ast.AIExpr`), a converter for a node nothing produces (`convertAIExpr`), an
engine expression nothing constructs (`AIExpression`) with five dead switch arms
behind it, a validator that refuses the expression outside projections
(`validateAIContext`), a shape-template value that is dead at the type level
(`shapeAIValue` / `renderAIValue`), a `CacheSeconds` field wired end to end, and
a semantic-cache layer that is fully built, fully wired, and whose namespace
registry is empty by default with no caller anywhere. Read together, the tree
looks as though the feature exists. It does not.

**The runtime under all of it is healthy and is not the problem.**
`MemQLEngine.InvokeAI` -> `aiRuntime.Invoke` resolves the prompt, builds an
`airoute.ResolveRequest`, routes through the one seam the levels epic built,
interface-checks the modality, validates the data against the prompt's input
schema, checks an exact-hash cache, passes through the model-call journal seam,
and returns. Twenty-two prompts sit on the other side of it. `applyShapeTemplate`
already receives that runtime and already runs **once per row**, after the SQL
page and after `refine`, with the request context in hand. The plumbing for
per-row inference is, mechanically, already there.

What is missing is every decision around it. There is no position in the tier
manifest for a projection. Shape bodies admit no expressions by design, and the
gate that makes that true is one of the few total gates in the DSL. And per-row
inference is **unbudgeted**: a query has no run, so `modelSeam.serve` takes the
"no run on the context" branch, nothing is journaled, `component/work/budget.go`
is never consulted, and the only thing between a 200-row page and 200 provider
calls is a process-global rate ceiling that throttles to 20 calls per 10 seconds
and then lets every one of them through.

## 2. What the tree already has

Read on 2026-09-21 at commit `05a10a5e0`. Section 11 lists what to re-verify.

- **The tier manifest** (`component/language/tiers/`). Thirteen positions, each
  with a tier (P, pushed down to SQL; M, evaluated in process), a node-kind rule
  set, a function rule (`noFunctions` / `pushdownFunctions` / `allFunctions`) and
  a predicate admission. `Positions()` is read by the corpus completeness gate,
  the generated docs table and Sense. `TestEveryNodeKindIsInATier` fails naming a
  kind nobody placed.
- **`refine`** (`component/memql/refine.go`, `PositionQueryRefine`), the twelfth
  position and the exact precedent: a named clause, M tier, a lambda over the row,
  applied **between the SQL read and the bundle**, refused without an authored
  `paginate`, static-cost-checked at load (`tiers.MaxStaticCost`), validated by a
  walk against `tiers.KindAdmission`, and counted by
  `metrics.QueryRefineRows(query, kept, dropped)`. Its page **cursor is minted
  from the last row SQL returned, not the last row refine kept**.
- **`dslclause.StructQueryDirectives`** is the authoritative clause list, pinned
  against the parser's switch by `TestStructQueryDirectivesMatchTheParser`.
  `dslspec/lexicon.go` carries each clause's one-line meaning and grammar.
- **The shape contract.** A shape body is a path list. `validateShapeBody` refuses
  a body with neither `@row` nor `@actor`, refuses `include`, refuses an actor
  path outside the closed envelope, and refuses two paths collapsing onto one
  terminal key; `shape_concept_validator.go` checks every payload path against the
  bound concept at bootstrap. **Every key a shape emits is traceable to a declared
  concept field or one of five actor keys.** That totality is what the load gate
  is for.
- **The runtime template union is wider than the authored surface.**
  `shapeTemplate` already has `shapeRelationFunc`, `shapeMatchExpr`, `shapeJSONFunc`
  and `shapeAIValue` beside `shapeNodeFunc`. The v1 freeze closed the authoring
  surface over those; the runtime forms remain.
- **The one model seam.** Every call builds an `airoute.ResolveRequest` and takes
  what `component/router` returns; a Go AST gate fails the build on a direct
  registry accessor outside the router. `requestForPrompt`
  (`component/memql/ai_request.go`) fills `Level`, `Modality`, `Needs` and
  `PromptName` -- and sets **no `UserId`, no `RequestId` and no `Touches`**.
- **The decision record.** `Router.recordCall` writes one `v1:router:call` row per
  **resolution**, fire-and-forget on a detached goroutine, as a full
  `recordRouterCall` mutation through `engine.Execute` under a synthetic
  `system:router` principal. Read through `routerDecisionsRecent`, gated
  `@requiresRank("admin")`.
- **The caches.** `buildAICacheKey(templateId, providerName, renderedText)` --
  content-addressed on what actually went to the model. Process-local, 5000
  entries, TTL default 60s and clamped at 300s (`maxAICacheSeconds`). Beside it
  the semantic cache is wired (`SetSemanticCache` is called from
  `engine_ai.go`), its registry is empty by default, no invocation sets
  `SemanticNamespace`, and its own comment says: *never for free-form generation,
  where a near-duplicate prompt does not imply the same correct answer.*
- **The journal seam.** `modelSeam.serve`: *"A call with no run on its context
  runs live and is not journaled. That is by far the common case."* A query has no
  run.
- **What actually bounds spend today.** `ai_guard.go`'s RoundTripper: an
  identical-request loop breaker (fingerprints on method+URL+body -- 200 rows are
  200 different bodies, so it never fires), a process-wide rate ceiling (20 calls
  per 10 seconds by default -- which turns 200 calls into 100 seconds, not into
  fewer calls), and a cumulative latching kill switch scoped to the process.
  `component/work/budget.go` reads the RUN's ceilings and there is no run.
  `v1:router:budget` exists as rows, a mutation and a generated SDK method, and
  **no Go code reads it**; it is a dead ceiling, not a place to hang this.
- **The page sizes that matter.** `defaultListCap = 50` (a list query with neither
  `paginate` nor `sort`), `MaxResults = 500`, `MaxWindow = 5000`. And
  **`@unbounded("reason")` rewrites to `paginate 1000000`, clamped to
  `MaxWindow`** -- so a naive "requires paginate" check admits a 5000-row page.
- **Subscriptions do not carry projections.** A `graph.node.*` notification
  carries the flattened row payload gated by `rowAuthzAdmits`
  (`rowauthz_subscription.go`, `server.go`). It does not re-run the query and it
  does not apply a shape.
- **The SDKs are indifferent.** `sdk/gen` emits a typed args struct per construct
  and an untyped `Result` wrapping `*ExecuteResult` for every query. A new
  projection key needs no generator change.
- **The conformance corpus** has five verdicts -- `load_ok`, `refuse_parse`,
  `refuse_load`, `lower`, `evaluate` -- and `TestCorpusCoversEveryTierPosition`
  demands, per position, at least one case the engine accepts and one it refuses.
- **The differential lane** (`test/conformance/differential_db_test.go`) compares
  SQL lowering against in-process evaluation, and selects its cases with
  `if tiers.TierOf(pos) != tiers.TierP { continue }`. Every M position is already
  outside it.

## 3. Decisions

### D1 -- It is a query CLAUSE, not an expression in a shape

The field is declared on the query, in a new `derive` clause beside `refine`, and
it lands on the result **next to** the shape's keys rather than inside them:

```memql
@maxCalls(20)
@description("Open tickets for the caller, each with a note for the chosen lens")
query ticket triageQueue {
  args {
    ownerUserId  string  @required
    /// The decision the reader is making; the note is written for it.
    lens         string  @required
  }
  filter   row => row.ownerUserId == args.ownerUserId && row.status == "open"
  sort     "row.createdAt", "desc"
  paginate 20
  shape    ticketCard
  derive   urgencyNote = ai("ticketUrgency", row => { title: row.title, lens: args.lens })
}
```

`derive <key> = ai("<prompt>", row => <map>)`. The key is the projection key; the
map is the prompt's input, per row.

**Admitting a computed projection into a SHAPE BODY was the obvious move and it
is rejected, for three reasons and one that decides it.**

- It is a REVERSAL, not a restoration. `validateShapeBody` plus
  `shape_concept_validator.go` make every emitted key traceable to a declared
  field or a closed actor key. That totality is what lets a reader know what a
  query returns without running it, what makes the concept-field snapshot mean
  something, and what makes the SDK's concept map honest. Once one key is a
  computed value, the gate stops being total and becomes a list of exceptions.
- It reopens more than it looks. The runtime template union already holds
  traversals, conditionals and JSON serialisation whose authoring surface the v1
  freeze closed. A body that admits one expression form has no principled reason
  to refuse the others, and "a shape that can compute anything is a shape that can
  do anything" is the accurate summary.
- **The decider: a shape is a shared noun and a budget is a per-call fact.** A
  shape is applied by name from any number of queries. A model call inside one is
  paid for by every query that names it, including the ones the shape's author
  never saw, at page sizes they never chose. There is no honest place to put a
  ceiling. On the query, the ceiling sits beside the `paginate` that multiplies
  it, which is the only place the arithmetic can be done.

**A derived field on the CONCEPT was also considered and rejected.** A concept
field is a stored-row contract: `additionalProperties: false`, the concept-field
snapshot, and the rule that removing a field bricks its rows. A derived field has
no stored rows, so the ledger would need a second kind of field; and it would
appear on every read of the concept, including the reads that must never call a
model -- the seed materializer, the retention sweeps, `rowAuthzAdmits` reading
payloads, a connector's mirror write.

**Reusing `refine` was rejected fastest.** `refine` is a predicate: it drops rows.
A model deciding row membership makes the page's row count model-dependent, and
`refine`'s cursor is minted from the SQL page, so paging would become
irreproducible in a way nothing could diagnose.

### D2 -- The fourteenth position, M tier, and `ai` is clause grammar

`tiers.PositionQueryDerive = "queryDerive"`, tier `TierM`, rule set
`inProcessKinds`, functions `allFunctions`, predicates `Refused`. That is
character for character the row `PositionPromptInput` already has, which is the
right analogy: **a derive clause is a prompt input, evaluated per row.** The only
difference is the scope -- the lambda's parameter is bound to the row, exactly as
`refine`'s is -- and a scope difference is what a position is for.

`inProcessKinds` refuses `KindConstructCall`, and that refusal is load-bearing
here rather than inherited: a construct call in this position would run a query or
a **mutation** once per row inside a read.

**The `ai` token is clause grammar, recognised only inside a query body, and is
NOT a catalog function.** This is the sharp edge. `functionAdmission` returns
`Admitted` for any catalogued function at any position whose rule is
`allFunctions` -- seven of the thirteen positions. Cataloguing `ai` would
therefore admit a model call, silently and with no ceiling anywhere, in an
automation condition, a trigger filter, a mutation value, a tool default and a
prompt input. A trigger filter is evaluated once per matching event.
`functions.Lookup("ai")` must return not-found, with a test that says why.

**No new `ast.NodeKind`.** The clause is a directive (like `refine`'s
`RefineExpr`), its head is grammar, and its payload is an ordinary map expression.
So `AllNodeKinds()` is unchanged and thirteen existing rule sets need no edits --
which is the difference between a position that costs a table row and one that
costs a judgement call in fourteen places.

### D3 -- What the differential lane does with an evaluation-only position: nothing, and that must be asserted

The lane already skips every M position (`tiers.TierOf(pos) != tiers.TierP`), so
`queryDerive` costs it no code. **But "it happens to be excluded" and "it is
excluded because it cannot be compared" are different facts, and only the second
survives a refactor.** An `ai()` call has no SQL lowering at all -- not a missing
one, an impossible one -- so there is no second implementation to disagree with.

The change is one assertion, not one branch:
`TestDifferentialLaneExcludesEvaluationOnlyPositions` names `queryDerive`
explicitly and fails if `TierOf` ever returns `TierP` for it or if the lane's
filter stops excluding it. Without it, someone widening the lane to M positions
(a reasonable thing to want, for `refine`) sweeps in a position whose every run
costs money and whose every answer differs.

`MemQLEngine.Init` is unaffected in kind: `lowerAllPushdownPositions` walks P
positions and will not see this one. The load work is `validateDerive`, a mirror
of `validateRefine` -- the kind walk against `tiers.KindAdmission`, the name walk
(the parameter, `args` declared, `actor`, `now`, `config`), `EstimateCost` against
`tiers.MaxStaticCost` -- plus three checks `refine` has no need of: the prompt
resolves in the registry, the map's keys satisfy the prompt's input schema by
name and `@required`, and the projection key does not collide with a key the
shape emits.

### D4 -- The budget is arithmetic done before the query runs, not a counter that trips during it

This is the crux and it is answered, not deferred.

- **`@maxCalls(N)` is REQUIRED on a query carrying a `derive` clause.** No default.
  A defaulted ceiling is a policy nobody wrote, which is the reason `Level` has no
  default either.
- **At LOAD, when the page size is a literal, the arithmetic is checked and a
  query that cannot satisfy both is refused**, naming both numbers:
  `paginate 200 with a derive clause is up to 200 model calls, above @maxCalls(50)`.
  The author raises one or lowers the other, and either way a person looked at the
  number before it existed.
- **`derive` requires an authored `paginate`**, as `refine` does, and
  additionally **`@unbounded` with `derive` is refused at load, by name.**
  `@unbounded` rewrites to `paginate 1000000` clamped to `MaxWindow`, so it
  satisfies a "requires paginate" check while meaning 5000 rows. That is precisely
  the silent path, and it is closed explicitly rather than by arithmetic that
  happens to catch it.
- **When the page size is an argument (`paginate args.limit`), the check runs at
  ARGUMENT BINDING, before the first row is read**, and refuses the call with a
  typed code naming the ceiling and the requested page. It is a bad argument, and
  the engine refuses bad arguments; discovering it halfway down a page is strictly
  worse than refusing it at the top.
- **So the derive loop can never reach the ceiling mid-page.** The runtime counter
  survives as an assertion -- it fires only if the arithmetic was wrong -- and
  when it fires it fails the read.

**At the limit, the PAGE is refused. Not truncated, and not degraded to an absent
field.** Both alternatives were considered:

- *Truncate*: a short page is indistinguishable from exhaustion to a cursor, and
  the cursor is minted from the SQL page. A reader paging through would silently
  skip rows. `refine` does return short pages -- but a row `refine` dropped was
  decided against, and that is its documented contract. A row dropped because the
  money ran out was not decided about at all, and letting one clause's short page
  mean two things destroys the meaning of the other.
- *Degrade the field to absent*: the tempting one, and the one to refuse loudest.
  **In this codebase an absent field means "not measured"** -- `AiSuggestResult.usage`,
  `watchedFolder.originState`, `registration.rttAt`, and `component/proving`'s
  whole `Figure`/`AbsentReason` type exist to keep absence and zero apart. If
  budget exhaustion also produced absence, absence would mean either "the model had
  nothing to say" or "we stopped paying", and no reader could tell. A page whose
  first twenty rows carry the field and whose last thirty do not reads as a
  finding about the data.

Because the ceiling is checked up front, the refusal is never a partial page: it
is a call that did not start.

### D5 -- One resolution per page, N calls under it, one decision row carrying N

`Router.recordCall` writes a `v1:router:call` row per resolution, and it writes it
as a full `recordRouterCall` mutation through `engine.Execute`. Resolving per row
would therefore turn a 50-row page into **50 ledger mutations** -- a database write
amplification that is invisible at the call site and that the volume argument
excluding `v1:worker:invocation` from broadcast applies to exactly.

So the derive applier **resolves once per clause execution** and calls
`entry.Client.Call` per row under that one resolution.

This is more correct as well as cheaper. Every row in the page shares a prompt, a
level, an actor and a moment; a page whose first twenty rows were answered by a
local model and whose last thirty went to a vendor because a machine went offline
mid-page is a page with two provenances and one column. One resolution also fixes
the cache key's provider component for the page, which D7 depends on.

The decision row gains `rowCount` and `calls` (the two differ by the cache hits).
`ai_guard` is untouched and still counts every call, because it lives on the
RoundTripper and sees HTTP, not resolutions.

**And the request is filled in.** `requestForPrompt` sets no `UserId`, no
`RequestId` and no `Touches` today, so a derive call would land in the ledger
unattributed. The derive path sets all three, and `Touches` is set to **the bound
concept id** -- see D10.

### D6 -- Who sees the cost: three readers, three answers, and one refusal

- **The author, at load.** The refusal message in D4 is the primary instrument,
  and it is the only one that arrives before any money moves.
- **The operator, after.** `routerDecisionsRecent`, which now carries the prompt,
  the query, the row count and the call count; plus
  `metrics.QueryDeriveCalls(query, outcome)` with outcomes `computed` / `cached` /
  `refused`, beside `QueryRefineRows`.
- **The caller, during: nothing, and that is refused deliberately.** Threading a
  cost figure into `ExecuteResult` would make every client learn a field that is
  empty on essentially every read, to serve the one query kind that has one. If
  the owner wants it, it belongs on a per-invocation observability record
  (`component/observe`), not on the result envelope.

### D7 -- The cache key is the rendered text, a hit is free, and the semantic cache is refused here

**Cache on what already exists:** `(templateId, providerName, renderedText)`. That
is content-addressed on what actually went to the model, which is exactly right
for a per-row call -- two rows whose relevant fields agree share an answer, and a
row that changed in a way the data expression does not read still hits. Nothing
new is needed.

**A cache hit is zero calls and does not count against `@maxCalls`.** The ceiling
is on CALLS, not on rows. Which makes D4's load-time arithmetic the **cold-cache
upper bound** rather than the expected cost, and the record says so rather than
implying a page always costs its page size.

Two facts that must be written down because they are not obvious:

- **The exact-hash cache is PROCESS-LOCAL.** A cluster runs two replicas of every
  mesh node by default; that is two caches, and a re-read that lands on the
  sibling misses everything. The multi-node rule in the root CLAUDE.md applies
  here in its usual form: the state lives on one node and the next request may not
  be there. Sharing the cache across replicas is out of scope and named as such.
- **A cache hit still costs a resolution today**, because `Invoke` resolves before
  it checks the cache (it needs the provider name for the key). Under D5 the page
  resolves once anyway, so this stops mattering for derive; but the ledger must
  distinguish a resolution that served N calls from one that served none, or a
  cache-warm page reports N decisions and no spend.

**The semantic cache is not reachable from a derive clause, and this is a
refusal, not an omission.** Its own documentation says to use it only for
classification with a stable input-to-label mapping and never for free-form
generation. A derived field over arbitrary row content is free-form generation,
and its failure mode is the worst available: two different rows getting each
other's answer, plausibly, with no signal. `SemanticNamespace` stays on
`AIInvocation` for the classification callers it was built for and the derive path
never sets it.

`@cacheSeconds(N)` is accepted on the clause within the existing 300-second clamp,
defaulting to the config default. `CacheSeconds` is the one orphan that survives
because this design uses it.

### D8 -- The corpus holds the shape of the answer; the proving suite holds the answer

The corpus compares answers. A model-derived field has no answer to compare, and
pretending otherwise produces a gate that passes once and then goes yellow at
3am.

- **A `queryDerive` case may carry `load_ok`, `refuse_parse` or `refuse_load`, and
  nothing else.** `evaluate` and `lower` are refused **by the corpus runner
  itself**, at this position, with a message saying why. Without that refusal
  somebody writes an `evaluate` case, it passes against a warm cache or a stub,
  and the lane teaches everyone to ignore it.
- `TestCorpusCoversEveryTierPosition` is satisfied normally: an accepted case and
  a refused one. The refused cases are the interesting half and there are many --
  no `paginate`, `@unbounded`, missing `@maxCalls`, page above the ceiling, a
  prompt that does not resolve, a map key the prompt does not declare, a
  `@required` prompt field the map omits, a key colliding with the shape's, a
  construct call in the map, a cost estimate over `MaxStaticCost`.
- **What the corpus CAN pin about the accepted side** is everything but the text:
  that the clause loads, that the map's bindings resolve, that the prompt's input
  schema validates the rendered data (`prompt.ValidateData` already runs), and
  that the projection key lands on the result.
- **The answer's properties belong to `component/proving`,** which already has
  cassettes, a fake step registry and a rule that every zero-claim carries a
  negative control. The scenario: one page, a recorded answer per distinct
  rendered text; assert that the call count equals the count of DISTINCT rendered
  texts and not the row count, and that a second identical read inside the TTL
  costs zero. **The negative control is the same scenario with the cache
  disabled**, which must produce a non-zero call count -- because "the cache saved
  N calls" is a counter that never rises on any path if nothing checks it.

### D9 -- Authorization: the row is already admitted; the egress is the router's, and the control already exists

The rows reaching a derive clause have already passed `rowAuthzAdmits` on the read
path -- the clause runs after the SQL page and after `refine`. **So inference over
them discloses nothing to the caller they could not already read.** There is no
new read-authorization question and this design invents no annotation for one.

The real question is egress: the row's contents leave the process, and when the
resolved door is `federation:` they leave the cluster. `@rowAuthz` governs who may
read; it has never said anything about where data may go.

**The control already exists and this is its first real use.** A rule's `@when`
takes `touches`, matched with `startsWith` semantics, and `Touches` has been
almost entirely empty since the levels epic built it. The derive path sets
`Touches` to the bound concept id, so an operator writes one rule:

```memql
@when(touches="v1:shopify:")
@policy("localOnly")
@precedence(90)
rule mirroredDataStaysLocal { }
```

and no mirrored row's contents reach a vendor, for this or any other call that
declares its footprint. Inventing a second egress mechanism beside the rules
system would be a second place to look and a second place to forget.

**Refusing `derive` over a `@origin`-declared mirror concept outright was
considered and rejected.** "Summarize this order" is a legitimate thing an owner
wants, and a blanket refusal is worked around by copying the row into a native
concept -- which is strictly worse: a stale copy AND the same egress, now with
nothing declaring where it came from. The `touches` rule is the control; the
operator doc carries the rule, written out, in the mirror section.

**One gap is named rather than closed.** `ai_guard`'s per-scope latch has scopes
for a plan lineage and a work run, and none for a query or a user. A derive page
is bounded by `@maxCalls` and by the process-wide ceilings, and is NOT bounded per
user. That is acceptable at the page sizes D4 permits and stops being acceptable
if `@maxCalls` is ever allowed a large value; it is recorded here so that decision
is made knowingly.

### D10 -- The field does not ride a subscription, and the cache is the only defence

A `graph.node.*` notification carries the flattened row payload. It does not
re-run the query and it does not apply a shape or a derive clause. **So a derived
field never reaches a live surface through a delta**, and no amount of design
changes that without re-running the query server-side per event, which is the
thing this whole record is trying to bound.

The consequence is the multiplier the brief asks about, and it is worse than per
page: **a client written the obvious way -- retain a `LiveCollection`, re-read the
query on every delta -- pays a page of model calls per delta.** A list with a
busy concept behind it does this dozens of times a minute.

Three things, and only the second is a real defence:

- **Said plainly, in the clause's documentation and in `clients/os/README.md`'s
  rules-a-fourth-app-gets-wrong list: a query carrying a `derive` clause is not a
  live surface.** The engine cannot enforce it -- the client chooses to re-read --
  so it is a rule people have to know, which is the weakest kind and is stated
  first so nobody mistakes the next point for a solution.
- **The cache is what makes a re-read survivable.** After one row changes, that
  row's rendered text is new and every other row's is not, so a re-read costs ONE
  call and passes `@maxCalls` trivially. This is why D7 counts a hit as zero and
  why the TTL is settable: a 60-second default against a delta every few seconds
  is fine, and against a delta every two minutes is nothing at all.
- **And the cache is process-local**, so on two replicas the hit rate is what the
  load balancer decides. A live surface over a derived query on a multi-replica
  cluster is the failure mode this design does not solve and does not claim to.

### D11 -- The SDKs need no change, and that is a fact rather than a goal

`sdk/gen` emits a typed args struct per construct and an untyped `Result`
wrapping `*ExecuteResult` for every query. The derived key appears in the payload
like any other projected key, in Go and in TypeScript alike. `make sdk-gen-check`
stays green with no generator edit. The only SDK-adjacent work is documentation:
the key is not a concept field, so it is absent from `BoundConcepts` and from the
concept map, and a reader who goes looking will not find it. The clause's docs say
where it comes from.

### D12 -- When NOT to use this, which is most of the time

The alternative that already works is `docSummary`: computed once during file
analysis, persisted on `v1:library:file.summary`, best-effort (a failure writes
nothing and never costs the owner a searchable file), and read thereafter by every
reader at zero cost, with a row version and a writer behind it.

**Query-time is genuinely better in one case: the answer depends on the READ.**
When the field takes a parameter the row does not carry -- the lens, the question,
the comparison target -- a persisted field cannot hold it, because there is one
row and many readings. That is the whole justification, and everything else is a
tiebreak:

- the row set is small and the read is rare (an operator's one screen, not a list
  anyone scrolls);
- the answer is cheap to produce and expensive to keep correct, so a persisted
  field would need an invalidation job that is more machinery than the call.

**Tell a reader to persist instead when any of these is true, and the first is a
hard boundary rather than advice:**

- **They want to filter, sort or page by it.** A derived field is computed after
  the SQL page. It cannot be a filter, it cannot be a sort key, and it cannot
  participate in a cursor. A reader who wants "the ten most urgent" wants a
  persisted field, always, and no version of this feature will ever give it to
  them.
- The answer is a function of the row alone. Then it is a field: an automation on
  `node.created` / `node.updated` computes it once and every reader shares it.
- It appears in a live surface, or in anything a client polls (D10).
- It has to be auditable. A persisted field has a row version and a `createdBy`. A
  query-time answer has a `v1:router:call` row and leaves nothing on the thing it
  described -- you can prove a call happened and not what it said about which row.

### D13 -- The orphans: what goes, what is replaced, what survives

- **Deleted**, because this design refuses the shape-level form they implement:
  `ast.AIExpr`, `convertAIExpr`, `AIExpression` and its five switch arms,
  `validateAIContext` (whose whole content is "refuse this outside projections"),
  and `shapeAIValue`.
- **Replaced rather than deleted:** `renderAIValue`. It is the one existing
  function that renders a per-row data template and calls `runtime.Invoke`, which
  is exactly what the derive applier does with an expression instead of a
  template. The new code is its successor and should say so.
- **Survives:** `CacheSeconds` (D7 uses it); `SemanticNamespace` and the semantic
  cache layer, which are not this feature's orphans -- they are a built,
  deliberately-disabled facility with a documented enablement path, and deleting
  them on the way past would be a second decision smuggled into this one.
- **`ai(` joins the retired-forms list either way.** Whether or not this clause is
  built, the shape-level `ai()` projection form should be refused by name with a
  message, because four `.memql` files (`dsl/agents/builtins.memql`,
  `dsl/agents/prompts.memql`, `dsl/memory/logic.memql`,
  `dsl/memory/prompts.memql`) describe it in comments as though it worked, and
  `dsl/agents/builtins.memql` tells an author to reach for it by name. If the
  clause is built, the message names the clause; if it is not, the message names
  the `ai` builtin.

### D14 -- The clause never drops a row

`derive` adds a key. It does not filter. So the page's row count, the cursor and
the bundle are untouched by it, and `refine`'s careful cursor contract needs no
second reading. A derive whose call fails on one row fails the READ, for the same
reason `applyRefine` fails the read on an undecidable row: a silently missing key
on one row of a page is a data pattern nobody can distinguish from a finding.

**One narrow exception is refused explicitly** so that it is not added later
without a decision: a per-row `on error continue` that leaves the key absent. That
is D4's degrade-to-absent argument in a different costume, and it loses for the
same reason.

### D15 -- One derive clause per query

A second clause doubles the page's calls and makes `@maxCalls` ambiguous (per
clause, or for the query?). One clause, one ceiling, one number the author read.
A query that needs two derived fields asks one prompt for both, which is cheaper
anyway.

### D16 -- The clause names a prompt, and the prompt carries `@level`

There is no `@level`, `@policy` or provider pin on the clause. The prompt already
carries `@level` (required since the levels epic), the rules decide the policy,
and `@defaultProvider` survives on the prompt as the explicit pin it is
everywhere else. Adding a second place to declare routing for the one call site
that happens to be new would be the `agent.providerConfig.llm.policyName` mistake
the levels epic deleted: a field that reads like a control and is consulted by
nothing.

## 4. The change

**`component/language`**
- `tiers/tiers.go`: `PositionQueryDerive`, in `Positions()`. `tiers/manifest.go`:
  its row (`TierM`, `inProcessKinds`, `allFunctions`, `predicates: Refused`).
  `tiers/tiers_test.go` and `manifest_test.go`: the counts and the tier map --
  note `TestPositionsAreTheThirteenCorpusDirectories` is named for its count and
  will need renaming as well as editing.
- `parser/`: the `derive` production inside the struct-query body, and a
  `DeriveExpr` directive node in `ast/` (no new `NodeKind`).
- `dslclause/dslclause.go`: `"derive"` in `StructQueryDirectives`.
  `dslspec/lexicon.go`: its one-line meaning and grammar. `dslspec/grammar.go`:
  the lambda rung.
- `annotations/registry.go`: `@maxCalls(N)` and `@cacheSeconds(N)` on a query,
  with long-doc entries.
- `functions/`: nothing -- and a test asserting `functions.Lookup("ai")` is
  not-found, with the widening argument in its comment (D2).
- `parser/v1_refusals.go`: `ai(` as a retired spelling (D13).

**`component/memql`**
- `derive.go` (new): `convertDeriveExpr`, `validateDerive` (the mirror of
  `validateRefine` plus the prompt, schema and key-collision checks), and
  `applyDerive` -- one resolution, then per row: render the map through `EvalExpr`
  in a `deriveScope`, check the cache, call, write the key.
- `engine.go`: `plan.Derive`, peeled off by `applyDirectiveWrappers`, applied
  after the shape and before the result.
- `expr_lower_load.go` / the Init pass: `validateDerive` beside `validateRefine`,
  and the load-time `paginate` x `@maxCalls` arithmetic.
- `ai_request.go`: `Touches`, `UserId` and `RequestId` on the derive request.
- `ai_runtime.go`: a page-scoped entry point that resolves once and serves N.
- deletions and the replacement in D13; `shape_template.go` loses `shapeAIValue`
  and its arms.

**`component/router`** -- `rowCount` and `calls` on `CallRecord` and
`buildRouterCallArgs`; the cached-vs-served distinction so a warm page does not
report spend it did not have.

**`component/metrics`** -- `derive.go`, mirroring `refine.go`.

**`dsl/router/concepts.memql`** -- two fields on `v1:router:call`;
`dsl/router/queries.memql` -- the two fields on `routerDecisionsRecent`'s shape.

**Tests** -- `test/conformance/2026/expr/queryDerive/` (fixture, cases,
`expect.json`); the verdict gate in `corpus_test.go` (D8);
`TestDifferentialLaneExcludesEvaluationOnlyPositions` (D3);
`component/proving/` scenario and its negative control (D8).

**Docs** -- `docs/public/language/memql.md` (the clause, the position, the
ceiling, the retired `ai()` spelling); `authoring-rules.md` (D12, verbatim: what
cannot be sorted by cannot be derived); `docs/public/ai/llm-cost-control.md` (a
layer row, and the honest note that the ceiling is load-time arithmetic rather
than a runtime latch); `docs/public/operate/ai-routing.md` (the `touches` rule,
written out); `clients/os/README.md` (D10); the root `CLAUDE.md`; the generated
tier table and the architecture model.

**Risk areas, ranked.**

1. **The budget arithmetic** (D4). Get it wrong and `@unbounded` walks a 5000-row
   page through. Everything else is recoverable; this is money.
2. **The subscription re-read multiplier** (D10). Unenforceable by the engine,
   defended only by a process-local cache, and it is the way this feature will
   actually hurt somebody.
3. **The manifest widening** (D2). If `ai` is ever catalogued, seven positions
   admit it silently, one of them per event.
4. **The load gate's completeness.** `validateDerive` has more to check than
   `validateRefine` (the prompt, the schema, the key collision) and each miss is a
   runtime failure per row rather than a load refusal.
5. **The corpus flake** (D8). One `evaluate` case is all it takes.

## 5. The narrow first subset

**Build the single-row form and nothing else.**

A `derive` clause is admitted only on a query the pagination checker already
classifies as `pagination.SingleRow` -- a filter carrying the primary-intrinsic id
equality, `row.id == args.id`. At most one row, therefore at most one model call.

What that buys: the position, the clause and its grammar, `validateDerive`, the
prompt and schema checks, the key-collision check, the applier, the router request
with `Touches` and attribution, the cache key, the ledger fields, the metric, the
refusal codes, the corpus directory with its accepted and refused cases, the
verdict gate, the differential-lane assertion, the orphan removal and every line
of documentation -- all built and exercised exactly as the list form needs them.

What it defers: `@maxCalls`, the load-time arithmetic, the `@unbounded` refusal,
the argument-binding check, and the whole of D10. The list form is then **one
decision on proven plumbing**, taken with a working single-row feature in hand and
a real prompt to measure.

It is also plausibly the whole feature. "Open this record and tell me about it for
the decision I am making" is a detail view, and a detail view reads one row.

## 6. Failure modes

- **A prompt the clause names is deleted.** Load refusal, as for a tool handler
  whose target is missing -- `tool_handler_resolution.go` is the precedent and the
  reason it is a load problem rather than a mid-read one.
- **A bundle at `MEMQL_DSL_PATH` ships a derive clause with no `@maxCalls`.**
  Strict boot refuses; `MEMQL_DSL_ALLOW_SKIPS` is the break-glass, as for every
  contract gate. The gates run inside `MemQLEngine.Init` for exactly this reason.
- **The router cannot serve the page's level.** One refusal for the whole page,
  before any row is called, carrying the door report -- which is better than the
  per-row version and is a direct consequence of D5.
- **A provider fails on row seventeen.** The read fails (D14). The sixteen calls
  already made are on the ledger and were paid for; the refusal says so rather
  than implying the page was free.
- **Every row renders the same text** (the map reads a field every row shares).
  One call, forty-nine hits, and the ledger reports `rowCount: 50, calls: 1`. This
  is the good case and it should be visible, because an author seeing it will
  realise they wanted a persisted field.
- **A page runs on replica A, the re-read on replica B.** Full cost again.
  Recorded, not solved (D7).
- **Somebody points a `LiveCollection` at a derived query anyway.** Dozens of
  pages a minute, bounded only by the cache. `@maxCalls` does not catch it -- each
  page is individually legal. The metric is what shows it, which is why
  `QueryDeriveCalls` is labelled by query.
- **An author writes `derive` over a mirror concept with no `touches` rule in
  place.** It works, and another system's rows reach a vendor. Nothing refuses it
  by design (D9); the operator doc is the control and the ledger is the evidence.

## 7. Testing

- **Language:** parse and refuse cases for the clause, `@maxCalls`,
  `@cacheSeconds`; `derive` without `paginate`; `derive` with `@unbounded`; two
  derive clauses; a construct call in the map; `functions.Lookup("ai")`
  not-found; `ai(` refused with the clause named.
- **Load:** the prompt resolves; the map satisfies the prompt's schema by name and
  `@required`; the key does not collide with the shape's; the cost estimate;
  `paginate` x `@maxCalls` refused with both numbers in the message; the bundle
  case under strict boot.
- **Runtime:** one resolution for N rows, asserted by counting resolutions rather
  than by mocking; N distinct texts produce N calls; a repeated text produces one;
  a cache hit does not count against the ceiling; a failing row fails the read; the
  cursor and row count are identical with and without the clause.
- **Request:** `Touches` is the bound concept id; `UserId` and `RequestId` are set;
  a `touches` rule routes a derive page to `localOnly`.
- **Ledger:** `rowCount` and `calls` on the row; a warm page reports one decision
  and zero calls rather than N decisions.
- **Corpus:** the position's accepted and refused cases; the verdict gate refusing
  `evaluate` and `lower` at this position, with its own negative control (a case
  file that would pass if the gate were removed).
- **Differential:** the exclusion assertion (D3).
- **Proving:** the cache scenario and its cache-disabled negative control (D8).
- **Unaffected and asserted so:** `make sdk-gen-check`, the row-authz audit,
  `TestEveryNodeKindIsInATier`, `TestStructQueryDirectivesMatchTheParser`.

## 8. Delivery

Two PRs for the narrow subset, one more for the list form if the owner takes it.

- **PR 1, the position and the clause, single-row only.** The manifest row, the
  grammar, `validateDerive`, `applyDerive`, one resolution per read, the request's
  attribution and `Touches`, the ledger fields, the metric, the corpus directory
  and both gates, the orphan removal, the docs.
- **PR 2, the proving scenario** with its negative control, and the ledger's
  cached-vs-served distinction.
- **PR 3, the list form:** `@maxCalls`, the load-time arithmetic, the
  `@unbounded` refusal, the argument-binding check, the `LiveCollection` rule in
  `clients/os/README.md`, and the measurement that says whether the cache carries
  a real page.

## 9. What this design refuses to do

Named as refusals so that a later session does not read them as gaps.

- **No expressions in shape bodies.** The load gate stays total (D1).
- **No derived field on a concept.** A concept field is a stored-row contract (D1).
- **No `ai` catalog function.** Seven positions would admit it silently (D2).
- **No degrade-to-absent and no truncated page.** Absence means not-measured
  everywhere else in this tree and must keep meaning it (D4).
- **No per-row `on error continue`** (D14).
- **No semantic cache on this path.** Two rows getting each other's answer,
  plausibly (D7).
- **No cost figure on `ExecuteResult`** (D6).
- **No second egress annotation.** `touches` plus a rule is the control (D9).
- **No `@level` or policy on the clause.** The prompt carries it (D16).
- **No filtering, sorting or paging by a derived field**, now or later. It is
  computed after the page (D12).
- **No cross-replica cache.** Named, not solved (D7).

## 10. The strongest argument against building this at all

A derived field cannot be filtered, sorted or paged on, because it is computed
after the SQL page. So it is never the thing a list is organised by -- and the
moment anyone wants to organise by it, the answer is a persisted field, which the
platform already supports and which `docSummary` already demonstrates. D10 then
removes live surfaces, and D12's own advice removes anything a person scrolls.

What is left is one screen: a small page of rows, each annotated with something a
model said, that nobody will sort by and nobody is watching change. **And that
screen can be built today** -- a logic body reads the page, loops, calls the `ai`
builtin the parallel change is restoring, and returns the rows with the field
attached. It is a statement position, so it is journaled; it sits in a run, so
`component/work/budget.go`'s ceilings apply to it; and nothing in this record has
to exist.

Against that, the clause buys declarativeness at the price of a fourteenth
position, a new load gate with more to check than any existing one, a budget that
has to be right the first time, a cost multiplier that a live surface silently
squares, and a permanent asymmetry in the language -- the one clause whose answer
is not a function of its inputs, which the corpus cannot pin and the differential
lane cannot compare.

**The counter, stated so the owner can weigh both:** the loop somebody can write
today has no ceiling, no load-time arithmetic, no ledger attribution and no cache
discipline. "You can already do it" is true, and the thing people can already do
is the unbounded version -- which is an argument for a governed clause rather than
against one, and is the same argument that put `refine` in the language instead of
leaving people to filter client-side.

The honest summary is that the case for this rests entirely on the read-time
parameter (D12). If the owner cannot name a field whose answer genuinely depends
on the reading rather than the row, the persisted field wins on every axis and
this record should stay unbuilt.

## 11. Facts to re-verify before starting

All read on 2026-09-21 at commit `05a10a5e0`.

- That `ast.AIExpr` still has no parser production and no non-test constructor,
  and that no `.memql` file outside comments writes `ai(`.
- That `applyShapeTemplate` still receives `e.aiRuntime` and still runs per root
  id, after `applyRefine` and after `buildGraphBundle`.
- That `modelSeam.serve` still takes the live-and-unjournaled branch with no run
  on the context.
- That `Router.recordCall` still writes one row per resolution through
  `engine.Execute`, and that resolution still precedes the cache check in
  `aiRuntime.Invoke`.
- That `requestForPrompt` still sets no `UserId`, `RequestId` or `Touches`.
- That `v1:router:budget` still has no Go reader.
- That the differential lane still filters with `tiers.TierOf(pos) != tiers.TierP`.
- That `@unbounded` still rewrites to `paginate 1000000` clamped to `MaxWindow`,
  and that `defaultListCap`, `MaxResults` and `MaxWindow` are still 50, 500 and
  5000.
- That the semantic-cache namespace registry is still empty and still has no
  caller.
- That `functionAdmission` still returns `Admitted` for every catalogued function
  at an `allFunctions` position.
- That a `graph.node.*` notification still carries the raw flattened payload and
  applies no shape.
- That `sdk/gen` still returns an untyped `Result` for every query.
- The exact state of the parallel body-level `ai` builtin change, which may have
  landed and may have moved some of this.
