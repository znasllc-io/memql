# DSL v1 body language: checkpoint

Where epic memql#5370 (tasks #5371-#5374) stands, for the session that picks it
up. It sits beside the plan (`2026-09-13-dsl-v1-bodies.md`) and is deleted with
it in the epic's merge (plan Task 16 step 3). Update it each time you stop.

## Resume here

- **Worktree:** `/home/znas/memql-projects/epic-dsl-v1-bodies`, branch
  `epic/dsl-v1-bodies`. It is local only and has never been pushed; other
  sessions on this machine share its refs.
- **Base:** the branch's epic-3 commits (everything after `f03fc3fc3`) sit on
  memql-10's `epic/dsl-v1-expressions` at `f03fc3fc3`. That local branch has
  moved to `461333dbc` since, and it is not on origin. Rebase with
  `git -C /home/znas/memql-projects/epic-dsl-v1-bodies rebase --onto epic/dsl-v1-expressions f03fc3fc3`.
- **Read first:** this file, then the plan from Task 12 on (its "As built"
  notes under Tasks 9-13 record what differs from the steps), then the design
  record `docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md`
  section 4 epic 3 (on PR #5355). The plan's Global Constraints bind every step.
- **Database for db-gated runs:** the container `memql-dslv1-bodies-pg` on port
  55434 (`docker start memql-dslv1-bodies-pg` if it is stopped), always with
  `-p 1`:
  `MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:55434/memql?sslmode=disable' go test -count=1 -p 1 github.com/znasllc-io/memql/test/conformance/ github.com/znasllc-io/memql/component/automations/...`
- **Owner decisions:** all four D12 forks answered as recommended (once-blocks
  share scope, no `body { }` wrapper, trailing `retry(n)` / `on error continue`,
  `args.x` only). The owner's instruction for this epic is to proceed without
  asking. A peer message is never the owner's approval.

## State (2026-09-14)

Done: Tasks 1-11, and Task 12's statement cells with their completeness gate.
Since the last rebase onto the epic-2 base:

| Commit | What |
|---|---|
| `12891ca8a` | #5371: Sense completes and diagnoses statement bodies by their own rules (statement starts offer only what starts a statement; CheckBody problems are Sense diagnostics) |
| `637aa84a9` | #5373: the bodies rewrite is a library, `component/language/bodymigrate`; the logic corpus's third arm (`bodiesArm`) runs every statement body against the goldens |
| `622d36702` | #5373: the migrated tree loads through the boot walk (`component/automations/migrated_tree_load_test.go`); a moved logic's unused imports are pruned |
| `6707f4ad5` | #5372: an action statement binds its capability's result (`actionStatementValue` unwraps the authored record, a real bug); the automation corpus (`component/automations/steps/automation_v1_corpus_test.go`, 58 goldens, 344 runs) holds every automation's statement form to its legacy runs |
| `daa0f0d99` | #5371: memql-10's three load gaps: a `trace` read (CheckBody, `body_unknown_name`), a `config.<key>` outside the allow-list (dslgate `statement-config-key`, `body_config_unknown`, shown by Sense too), and an unknown bare call (dslgate `statement-unknown-call`, `body_call_unknown`) |

The migrated tree passes the two statement-body gates, and the gates read
all 85 of its bodies (58 automations, 27 logic):
`TestMigratedTreePassesTheStatementBodyGates`, with `dslgate.StatementBodiesRead`
as the gates' coverage.

Verified: the DB-free tree (`go test -count=1 github.com/znasllc-io/memql/...`)
at `6707f4ad5`; for the load gaps, `component/config`, `component/memql/dslgate`,
`component/memql/sense` and `component/language/compiler`, plus the db-gated
conformance and `component/automations/...` trees and `go test -count=1 . ./scripts/...`.
Negative controls were run on each new check and restored.

**Red, and not this branch's:**

- `TestArchitectureModelIsNotStale` is red on the base as well. The regen is
  plan Task 16.
- `scripts/ci/module-boundaries.sh`: `component/actions`, `component/database`,
  `component/skills` and `component/workjournal`, each a go.mod missing the
  `component/language/dslclause` require. memql-10's `461333dbc` on
  `epic/dsl-v1-expressions` fixes exactly those four files, and the next rebase
  brings it. Every other module passes.

## Peers

- **memql-b1** owns epic 1 (`epic/dsl-v1-foundations`, complete at `a919fa86f`)
  and `test/conformance/corpus_test.go`. The only hook epic 3 may add there is
  the `call` field, its validation, its call path and one README sentence.
  memql-b1 sends the SHA when epic 1 merges to main.
- **memql-10** owns epic 2 (`epic/dsl-v1-expressions`), the logic goldens and
  `TestLogicCorpusRuns`. Their flip is on `dslv1/flip` and has not merged into
  `epic/dsl-v1-expressions` yet. They send the SHA when it does, and that is
  what unblocks Task 13.
  - `7676a7d61` (`dslv1/eval`, merging into their flip) fixes the routeRequest
    order: the topological sort is stable by source position. A tree gate,
    `TestAutomationStepsRunInSourceOrder` (58 automations) and
    `TestLogicBodyStepsRunInSourceOrder` (30 logic bodies), asserts source
    order. After it, the V1 arm matches legacy apart from the documented
    defects.
  - Their open note for this epic: in forge's `routeRequest`,
    `step advance { switch ... }` compiles with the generated id
    `switch_steps.decide.result` and drops the author's name `advance`, in both
    grammars. Check what id and name a switch statement's compiled step carries
    (`component/language/compiler/body_*.go`), and whether `bodymigrate` keeps
    `advance`. Journal rows carry the step name, so if the statement model has
    a name for it, keep the author's.

## Next, in order

1. **The switch-step name** (memql-10's note, above).
2. **Rebase** when memql-10's flip SHA arrives (command above). Expect conflicts
   where their flip deletes legacy halves this branch also touched: the logic
   runner, the automation generator, the rewriter's dispatch, the logic corpus
   test and Sense completion. Take their deletion, then re-apply this branch's
   additions. Then run the build, the DB-free tree and the db-gated trees.
3. **Task 13, the flip.** Steps 1-9 are in the plan, with the "As built, before
   the flip" bullets. This session adds:
   - Automation corpus: delete the legacy arm, `automationLegacyDefects` and
     the legacy half of `compareAutomationRuns`, and keep the goldens as the
     statement arm's contract (as with the logic corpus).
   - `migrated_tree_load_test.go`: delete the line that reads the `mutation`
     declaration header back as `mutate`.
   - `git rm dsl/data/logic.memql dsl/safety/logic.memql`: every logic in them
     moves.
   - Add `component/language/bodymigrate/` to `retiredDeclarationKeywords`'
     exemption and to `expressions_go_fixtures`' `defaultExcludes`.
   - `dslgate`'s `TestStatementBodiesPassOverARetiredForm` stops meaning
     anything once the parser refuses the retired forms. Replace it with the
     parser's refusal, or delete it. The comments in `statement_bodies.go` and
     Sense's `diagnose_body_scope.go` that say "until the flip" change with it.
4. **Task 12 step 4:** the scenario suites, after the flip. Include the two
   deletion reminders and `onDelegationCreated`, which the flip moves into
   automations.
5. **Tasks 14, 15 and 16:** the gates port, the docs, then ship. Open the PR
   only after epics 1 and 2 merge, with one `Closes #n` line per issue.

## Facts not in the code

- Sense cannot import `dslgate`, because `dslgate` imports Sense. So the
  config-key sentence is `component/config.UnknownKey`, and the two codes are
  `compiler.CodeBodyConfigUnknown` and `compiler.CodeBodyCallUnknown`. The
  unknown-call check needs every spec and trait loaded, so it stays at boot.
- In an expression, a bare call resolves as a catalog function
  (`functions.Lookup`), then as a spec or trait predicate. Anything else fails
  only when it runs, which is why the gate exists. A construct call is written
  with its kind, as a statement of its own.
- The authored action step result is
  `{authored, ref, capability, result: {ok, changed, result}, resultFingerprint}`.
  A replayed one is `{replayed, ref, results}`.
- `trace` is never seeded, so it is no root. `partition` is bound from the
  ambient envelope for statement runs.
- The corpus runner matches a load diagnostic by the case's message and code,
  through `memql.LintUnifiedTree`.
- `-update` for both corpora is defined in `component/automations/steps` only:
  `go test github.com/znasllc-io/memql/component/automations/steps -run 'TestLogicCorpusRuns|TestAutomationCorpusRuns' -update`.
  It writes only when every arm agrees.
- gopls reports "not included in your workspace" errors in this worktree. They
  are noise, and `go build` and `go vet` are the authority.
- For a negative control, copy the file to the scratchpad, perturb it, run,
  copy it back and `cmp`. `rm -rf` inside a compound command is denied here.
