# DSL v1 body language: checkpoint

Where epic memql#5370 (tasks #5371-#5374) stands, for the session that picks it
up. It sits beside the plan (`2026-09-13-dsl-v1-bodies.md`) and is deleted with
it in the epic's merge (plan Task 16 step 3). Update it each time you stop.

## Resume here (latest, 2026-09-14 evening) -- read this section first

The sections below it are the earlier history and are superseded where they
disagree with this one.

- **Where it landed:** at the owner's instruction (2026-09-14) the work so
  far merges to main NOW, through a PR from `epic/dsl-v1-bodies` that REFERS
  to #5370-#5374 without closing them; each issue carries a comment naming the
  PR, its merge commit and what is left. So this plan and checkpoint are on
  main until the epic finishes. Continue on a NEW branch off `origin/main`
  (for example `epic/dsl-v1-bodies-finish`), do the "To finish" list below,
  delete this plan and checkpoint in that PR, and close the five issues when
  it merges.
- **Done:** flip steps F1-F7, all of them, plus:
  - the step bodies' accessors `step("x")`, `input()`, `item()`, `index()`
    are refused by name (`body_accessor_retired`, parser + corpus + docs);
    GrammarVersion is `2026.09-dsl-v1-bodies-8fef9e94`;
  - F6: `mutation` is the declaration keyword, `mutate` refused
    (`construct_unknown` -- a deviation from the plan's
    `mutate_keyword_retired`, worth one line in the PR body);
  - F7: `editors/vscode/package.json` pins the new GrammarVersion, the 0.4.0
    CHANGELOG names it and has a "Writing a logic or an automation" section,
    the TextMate grammar is regenerated (EditorRelease stays 0.4.0: unpublished);
  - the DSL comment sweep merged; stale citations fixed; the last
    `// memqlmigrate:` marker in dsl/ replaced; attribute matrix, the
    where-each-expression-runs table and the architecture model regenerated;
    `sdk-gen --check` reports no drift.
- **Verified green (DB-free) at this point:** `go test -count=1 .` (root
  gates); `component/language/...`; `component/memql/sense/...`;
  `test/conformance/...` (DB-free cases); `cmd/memql-lsp/...`; the extension's
  `npx tsc --noEmit -p .`, `-p tsconfig.host.json`, `npm test` (2403 pass) and
  the real-editor host lane (20 pass, 12 skipped by design; needs
  `go build -o editors/vscode/bin/linux-x64/memql-lsp ./cmd/memql-lsp` first).
- **NOT yet run on this tip:** the full DB-free suite
  (`MEMQL_DATABASE_DSN='postgres://unused:unused@127.0.0.1:1/unused?sslmode=disable' go test github.com/znasllc-io/memql/...`);
  the db-gated trees (`scripts/ci/db-gated-packages.sh --trees`, plus
  `./test/conformance/...`) against `memql-dslv1-bodies-pg` on 55434 with
  `-p 1` and `MEMQL_REQUIRE_DB=1`; the seven node-tag builds and vets;
  `go run ./cmd/memqllint dsl/`; `go test -count=1 ./scripts/...`;
  `bash scripts/dev/proto-gen.sh --only=component/grpc --check`;
  `make sdk-ts-typecheck`; `make frontdoor-paths-check`;
  `scripts/ci/module-boundaries.sh`; a dead-code sweep.
- **Was being considered when stopped (optional, nothing edited):** retiring
  `error()` with NO arguments the same way (it read the current error inside
  the old onError handlers, which no longer exist; `error("msg")` is the live
  catalog function and stays). Its legacy node is `ErrorRefExpr` /
  `ErrorRefExpression` (parser.go parseErrorAccessor, compiler
  automation_generator.go, memql ast_converter.go / executor.go /
  expression_helpers.go, tests in parser_test.go, compiler_test.go,
  literal_value_node_test.go). Found on the way: a probe logic calling
  `error("x is required")` trips the cross-namespace-import gate asking for
  `use common.builtins.{ error }`, because `dsl/common/builtins.memql`
  declares a builtin named `error` beside the catalog function -- file it.
- **To finish:** (1) the verification above, reading every output, on a
  branch off main -- CI's `ci-required` covers most of it, but not
  memqllint's whole-tree run or a dead-code sweep; (2) decide `error()`
  (above); (3) delete this plan and checkpoint (plan Task 16 step 3); (4) open
  the finishing PR with one `Closes #n` line per issue #5370-#5374, wait for
  `ci-required`, `scripts/dev/merge-as-owner.sh --pr=<n> --check`, merge,
  verify main, and close any issue the merge did not; (5) message peer
  `memql-6f` the merge SHA of whichever PR it is waiting on; (6) delete the
  local branches `tmp/dsl-v1-bodies-*` and the scratch worktrees
  (`git worktree prune`); (7) file the follow-up issues below.
