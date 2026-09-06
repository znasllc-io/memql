# The Proving Suite -- Design

- **Date:** 2026-09-06
- **Status:** approved in the 2026-09-06 design round. The four forks below
  (P1-P4) were put to the owner as selectable options and each was answered;
  they are not open. The decisions carried down from the epic body
  (memql#4993) are not open either. Everything else is a recommendation with
  its rationale and what it rejected.
- **Program:** sub-project G of the program agreed on 2026-09-05
  (memql#4961). It sits on the work spine: its first PR after epic A1, its
  second after A2 and A3. All three landed, so this record covers the whole
  epic in one PR.
- **Scope:** `component/proving/` (new, with three PURE sub-packages),
  `cmd/memql-bench/` (new), `dsl/bench/` (new), `test/proving/` (the corpus
  and the cassettes), `.github/workflows/ci.yml` (one job), a new
  `.github/workflows/proving-live.yml`, `component/node/routing.go`,
  `clients/os/src/apps/settings/` (a Benchmarks section),
  `docs/public/overview/proving/scorecard/` and the generated page beside it,
  plus the generated artifacts.
- **Extends:**
  [the work spine](2026-09-05-work-spine-design.md) (the rows this measures,
  the journal it replays from, the three replay modes, the counter split),
  [logs](2026-09-03-logs-design.md) (archive-then-delete, reused nowhere here
  but the shape of the retention argument is the same),
  [deployables run](2026-09-03-deployables-run-design.md) (exactly-once by
  node and heartbeat, which the durability family measures).
- **Closes:** memql#4993, #4994, #4995, #4996.

---

## Why

The owner's brief, in its own terms: a suite of scenarios and benchmarks that
proves the system is viable, works and is fast, measured as "a model by itself
versus the model with MemQL", with statistics that can be watched improving
across releases. A selling point -- **so the numbers must be honest before
they are good.**

That last clause is the whole design. Everything below exists to make a
dishonest number hard to produce and impossible to publish by accident.

The platform already ships a page that takes this seriously.
`docs/public/overview/why-memql-harness.md` opens with *"This page is the
proof, not the pitch"* and carries a table titled **"Claims this page does NOT
make yet"** with a `Proven by` and a `Lands in` column. This epic is the
machinery that table has been waiting for: the numbers, where they come from,
and a gate that stops a claim outliving its number.

---

## What was already decided, and what this round decided

From the epic body (owner, 2026-09-05), not reopened here:

- **The baseline is the same model in a bare tool loop** -- same model, same
  tools, same scenarios, the platform's machinery switched off (no catalog, no
  journal, no replay, no classifier, no healing). Framework-neutral and apples
  to apples; **no other framework is named or measured.**
- **Two tiers.** A CI tier with no provider, replayed deterministically, in
  the slot the harness eval gate left. A live tier against real providers
  (cheap, mid, strong), capped per run and self-skipping past the ceiling.
- **Publishing is rows plus a committed scorecard.**
- **Honesty rules**: medians and spread, N, model ids, commit, date and cost
  on every number; regressions published with improvements; a claim appears in
  README or site copy only when its test is green on `main`.
- **The six families** and the shape of a scenario.

This round's four forks:

- **P1 -- The CI tier publishes ONLY what a replay can honestly measure.** A
  replay is deterministic by construction, so "pass rate and variance over K
  runs" is trivially perfect and wall-clock is a property of the runner. Those
  are `unmeasured` in CI, with the reason attached, until the live tier fills
  them. Rejected: republishing the cassette's recorded tokens as a CI
  measurement (a recording wearing a measurement's clothes), and a two-column
  scorecard (a reader takes the second column for the first).
- **P2 -- The CI gate blocks on structural properties and reports on cost and
  speed.** A scenario that stops passing, a replay that reaches a provider, a
  duplicated side effect, or a failed governance property fails the PR. Cost
  and speed deltas are computed, published and never red. Rejected: thresholds
  on cost and speed (runner noise reds the lane, the first fix is widening the
  threshold until it means nothing, and the structural half dies with it), and
  report-only everywhere (the pass/fail properties are not noisy and deserve
  teeth on day one).
- **P3 -- The live tier lands DISARMED**, `workflow_dispatch` only, no
  schedule. Rejected: an armed nightly with a hard cap. The owner's call, and
  the honest consequence is written into the scorecard rather than hidden: a
  scorecard whose live figures are `unmeasured` says exactly that, and the
  page says when the lane was last dispatched.
- **P4 -- A build gate over MARKED claims.** A published numeric claim carries
  an explicit marker naming the scorecard metric it rests on; a Go gate fails
  the build when a marked claim names a metric the committed scorecard does
  not carry, or when the prose and the scorecard disagree. Rejected: scanning
  every number in `docs/public` (version numbers, ports and table values would
  flood it and the exemption list would become the real policy), and
  hand-maintaining the existing table (which is what "a claim can outlive its
  number" describes).

---

## A. The honesty model

### A figure is a value with provenance, or it is `unmeasured`

`component/proving/figure` is a PURE package -- no engine, no database, no
provider -- and it is where the honesty rules stop being a policy and become
a type.

```go
type Figure struct {
    Metric   Metric      // what was measured
    Stat     *Stat       // nil when unmeasured
    Unit     Unit
    Absent   AbsentReason // "" iff Stat != nil
    Prov     Provenance
}
```

Three properties, each enforced by a test rather than a convention:

1. **`unmeasured` is a value, not a zero.** `Figure` carries either a `Stat`
   or an `AbsentReason`, never both and never neither. `Render` on a figure
   with neither is a programming error that fails loudly rather than printing
   `0`. This is the house rule already written down in three places -- the
   campaigns scorecard's `unmeasured`, `AiSuggestResult.usage`'s absence, and
   the OS's "an absent figure and a zero are different answers".
2. **A `Stat` cannot be a best case.** It carries `N`, `Median`, `P10`, `P90`,
   `Min`, `Max` and `MAD`, and there is **no `Mean` field and no
   single-number constructor**. `NewStat` refuses `N == 0`. A caller who wants
   one number gets the median and is handed the spread with it.
3. **Provenance is mandatory.** `Provenance` carries `Commit`, `Date`, `Tier`,
   `ModelIds`, `CostUSD` and `Runner`. `Figure.Render` and the scorecard
   writer both refuse a figure whose provenance is incomplete for its tier.
   The rule differs by tier and says so: a CI figure needs a commit and a
   date; a live figure needs those plus model ids and a cost.

`AbsentReason` is a CLOSED set, and the closure is the point -- a free-text
reason drifts into "n/a" and stops meaning anything:

| Reason | What it says |
|---|---|
| `notMeasurableOnReplay` | deterministic by construction; only the live tier can answer |
| `seamNotBuilt` | the code that would produce this does not exist yet, and names it |
| `tierNotRun` | this tier has not been dispatched |
| `noProvider` | the live tier ran and self-skipped for want of a credential |
| `belowFloor` | the sample count was under the family's declared floor |
| `ceilingReached` | the spend ceiling stopped the run before this figure was complete |

`seamNotBuilt` is the one that matters most today, and section F says why.

### A delta is an improvement, a regression, or neither

`Delta` compares two figures and answers `Improved`, `Regressed`, `Unchanged`
or `Undecidable`. **`Undecidable` is the default**, returned whenever either
side is unmeasured, the units differ, or the two provenances are not
comparable (a live figure against a CI figure is not a comparison). The
direction of "better" is a property of the `Metric`, declared once in a table,
because a lower cost is an improvement and a higher pass rate is too.

### Nothing renders without its spread

`Figure.Render()` produces `median (p10-p90, N=k)` and has no mode that
produces the bare median. A number in the scorecard, on the generated page, in
the OS surface and in the CI comment all come through that one function.

---

## B. The two arms

An arm is the thing under test. There are exactly two and there will not be a
third, because the epic's comparison is "a model by itself versus the model
with MemQL" and a third arm turns a comparison into a survey.

| Arm | What runs | What is switched off |
|---|---|---|
| `platform` | the work spine: compile with catalog lookup, the automation executor, the journal at every step boundary, resume from the journal, the symptom classifier's rules table | nothing |
| `baseline` | the same model, the same tool set, the same scenario, in a plain bounded tool loop | catalog, journal, replay, classifier, healing, budgets-as-parking |

**The baseline is a real implementation, not a strawman.** It gets the same
tools, the same system prompt shape, the same retry allowance and the same
wall-clock ceiling. What it does not get is a memory of having done this
before -- which is the whole thesis, and stating it plainly is what makes the
comparison fair rather than flattering.

**In the CI tier both arms replay.** This is the fact that surprises people
and it follows from having no provider: a baseline that cannot call a model
cannot run, so the baseline's model responses come from a cassette exactly as
the platform's do. What CI therefore measures about the baseline is
structural -- how many model responses it consumed, how many steps it re-ran,
whether it duplicated a side effect -- and never its dollars.

---

## C. The scenario format

A scenario is DATA. The corpus is reviewable in a pull request without reading
Go, and a scenario with no Go behind it is the common case.

```
test/proving/scenarios/<id>.json
test/proving/cassettes/<id>.<arm>.<modelClass>.json
```

```json
{
  "id": "durability.resume-elsewhere",
  "family": "durability",
  "title": "A run killed mid-step resumes on another node with no duplicated effect",
  "goal": "Produce the weekly ledger reconciliation for {{account}}",
  "variables": [{"account": "acme"}, {"account": "globex"}],
  "steps": [ ... ],
  "world": { "machine": {...}, "mailbox": {...}, "http": {...} },
  "inject": [{"at": "step:post", "kind": "kill", "afterMs": 0}],
  "verify": [
    {"rows": "v1:work:step", "where": {"key": "post", "status": "done"}, "count": 1},
    {"effects": "mailbox.sent", "count": 1}
  ],
  "floors": {"reliability": 5}
}
```

Every enum in it is CLOSED and refused at load with the value and the legal
set named: `family`, the injection `kind`, the verifier form, the world's
facet names. An unknown key is refused too. The loader is pure and its errors
are the corpus's documentation.

**The verifier is declarative, with one escape hatch.** Two forms cover the
corpus: a **row assertion** (a concept, a field predicate, an expected count)
and an **effect assertion** (a facet of the fake world and a count). Where
neither fits, `{"check": "<name>"}` resolves against a **closed registry** of
named Go checks; an unknown name is refused at load, never at run. A named
check exists to express something data cannot, and the registry being closed
is what stops it becoming the place scenarios go to be untestable.

**The fake external world has three facets and no network.** `machine` (a
worker that accepts a script, returns a recorded stdout and records the hash
of what it was asked to run), `mailbox` (deliveries, with duplicate detection
that is the durability family's whole assertion), and `http` (a request/reply
table keyed by method and path). Every facet counts what it was asked to do,
because "zero duplicated side effects" is a claim about a counter somebody
kept.

The only real infrastructure is Postgres with TimescaleDB. That is deliberate
and it is the reason the `proving` job carries the pinned database service
rather than living as a step in `go-checks`: **a speed claim that excluded the
database would be measuring a different product.**

---

## D. The six families and what each tier can honestly say

This table is the P1 decision made concrete, and it is generated into the
scorecard rather than restated there.

| Family | CI tier (replay) | Live tier (real providers) |
|---|---|---|
| **Amortized cost** | provider calls per run (measured; zero on a served replay is the headline), steps served from the journal | tokens and dollars for one goal run N times with different variables |
| **Reliability** | pass/fail per scenario (measured); variance `notMeasurableOnReplay` | pass rate and variance over K runs |
| **Recovery** | recovery rate, model calls, steps re-run after each injected failure kind, repair-from-failed-step against re-run-from-start (all measured) | the same, plus wall-clock |
| **Durability** | duplicated side effects after a mid-run kill -- **measured, and fully honest in CI**; resume on another node | the same |
| **Learning curve** | fraction of steps served from the catalog across a related sequence (measured); cost per goal `notMeasurableOnReplay` | cost per goal falling as the catalog fills |
| **Speed** | the journal's own per-step overhead as a same-process ratio (measured); wall-clock per goal `notMeasurableOnReplay` | wall-clock per goal, first run against replay |

Two of the six are fully answerable in CI. That is not a weakness of the
design; it is the design refusing to dress four recordings as measurements.

**Governance is pass-or-fail, in both tiers**, and it is the part with no
statistics at all:

- every step whose footprint includes a side effect has a receipt;
- every model call the run made is journaled;
- every approval names an artifact hash, and a resume against a changed
  artifact refuses.

A governance property that fails blocks the merge (P2). There is no
"governance score".

---

## E. `memql-bench`

`cmd/memql-bench` is a Go binary that **adopts the capability-script
contract** rather than merely resembling it: `--flag=value` in, exactly one
JSON envelope on stdout, every human line on stderr, `--print-spec`, and the
contract's exit codes (0 ok, 2 bad parameter, 3 refused, 4 prerequisite
missing, 5 operation failed).

`scripts/lib/capability_contract_test.go` walks `scripts/**/*.sh` and will
never see a Go binary, so the contract would be a claim with nothing behind
it. Two things stand behind it instead:

1. `component/proving/capability` implements the envelope in Go, and its test
   **reads the `printf` format strings out of `scripts/lib/capability.sh` at
   test time** and asserts the Go emitter produces the same shape. A drift
   gate, in the repo's own idiom: when the shell library's envelope changes,
   the Go one goes red rather than silently diverging.
2. The `proving` CI job runs `memql-bench --print-spec` and pipes the run's
   envelope through `jq`, so a malformed envelope reds the lane.

Subcommands, each a capability id:

| Invocation | Capability id | What it does |
|---|---|---|
| `memql-bench run --tier=ci` | `bench.run` | runs the corpus in both arms from cassettes, writes rows, emits the envelope |
| `memql-bench run --tier=live --models=cheap,mid,strong --ceiling-usd=N` | `bench.run` | the same against real providers, refusing (exit 3) rather than exceeding the ceiling |
| `memql-bench gate --against=<scorecard.json>` | `bench.gate` | the P2 regression gate: structural properties block, cost and speed report |
| `memql-bench scorecard --out=<dir>` | `bench.scorecard` | writes the dated JSON and regenerates the page |
| `memql-bench record --scenario=<id>` | `bench.record` | captures a cassette against a real model |

`--ceiling-usd` is checked **before every model call, not after** -- the
process-wide guard in `component/memql/ai_guard.go` stays exactly where it is
and this is a second, per-run bound above it. A run that would cross the
ceiling stops with exit 3 and a `ceilingReached` figure, which is a published
result rather than a crash.

---

## F. What is measurable today, and what says so

The work spine landed A1, A2 and A3, and two seams the proving suite would
like are not built yet. **The suite reports them as `seamNotBuilt` naming the
missing code, and never as a zero.** This is the single most load-bearing
paragraph in this record: a benchmark that reports "zero provider calls"
because nothing in the path calls a provider has told a lie that reads exactly
like the headline result.

| Wanted | State | What the suite publishes |
|---|---|---|
| A replayed run serves every model call from the journal | `component/work.DecideServe` exists and is pure; **its caller does not** (`integrations/work/fork.go` says so in its header) | `seamNotBuilt: DecideServe has no call site; a replay run records intent only` |
| `v1:work:modelCall` rows per request | the concept, shape, query and `@serverOnly` mutation all exist; **no Go writer does** | `seamNotBuilt: nothing writes v1:work:modelCall` |
| A resumed run re-executes zero completed steps | **built and working** (`component/automations/resume.go` rehydrates completed steps) | measured, with a counting step registry |
| An exact catalog hit makes zero model calls | **built** (`work.Decide` returns `NeedsModel:false`; `CompileOutcome.ModelCalls` counts) | measured, with `countingCompileEngine`'s negative control |
| Zero duplicated side effects across a kill and resume | **built** (journal + idempotency key) | measured, against the fake world's counters |

**Every counter ships with a negative control.** `integrations/planner`'s
compile tests already establish the rule and the reason -- *"a counter that is
never incremented on ANY path reads as zero forever"* -- so each family's
zero-claim scenario is paired with a scenario that must produce a non-zero,
and the gate fails if the control ever reads zero. A green suite with a dead
counter is the failure mode this whole epic exists to avoid.

---

## G. The rows

`dsl/bench/`, two concepts, both `@rowAuthz(clusterOwner)`, both broadcast.

- **`v1:bench:run`** -- one row per `memql-bench run`: `tier`, `commit`,
  `corpusFingerprint`, `scenarioCount`, `startedAt`, `finishedAt`, `verdict`,
  `modelIds`, `costUSD`, `runner`, `blockingFailures`.
- **`v1:bench:sample`** -- one row per (scenario, arm, metric): `benchRunId`,
  `family`, `scenarioId`, `arm`, `metric`, `unit`, `n`, `median`, `p10`,
  `p90`, `min`, `max`, `mad`, `absentReason`, plus the provenance fields.

Both are cluster-owner tier because a benchmark is a fact about the
deployment, not about a person, and neither has an `ownerUserId` to be owned
by. Every mutation is `@serverOnly`: a client-reachable write to a bench row
is a primitive for forging the numbers the README rests on, which is the exact
thing P4's gate is for. `component/proving` stamps internal origin and takes
an entry in `call_origin_conformance_test.go`'s allowlist with that reason.

They broadcast so the OS charts the trend without polling. `modelCall`-style
volume is not a concern here: a bench run writes tens of rows, not thousands.

---

## H. The scorecard and the claims gate

`docs/public/overview/proving/scorecard/<YYYY-MM-DD>.json` is the committed
artifact and `docs/public/overview/proving-scorecard.md` is generated beside
the existing proving log. The page carries front matter and relative links
because it lives under `docs/public/`, and both gates apply to it.

The scorecard's JSON is the SOURCE and the page is DERIVED; `memql-bench
scorecard --check` fails when the page is stale, in the repo's usual
generated-artifact shape.

**The claims gate (P4).** A published claim that rests on a number is written
with a marker:

```markdown
Replaying a run re-executes no completed step.
<!-- proving: durability.resumed-steps-reexecuted = 0 -->
```

`TestPublishedClaimsRestOnAScorecardNumber` walks `README.md` and
`docs/public/**`, finds every marker, and fails when the metric is absent from
the newest scorecard, when the scorecard says `unmeasured`, or when the value
in the prose disagrees with the scorecard's median. Unmarked prose is
untouched, so the gate has no false positives and the marker is the opt-in.

The mirror half matters as much: `why-memql-harness.md`'s "Claims this page
does NOT make yet" table stays, and the gate ALSO fails when a claim in that
table has become measurable -- a claim that is proven and still listed as
unproven is a different kind of stale, and the whole point of the table is
that it is true.

---

## I. The OS surface

A **Benchmarks** section in Settings, beside Diagnostics rather than inside
it. Diagnostics is three panels about this session; benchmarks are a fact
about the deployment across releases, and folding them in would make the
"copy diagnostics" button mean something different.

`{ min: "admin" }`, matching Cluster and Logs: the rows are cluster-owner tier
and a reader would see an empty section with no explanation.

The picture is plain DOM in the OS's own idiom -- `<ol>`/`<li>`/`<span>` with
flex and token colours, no charting dependency, following `TrafficStrip` and
`SendBar`. One series, one hue, the accent; a non-zero value never draws
nothing; `role="img"` with an `aria-label`, the figures in a `Facts` grid
beside it, and a prose summary in `.os-sr-only`. **`--os-warn` and
`--os-error` are never a data series** -- amber is warn and red is error
everywhere in the shell, and a regression is expressed by the delta's own
words rather than by painting the bar red.

Three rules the surface inherits and must not break:

- **`unmeasured` renders as an em dash with its reason on the row**, never as
  a zero-height bar. A bar of height zero is a measurement of zero.
- **The arrival cue never fires on a benchmark row.** `ranAt`, `costUSD` and
  the sample counters are exactly the heartbeat trap the OS README names; the
  fingerprint is the run's `verdict` and `commit`.
- Every figure prints the commit and date it came from, because a scorecard
  read six weeks after its last run is the normal case under P3.

---

## J. Testing

- **Pure, no engine, no database.** The statistic refusing `N == 0`; a figure
  with neither a stat nor a reason failing to render; `Undecidable` for every
  incomparable delta; the scenario loader's closed sets; the named-check
  registry refusing an unknown name at LOAD; the capability envelope matching
  the shell library's format strings; the scorecard's page being a pure
  function of its JSON.
- **The import gate.** `figure`, `scenario`, `scorecard` and `capability` are
  separate PACKAGES so that "these cannot reach the engine" is a build-graph
  fact rather than a promise. `TestProvingPureSubpackagesImportNothingBeyondStdlib`
  reads `go list -deps` and fails on any import outside the standard library
  and each other. This is what a nested Go module would have bought, at none
  of its twelve gates' cost.
- **`component/proving`'s own tests are database-free**, deliberately: the
  package is therefore NOT db-gated and neither
  `scripts/ci/db-gated-packages.sh` nor the `db-tests` lane changes. The
  database-touching verification is the `proving` job running the binary
  end to end, which is a better gate for an end-to-end tool than a unit test
  with a mock.
- **The negative controls.** Every zero-claim scenario is paired with one that
  must be non-zero, and the gate fails if a control reads zero.
- **OS.** The Benchmarks section's tests run from inside `clients/os/`: the
  em-dash-not-zero rule in both directions, the fingerprint excluding the
  timestamps, and the section resolving against the real role ladder.

---

## K. Delivery

ONE PR, closing #4993, #4994, #4995 and #4996. The epic body says two,
sequential, gated on A2 and A3 landing; both have landed, so the sequencing
that made it two is spent and the owner asked for one (2026-09-06).

---

## L. Out of scope and follow-ups

- **The two unbuilt seams in section F.** Wiring `DecideServe` to the
  executor's model-call site and writing `v1:work:modelCall` rows is the
  remaining half of epic A2, not this epic. The suite is built so that
  building them turns a `seamNotBuilt` figure into a measured one with no
  change to the corpus.
- **Arming the live tier** (P3). One line in `proving-live.yml` and a
  provider secret.
- **A public comparison against any named framework.** Explicitly rejected in
  the epic body and not revisited.
- **Per-scenario cost thresholds** (P2). Reconsider when the live tier has a
  few weeks of history to set them from; a threshold invented today would be
  invented from nothing.

---

## M. References

- [The work spine](2026-09-05-work-spine-design.md) -- the rows this measures.
- [`docs/public/overview/why-memql-harness.md`](../../public/overview/why-memql-harness.md)
  -- the page the claims gate governs.
- [`docs/public/overview/proving.md`](../../public/overview/proving.md) -- the
  existing proving log; the scorecard is generated beside it, not into it.
- [`docs/internal/design/capability-script-contract.md`](../../internal/design/capability-script-contract.md)
  -- the envelope `memql-bench` adopts.
- [`docs/public/ai/llm-cost-control.md`](../../public/ai/llm-cost-control.md)
  -- the guards the live tier's ceiling sits above, not instead of.
