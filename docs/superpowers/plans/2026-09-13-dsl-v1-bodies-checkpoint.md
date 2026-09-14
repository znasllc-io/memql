# DSL v1 body language: checkpoint

Where epic memql#5370 (tasks #5371-#5374) stands, for the session that picks it
up. It sits beside the plan (`2026-09-13-dsl-v1-bodies.md`) and is deleted with
it in the epic's merge (plan Task 16 step 3). Update it each time you stop.

## Resume here

- **Worktree:** `/home/znas/memql-projects/epic-dsl-v1-bodies`, branch
  `epic/dsl-v1-bodies`. It is local only and has never been pushed; other
  sessions on this machine share its refs.
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
2. **Task 13, the flip.** Unblocked, since epic 2's grammar is the only one.
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