- **Follow-up issues to file after the merge:** healing patches still write
  `$config.X` / `$event.payload.X` / `$steps.a.result` (component/healing
  patch.go, repair_loop.go), which the statement runtime does not resolve -- a
  real defect; the dry-run sandbox runs `builtin` calls for real; a parallel's
  branch names are unreadable after it, so the work draft's sections run in
  sequence (latency) -- the owner may want `wait all` branches to bind; the
  compiler's legacy serializer `isRuntimeReference` still honours `$steps.` /
  `$item.` / `$input.` prefixes; the executor's `error()` message cites
  "automation onError handlers"; automations are not held to @actor binding or
  memql#3626 undeclared args at load; `@trigger(on=...)`; stale Go comments
  citing `harness_step_validation.go` / `harness_consolidation.go`
  (memory_consolidation.go, forge_request_validation.go, predating this epic).

## Resume here

- **Worktree:** `/home/znas/memql-projects/epic-dsl-v1-bodies`, branch
  `epic/dsl-v1-bodies`. It is local only and has never been pushed; other
  sessions on this machine share its refs.
- **The flip (plan Task 13) is being built on a throwaway branch,**
  `tmp/dsl-v1-bodies-flip2` (local, shared refs), in the worktree
  `/tmp/claude-1000/-home-znas-memql-projects-memql/c91c208e-2a3e-4341-a81b-e3333bcfd158/scratchpad/wt-flip`.
  The branch is the record; the worktree is a scratch directory and may be
  gone. If it is: `git -C /home/znas/memql-projects/epic-dsl-v1-bodies worktree prune`,
  then `git -C /home/znas/memql-projects/epic-dsl-v1-bodies worktree add <dir> tmp/dsl-v1-bodies-flip2`.
  Its steps and their state are under "The flip" below. When it is done, land
  it on `epic/dsl-v1-bodies` (a fast-forward if this branch has not moved, a
  merge if only this checkpoint has) and delete the branch.
- **Base:** epic 2's flip is MERGED in: `b7a1afa30` merges memql-10's
  `epic/dsl-v1-expressions` at `c6c52144c` (the flip `f507402bb` plus its plan's
  deletion). The branches are local and not on origin. Take later epic-2 work
  the same way: `git -C /home/znas/memql-projects/epic-dsl-v1-bodies merge epic/dsl-v1-expressions`.
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

Done: Tasks 1-11; Task 12's statement cells with their completeness gate, and
all four scenario suites the record names; Task 15's docs; epic 2's flip
merged. Commits since the base:

| Commit | What |
|---|---|
| `12891ca8a` | #5371: Sense completes and diagnoses statement bodies by their own rules (statement starts offer only what starts a statement; CheckBody problems are Sense diagnostics) |
| `637aa84a9` | #5373: the bodies rewrite is a library, `component/language/bodymigrate`; the logic corpus's third arm (`bodiesArm`) runs every statement body against the goldens |
| `622d36702` | #5373: the migrated tree loads through the boot walk (`component/automations/migrated_tree_load_test.go`); a moved logic's unused imports are pruned |
| `6707f4ad5` | #5372: an action statement binds its capability's result (`actionStatementValue` unwraps the authored record, a real bug); the automation corpus (`component/automations/steps/automation_v1_corpus_test.go`, 58 goldens, 344 runs) holds every automation's statement form to its legacy runs |
| `daa0f0d99` | #5371: memql-10's three load gaps: a `trace` read (CheckBody, `body_unknown_name`), a `config.<key>` outside the allow-list (dslgate `statement-config-key`, `body_config_unknown`, shown by Sense too), and an unknown bare call (dslgate `statement-unknown-call`, `body_call_unknown`) |
| `2596afda7` | #5371: the two statement-body gates report their coverage (`dslgate.StatementBodiesRead`), and the migrated tree passes them |
| `6daca7fcc` | #5374: the scenario suites (`test/conformance/scenarios_db_test.go`, `test/conformance/2026/scenarios/<suite>/scenario.json`): decide-and-apply, forge, deployment; 8 scenarios, each live plus dryRun and resume variants over a real database |
| `c6339c968` | #5374: the campaigns suite, a generated event-email automation run under its author, with its redelivery claimed |
| `45340627a` | #5371: the body language documented: memql.md's Bodies section, Logic and Automations rewritten; authoring rules 1, 11b, 11c, 13, 14, 15, 17, 18, 21, 21c; the root and component/language CLAUDE.md |
| `5cdc8ad02` | #5371: G5's refusal says `args.<field>` and names `memqlmigrate --rewrite=bodies` |
| `d48e527bc` | #3803 (found on the way): the cross-namespace import gate's hint names the declaring file, where it spelled `querys` / `logics` |
| `b7a1afa30` | the merge of epic 2's flip: nine conflicts resolved (the commit message lists each), and this branch's uses of the deleted switch fixed |
| `f0565eecc` | #5373: the rewrite's model of today's order follows epic 2's stable sort; the goldens keep every body as written (cases renamed workbench-source-order, deploypack-undotted-reads) |
| `9df496005` | #5374: the automation corpus declares no legacy defect (epic 2 fixed all of them); goldens regenerated |
| `513c9603a` | #5374: the scenarios carry no legacy count (epic 2 fixed the forge no-op) |

