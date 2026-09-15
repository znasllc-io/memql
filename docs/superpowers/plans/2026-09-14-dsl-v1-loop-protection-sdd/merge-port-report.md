# Merge-port report: epic 3 (dsl-v1-bodies) into epic 5 (loop protection)

Worktree: `/home/znas/memql-projects/epic-dsl-v1-loop-protection`, branch
`epic/dsl-v1-loop-protection`. Merge commit `f79e20823`. This work:
`3da31799a` ("Issue #5380: port the epic 3 merge's tests and corpus off
retired step syntax").

## Item 1 -- journal_loop_db_test.go

`TestLoopDepth_DB_AResumedRunKeepsItsDepth` built an `Automation` with two
`StepTypeQuery` / `&QueryStepConfig{Query: "q"}` steps -- both types deleted
by epic 3. Replaced with `StepTypeFunction` / `&FunctionStepConfig{Name: "q",
Kind: "query"}`, the exact pattern every sibling `_db_test.go` in the package
already uses (`journal_db_test.go`, `cancel_test.go`, `resume_test.go`,
`fingerprint_test.go`, `adopt_test.go`, `owned_journal_test.go`).

Verified under real Postgres (see Verify below):
`TestLoopRefusal_DB_TheRefusedRunIsOneFailedRowNamingTheChain` (1.42s) and
`TestLoopDepth_DB_AResumedRunKeepsItsDepth` (1.54s) both ran (not skipped)
and passed.

## Item 2 -- loop_check_test.go

Three call sites of `l.loadFromTree(...)`; the method is now the exported
`LoadFromTree` with an identical signature (`unified_loader.go:100`). Renamed
all three call sites. No other change needed.

## Item 3 -- loop_graph_source_test.go

`treeAutomationSlices` (and its `treeSlice` type) do not exist anywhere in
the current tree. History check (`git log --all -S treeAutomationSlices`):
they were defined in `step_order_test.go`, a file the statements flip deleted
outright (commit `96cbd0360`, "Issue #5372: one execution model -- the
legacy body grammar, compiler and runtime deleted"). The three `mutate`
declarations the task brief mentioned (~lines 149/163/267) were already
`mutation` in the merged tree -- no action needed there.

`treeAutomation`'s own doc comment already said what it should do: "compiles
one automation of the embedded tree, as the tree loader compiles it, with
the origin the loader stamps." Rewrote it to do exactly that -- call
`NewLoader(LoaderOptions{}).LoadFromTree(memqldsl.Tree())` (the loader's own
real walk, the same one boot runs) and pick the automation whose `.Origin`
equals `"unified:" + path + ":" + name` (the same origin `LoadFromTree`
stamps at `unified_loader.go:200/265`). This is a closer match to the doc
comment than the old hand-rolled parallel walk was, at the cost of compiling
the whole embedded tree per call; `LoadAll()` / `LoadFromUnifiedTree()` doing
exactly that is already an idiom used ~20+ times elsewhere in this package,
and `TestLoopCheckMeasureTheTrees` (which now also depends on a full walk,
see item 4) measured well under a second per walk.

## Item 4 -- loop_graph_test.go, loop_prepare_test.go, loop_runtime_test.go, loop_tree_measure_test.go

The two named files' Go-fixture literals were mostly already converted (41
literals, per the brief), but `go vet` still found stragglers across the
whole package once the earlier items compiled. Fixed all of them (the
task's "grep the package's _test.go files ... and convert them" is what
pulled in the extra two files):

- **loop_prepare_test.go**: two `@schedule(cron="0 0 * * * *")` fragments ->
  `@trigger(schedule="0 0 * * * *")` (parser refusal code
  `trigger_schedule_synonym_retired` names this exact fix). One
  `Steps: []*Step{{..., Type: StepTypeQuery, Query: &QueryStepConfig{Query:
  "1"}}}` Go fixture -> `StepTypeFunction` / `FunctionStepConfig{Name: "q",
  Kind: "query"}` (the step is never dispatched in that test --
  `TestLoopAndModeOnAnAutomationBuiltInGo` only calls `ensurePrepared` --
  so the placeholder name doesn't matter; used the package's standard "q").
- **loop_graph_test.go**: three leftover `step x { mutation y(...) }` blocks
  with bare `id`/`x` arg reads (`undecidedPair`'s helper, and the `reader`
  closures in `TestLoopGraph_UndeclaredArgsReadAbsent` and
  `TestLoopGraph_Strata`) -> `x := mutation y(id: args.id, ...)` statements;
  the one caller passing a bare `x` arg reference now passes `"args.x"`.
  Confirmed the assertion text in `TestLoopGraph_Undecided` doesn't depend on
  which spelling the caller uses (it asserts on `advanceThing`'s own
  `args.s`, not the caller's expression) -- unaffected.
- **loop_runtime_test.go**: `causeProbeAutomation`'s one `StepTypeQuery` step
  -> `StepTypeFunction` / `Kind: "query"`, same pattern as item 1.
- **loop_tree_measure_test.go**: `l.loadFromTree` -> `LoadFromTree` (same as
  item 2), plus `bundleTrees` (and the `mountedFS` type / `mountedAs`
  helper it needs) were undefined -- also step_order_test.go casualties, but
  they are plain filesystem-mounting helpers (walk `examples/**/dsl`, mount
  each pack's tree as a single-domain `fs.FS`) with **no** dependency on the
  retired step grammar, and this file is their only remaining caller in the
  package. Ported the three verbatim from the pre-flip
  `step_order_test.go` (commit `1f16dd509`) into this file, with a comment
  explaining the provenance. Confirmed by running `TestLoopCheckMeasureTheTrees`
  (7.27s, PASS): it walks the embedded tree plus `deploy/fleet/dsl` and every
  `examples/**/dsl` bundle exactly as intended.

**The computed-topic case (`TestLoopGraph_TopicEdges`'s automation `r`).**
Per the controller's ruling: the graph code still reads a computed topic
(`loop_graph.go`'s `eventPublishOf`, via `Step.Exprs.Topic` when it is not a
`*ast.LiteralExpr`) -- I did not touch that code. `r`'s retired body was
`step s { publishEvent(topic: "x." + kind, payload: {a: 1}) }`, and epic 3's
`publish` statement requires a *literal* topic, so that shape has no
statement-form spelling. Built `r` directly as a `*Automation` in Go
(`rComputedTopicAutomation`, same file): its one `Step` is unmarshalled from
a small JSON literal whose `event.topic` is `{"$expr": "\"x.\" + args.kind"}`
-- the compiled wire encoding for an expression-typed value leaf
(`value_leaves.go`) -- then `PrepareExpressions` is called on it directly
(the same call `compileMemQLFrom` makes for every DSL-compiled automation),
so `Step.Exprs.Topic` ends up a real (non-literal) parsed expression node,
exactly as a DSL-compiled automation's would. All of the test's original
assertions are unchanged: the edge to every raw-topic automation stays
undecided ("known only at run time"), `r`'s filtered target `v` still sees
no edge, and `r.Publishes` still reads `[topicKnownAtRunTime]`. Nothing was
guessed or reinterpreted -- the topic still reads as unresolved/opaque, never
a guessed value.

**TestLoopGraph_StepsBuiltInGo -- deleted, not ported.** This test's whole
premise was two step *types* the compiler never emits but a Go-built
automation supposedly still could: `StepTypeQuery` holding a raw expression
that happens to be a call, and `StepTypeMutation` holding an inline
insert-payload with no named construct. Both types are gone outright
(confirmed: no `Step.Mutation` field, no `MutationStepConfig` type, anywhere
in the current tree; the flip commit's own message says "query, mutation,
shape, webhook, switch, detectLeadSignal and emitConceptCard steps and their
executors are deleted"). There is no successor encoding for the inline-insert
case (a `StepTypeFunction` call always names a registered construct), and
recasting the query case as an ordinary `StepTypeFunction` call would just
duplicate coverage this file already has many times over elsewhere, silently
changing what the test claims to demonstrate. Rather than guess at a new
representation, I deleted the test and left a comment at its former location
naming the flip commit and the reasoning. This is the one place I made a
judgment call beyond a mechanical rename; flagging it explicitly per the
"if it needs a real design call, report it as open" instruction. It is not
one of the five named items -- it surfaced only because leaving it broken
would have kept the whole package from compiling.

## Item 5 -- conformance corpus cells

Ran `go run ./cmd/memqlmigrate --help` first (per instructions), then:

```
go run ./cmd/memqlmigrate --rewrite=bodies -w -v \
  test/conformance/2026/cells/automation/loop \
  test/conformance/2026/cells/automation/mode
```

It converted all 16 `.memql` files across both cells with no refusals
(`step x { mutation y(...) }` -> `x := mutation y(...)`, `mutate` ->
`mutation`, `partition="*"` dropped off `@trigger`). Reviewed every diff by
hand:

- `loop/permits-a-converging-self-cycle.memql`: still self-cycles
  (`advance := mutation advanceTicketLoopCell(id: args.id, status: "done")`
  on the same trigger concept) and still carries its `@loop(maxDepth=4,
  until=row => row.status == "done")`, unchanged by the rewrite.
- The six other `loop/` refusal cases and all seven `mode/` cases: the
  `@loop(...)` / `@mode(...)` annotation under test is untouched by the
  rewrite (it only touched the body and, for `on-a-scheduled-automation.memql`,
  the trigger spelling) -- codes and messages in `expect.json` needed no
  changes.
- **One hand fix beyond what the tool did**: `on-a-scheduled-automation.memql`
  used `@schedule(cron="0 0 * * * *")`, which the tool rewrote to
  `@trigger(schedule="0 0 * * * *")` as part of the `bodies` rewrite (its
  description explicitly lists `mutate -> mutation; partition= off @trigger`
  and the terse-header/`@schedule` retirement). This was necessary for the
  case's own verdict to stay correct: the expected verdict is
  `refuse_load` / `loop_not_event_triggered` (a LOAD-time refusal -- `@loop`
  requires an event trigger), which requires the file to *parse* successfully
  first. Left as `@schedule(cron=...)` it would now refuse at *parse* time
  with `trigger_schedule_synonym_retired` instead -- the wrong code, and
  the wrong verdict tier. Confirmed the tool already made this exact
  substitution, so no further hand edit was needed here.
- `namespace.pin` (`loop/`, pins the fixture concept's `@namespace("loopcell")`
  override) is untouched -- it's an orthogonal, unrelated mechanism (epic
  #2614) and the tool correctly left it alone.

No case needed hand-fixing beyond what the tool did.

## Verify

All commands run from the worktree root unless noted.

| Command | Result |
|---|---|
| `go build github.com/znasllc-io/memql/...` | clean |
| `go vet github.com/znasllc-io/memql/component/automations/...` | clean |
| `go test -count=1 github.com/znasllc-io/memql/component/automations/...` | `component/automations` package: **ok** (13.5-14s). `component/automations/steps`: **FAIL** -- `TestAutomationCorpusRuns` (see "Pre-existing failure" below; not touched by this change) |
| `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:15434/memql_loops?sslmode=disable' go test -count=1 -p 1 -v github.com/znasllc-io/memql/component/automations/...` | `component/automations`: **ok** (30.4s), every test including all `_DB_` tests PASS with real durations (not skipped) -- `TestJournal_DB_*`, `TestLoopRefusal_DB_TheRefusedRunIsOneFailedRowNamingTheChain` (1.42s), `TestLoopDepth_DB_AResumedRunKeepsItsDepth` (1.54s), `TestResumeV1_DB_*`, `TestLogicJournal_DB_*`, `TestCheckLoops_*`, `TestLoopGraph_*`, `TestLoopCheckMeasureTheTrees` (7.27s). `component/automations/steps`: same pre-existing `TestAutomationCorpusRuns` failure, confirmed unrelated |
| `go test -count=1 github.com/znasllc-io/memql/test/conformance/...` | **ok** (33.96s) -- includes both migrated corpus cells |
| `go test -count=1 github.com/znasllc-io/memql/component/memql/... github.com/znasllc-io/memql/component/language/... github.com/znasllc-io/memql/app/...` | all **ok** (component/memql 163s, its 6 subpackages, component/language's 9 subpackages, app 2.1s) |
| `go test -count=1 .` | **ok** (38.9s) |
| `go test -count=1 ./scripts/...` | all **ok** / no test files, nothing skipped |
| `go run ./cmd/memqllint dsl/` | `OK: 306 file(s) loaded, no diagnostics.` (the "40 domains not mounted" NOTE is expected/standard for linting the engine's own embedded `dsl/`) |
| `gofmt -l <the 7 touched .go files>` | no output -- already formatted |

### Pre-existing failure outside the five items: `component/automations/steps` `TestAutomationCorpusRuns`

`forge/automations.memql routeRequest`: for 4 of its golden run keys the
fixture's computed input JSON now carries `"firstVersion":true`, which the
recorded golden lacks ("the fixture is not the golden's input"). This is a
golden/fixture drift over `routeRequest`'s "fires on a request's first
version only" behavior (commit `aac4c7066`, an ancestor of the merge base,
predating epic 3's own work). None of this commit's files touch
`component/automations/steps/`, `dsl/forge/`, or that test's golden data
(`git diff --stat` for this commit shows only the 7 `component/automations`
Go test files and the 16 corpus `.memql` files) -- confirmed present before
and after this change. Per instructions this was left unfixed and is
reported here rather than folded into scope; it likely needs either a
`-update` golden regeneration or a look at whether `firstVersion` binding
changed shape across the merge, and belongs to whoever owns
`component/automations/steps`.

**Note found at final `git status`, unrelated to this commit:**
`component/automations/steps/testdata/automation_corpus/forge/routeRequest.json`
shows as modified in the working tree, unstaged -- it now carries
`"firstVersion": true` on the same 4 golden entries `TestAutomationCorpusRuns`
flags above, i.e. exactly the `-update` fix the test's own error message
suggests. This was not produced by any command in this report (I never
passed `-update`) and is left untouched and unstaged; it is very likely a
concurrent session's `-update` run against this same shared worktree (the
root CLAUDE.md's multi-session warning), and corroborates the diagnosis
above rather than changing it. Not staged, not committed, not mine to
resolve.

## Files touched (all under this worktree)

- `component/automations/journal_loop_db_test.go`
- `component/automations/loop_check_test.go`
- `component/automations/loop_graph_source_test.go`
- `component/automations/loop_graph_test.go`
- `component/automations/loop_prepare_test.go`
- `component/automations/loop_runtime_test.go`
- `component/automations/loop_tree_measure_test.go`
- `test/conformance/2026/cells/automation/loop/*.memql` (7 case files + fixture.memql)
- `test/conformance/2026/cells/automation/mode/*.memql` (7 case files + fixture.memql)

Commit: `3da31799a` "Issue #5380: port the epic 3 merge's tests and corpus off
retired step syntax", on top of merge commit `f79e20823`. Not pushed
(controller pushes).

**Addendum:** while writing this report, HEAD on this same worktree advanced
past `3da31799a` through what is clearly other, concurrent activity on this
shared branch: `10444e47b` ("the automation corpus golden for routeRequest
carries firstVersion") -- a fix for exactly the pre-existing
`TestAutomationCorpusRuns` failure reported above, landed directly on top of
this commit -- followed by a merge of `origin/main` (`aa8512f50`, docs-only:
epic 3's plan/checkpoint files deleted now that PRs #5441/#5449 are closed).
Confirmed `3da31799a` is still an ancestor of the current HEAD
(`git merge-base --is-ancestor`), the working tree is clean, and a fresh
`go build` + `go test ./component/automations/` against the now-current HEAD
still pass -- none of this touched the files in this commit.