The migrated tree passes the two statement-body gates, and the gates read
all 85 of its bodies (58 automations, 27 logic):
`TestMigratedTreePassesTheStatementBodyGates`, with `dslgate.StatementBodiesRead`
as the gates' coverage.

Verified: the DB-free tree (`go test -count=1 github.com/znasllc-io/memql/...`)
at `d48e527bc`, 200 packages green with `TestArchitectureModelIsNotStale` the
only failure (below); for the load gaps, `component/config`, `component/memql/dslgate`,
`component/memql/sense` and `component/language/compiler`, plus the db-gated
conformance and `component/automations/...` trees and `go test -count=1 . ./scripts/...`.
Negative controls were run on each new check and restored.

**Red, and not this branch's:**

- `TestArchitectureModelIsNotStale` is red on the base as well. The regen is
  plan Task 16.
- `bodymigrate` `TestLegacyOrderMatchesTheCompiler`, on the fleet bundle only:
  epic 2's compiler refuses those automations (step 1 below).

## The flip (plan Task 13), step by step

The plan's Task 13 is one commit; it is built here as seven, F1-F7, on
`tmp/dsl-v1-bodies-flip2`. `9da8ce6c3` merges epic 2's fleet fix
(`epic/dsl-v1-expressions` at `7a6b89767`: a switch step's id is its author's
name, duplicate ids refused, position-based sort) onto `2caaa3953`.

- **F1, `d9238ddce`: the tree is written in statements.** The rewrite over
  `dsl/`, `examples/`, `deploy/fleet/dsl`, `mutation` headers put back to
  `mutate` until F6; the seven publishing logic moved and deleted with
  `dsl/data/logic.memql`, `dsl/safety/logic.memql`; the two deletion reminders
  decide through the new pure `usersDueDeletionReminder(from, to)` (callgraph
  P4 forbids date math in an automation condition); the legacy-order tests
  deleted; the tests of the retired shapes ported; both corpora on one arm
  (goldens: the seven moved logic's deleted, the new logic's written, seven
  automation goldens gain the `output` the moved-logic rule left out and
  nothing else changes).
- **F1's fallout, `b4a13d4b0`:** deploy/fleet's two sweep gates walk the
  parsed statement body (every `for` in a scheduled sweep carries an `if`
  filter; `requestInstanceTeardown` has one caller, now checked across every
  fleet file); `embed_inventory_test.go`'s dsl count 424 -> 422.
- **F2, `a96483e12`: the corpus.**
  `test/conformance/2026` through the rewrite (62 files, `mutation` headers
  put back), three refused cells by hand (`keyless-map-entry` returns its map,
  a logic may not publish; the two object-literal cells keep their refused
  argument). Five verdicts moved, each answered:
  - `cells/logic/actor/reads-the-actor-undeclared` and
    `cells/logic/eventField/reads-an-undeclared-field` LOADED: a real gap.
    The loader's text validators find a body with `extractFunctionBody`, which
    knew only the rewriter's `func (Receiver)` header; a statement-body logic
    is left as written, so the `@actor` (#2621), undeclared-`args.x`
    (memql#3626), event-binding (memql#1706) and `@eventField` (memql#1743)
    checks all read "no body" and skipped. Fixed: `extractStatementLogicBody`
    (the statements after the args block), pinned by
    `component/memql/logic_statement_validators_test.go` with a negative
    control. The event-binding check reads only `args.event` in a statement
    body, leaving a bare `event` to CheckBody's `body_unknown_name`.
  - `expr/automationCondition/event-payload-read`: the rewrite migrated its
    defect away; written by hand with the dotted read, which G5 refuses.
  - `expr/automationCondition/no-condition`: `if {` read `{` as a map literal.
    New parser refusal `body_missing_expression` for an if/else-if condition,
    a for source or filter and a switch subject.
  - `cells/automation/schedule/*` stay in their legacy text: D15 retires
    `@schedule` on an automation in every form, so in F4 the registry's
    `schedule` placement on Automation goes and the family with it (the
    completeness gate says so), `an-invalid-cron` moving to the trigger cell.
- **F3, `3bf10ffdd` + `0a848a2de` + `c82004950`: the Go fixtures.**
  `3bf10ffdd`: the rewrite converts `@schedule("...")` as well as
  `@schedule(cron=)`. `0a848a2de`: three boot gates that went blind at F1
  read the parsed statement body -- dslgate unresolved-sub-automation and
  builtin-step-args (a named call, `x := automation y(...)`, opens no line
  with the keyword) and callgraph P4 (a loop's `if` filter, `} else if`, a
  one-line `if`). `c82004950`: the fixtures, as its message lists (kept with
  `memqlmigrate:keep`, reverted for deletion with their code, or ported --
  RunLogic -> RunLogicBody, the sweeps and the authored for/parallel through
  the executor). Verified: the DB-free tree at `c82004950`, 200 packages
  green, `TestArchitectureModelIsNotStale` the one failure (stale on the base
  too; Task 16 regenerates it).
  Measurements and method, for F4: `memqlmigrate --rewrite=bodies --go-fixtures` found 315
  literals to change in 112 files, 82 refused and 65 fragments (measured
  after F2). Run it with the keyword rename left out, since that is F6: build
  a scratch binary with `go build -overlay` replacing
  `component/language/bodymigrate/inline.go` by a copy whose
  `rewriteDeclarationsAndTriggers` skips the `mutate` loop (validated: on the
  corpus it reproduces the rewrite minus the renames exactly). Read every
  changed literal: fixtures that exist to be refused (the parser's
  `v1_body_refusals_test.go` cases, negative grammar) take a
  `memqlmigrate:keep` marker, and tests of the rewriter's logic/automation/
  terse stages, the legacy compiler and the legacy runtime are not migrated
  but deleted with their code in F4/F5.
  **What F3 left for F4:** 61 test files still hold the retired grammar -- 79
  literals the rewrite refuses (a callee declared nowhere, an object-literal
  argument), 63 fragments split across Go string literals, and the 16 files
  reverted on purpose. List them with the dry run of the scratch binary
  (`memqlmigrate-norename --rewrite=bodies --go-fixtures .` from the worktree
  root). Each is migrated by hand or deleted with the code it tests before
  the parser can refuse the retired forms.
- **Next: take main.** Epic 1 merged to main at `66d6ba208`, epic 2 (#5427)
  at `a0e44069a` (its second parent `5693fe164` holds the fleet fix this
  branch took plus epic 2's DB timing fixes, arch model and epic 1's tip).
  memql-10 deleted `epic/dsl-v1-expressions` and `dslv1/*`. Merge
  `origin/main` into `tmp/dsl-v1-bodies-flip2` (a merge, as the epic-2 merges
  were; the history already holds merges), resolve, rerun the DB-free tree.
- **The owner's instruction (2026-09-14):** land everything -- the PR merged
  to main -- and close the epic's GitHub issues, then report. That overrides
  "do not push" for this branch once the flip is complete and verified.
- **F4:** the parser refuses the retired forms (delete the transitional
  dispatch and the rewriter's logic/automation/terse stages; empty
  `statementRetiredAtTheFlip` and add the three retired cells; the `schedule`
  placement above; dslgate `TestStatementBodiesPassOverARetiredForm`).
- **F5:** the legacy compiler and runtime halves the plan lists.
- **F6:** `mutation` as the declaration keyword, `mutate_keyword_retired`,
  every `mutate` site and doc, `retiredDeclarationKeywords` inverted, the
  bodymigrate exemptions; rerun the standard rewrite (only renames remain).
- **F7:** `GrammarVersion`, grammar-surface corpus, `make vscode-grammar`, the
  extension pins and CHANGELOG (D25).

Verified at `2caaa3953` on the database: every db-gated tree, 51 packages
green; `component/node` `TestSelfStatusWriterProtectsSteadyHealthyNodeFromThirtyMinutePrune`
failed once on a database read timeout and passes alone.

**To raise with the owner, not this epic's to fix:** automations were never
held to the `@actor` binding rule or memql#3626's undeclared-`args.x` rule at
load, in either body form (probed with the boot walk over legacy and
statement bodies; the corpus pins `@actor` for query, mutation and logic
only). `validateArgsReferencesAreDeclared`'s comment says the rule covers
automations.

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
  - Their note on `step advance { switch ... }` in forge's `routeRequest` (the
    legacy compile gives it the generated id `switch_steps.decide.result`) is
    answered and moot. A switch binds nothing and flattens into one step per
    call, with an unnamed call's id being its callee: `decide`,
    `advanceRequest`, `advanceRequest#2`, `persistRouted`. The rewrite drops a
    switch step's label, as for `for` and `parallel` (`bodymigrate/read.go`).

## Next, in order

1. **Take memql-10's fleet fix** when its SHA arrives (#5427). Their flip's
   stable sort cannot compile any automation in
   deploy/fleet/dsl/fleet/automations.memql: two switch steps on
   `steps.command.result` get one generated id, the sort keys "emitted" by
   id, and the loop reports a cycle among no steps. They are making a switch
   step's id the author's step name, refusing duplicate ids, and making the
   sort index-based. Until it lands, `TestLegacyOrderMatchesTheCompiler`
   (bodymigrate) fails on the fleet automations and nothing else. Merge it
   as the flip was merged, then rerun the parity test.
2. **Task 13, the flip.** In progress: see "The flip" above, whose steps
   supersede this item's list. Unblocked, since epic 2's grammar is the only one.
   Steps 1-9 are in the plan, with its "As built, before the flip" bullets.
   `memqlmigrate --rewrite=bodies` alone now carries the tree (it is v1
   already), and it leaves 7 comments, the inlined publishing logic, and no
   order move. This session adds:
   - Automation corpus: delete the legacy arm, the now-empty
     `automationLegacyDefects` and the legacy half of `compareAutomationRuns`,
     and keep the goldens as the statement arm's contract.
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
   - `bodymigrate/order_legacy_parity_test.go` goes with the compiler's sort.
   - Docs owed at the flip:
     - every `mutate <Concept> <name> {` in markdown becomes `mutation` (D13);
     - `partition="*"` goes from authoring-rules rule 10's sentence, the
       memql.md example near the Mutations section, and the root CLAUDE.md
       Automations example with its #56 caveat, because the native parser
       refuses the kwarg (`trigger_partition_retired`);
     - component/language/CLAUDE.md's "Until the flip ..." sentence becomes
       the parser's refusal.
   - The scenarios then run over the statement bodies. They carry no legacy
     entries, so the same expectations hold.
3. **Task 12 step 4, the rest:** after the flip, scenarios for the two
   deletion reminders and `onDelegationCreated`, which the flip moves into
   automations. The marketing lanes of the campaigns engine (audience, row
   address) go through the campaigns worker, which the rig does not wire.
4. **Task 14, the gates port.** These read body text or the legacy step AST:
   - `test/dslconformance`: `bootstrap_forwards_every_field`,
     `agentauthz_stamped_userid`, `callgraph_contract`,
     `identity_login_form_field_contract`, `conformance`,
     `no_longhand_single_step` (it runs the terse-automation rewrite; delete
     it with that rewrite), `local_first_policies`, `server_only_parsed`,
     `prompt_levels`, `naming_conventions`;
   - `component/memql/dslgate`: `builtin_step_args.go`,
     `subautomation_calls.go`, `dslgate.go`.

   Each must still reach a positive over statement bodies.
5. **Task 16, ship.** The PR body is drafted in the plan (Task 16, "PR body
   draft"). Epic 2 took three behaviour changes this plan once listed: forge's
   no-op transition, and the workbench and deploypack orders. Open the PR
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
- Scenario runner specifics:
  - Every script action dispatches the one capability `shell.script`, so a
    call is labelled `shell.script:<script>`.
  - Resume refuses to re-run a failed action step unless `AllowSideEffects` is
    set. The runner sets it because its injected failure comes before the step
    runs.
  - `failAt` counts the top-level steps the registry is asked to run. A
    for-each's children go through the inner registry, and a flattened switch
    adds steps after the flip, so pick a small index.
  - The dry run intercepts actions, so an action's result reads absent there.
    The deployment dry run therefore takes the failure path, with nine
    intercepted calls.
  - A relationship field reads back canonical (`v1:identity:user:<id>`).
