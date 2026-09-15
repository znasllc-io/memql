# DSL v1 loop protection: checkpoint

This checkpoint says where epic memql#5380 (tasks #5381-#5384) stands, for the
session that picks it up. It sits beside the plan
(`2026-09-14-dsl-v1-loop-protection.md`), and both are deleted in the epic's
merge. Update it each time you stop.

The owner asked for this checkpoint on 2026-09-14 (~18:30 MST) because the
session was running out of credits. Their instructions for the epic still hold:

- all of #5380-#5384 done end to end, in ONE PR for all issues;
- merged into main;
- local cleanup (stale branches and worktrees removed) and every issue closed;
- then report that it is done so they can test it;
- do an excellent job on the UX/UI (the OS surface, Task 12).

Later that evening they asked the session to stop at a good point. This file is
that stopping point. They also asked it to:

- merge as much as possible to main, with no issue closed;
- comment on each issue, so another session can take over.

## Where it stands

Roughly 45% of the epic by effort.

**Built and on its way to main (PR #5442):**

- the causation chain on every event, locally and across the mesh;
- `@loop` and `@mode` as annotations;
- the static loop graph, report-only;
- the depth cap, which refuses a run as a journaled `loop_depth_exceeded` failure;
- the forge fix;
- the port onto epic 3's statement bodies.

**Remaining:**

- Tasks 5-7: dedup, the row budget, and modes at run time.
- The rest of Task 9, which includes flipping the loop check from report-only to refusing the load.
- Task 10.
- Task 11.
- Task 12: the OS screen. The engine half is WIP on `loops/ui`.
- Task 14: the before-write body.
- Tasks 15-17.

## Resume here

- **Read these first:**
  - this file;
  - the plan, from its "Decisions this plan makes" section (D-A to D-M), then the task you are resuming;
  - the design record `docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md`, section 4 epic 5 and D18-D20.
- **PR #5442 first.** If it is still open when you arrive, merge it with `scripts/dev/merge-as-owner.sh --pr=5442`. Run `--check` first, and merge only once `ci-required` is green.
  - It closes no issue, on purpose.
  - Once it is merged, merge `origin/main` into the epic branch. That merge changes nothing and only keeps the ancestry straight.
- **Two branches remain, both pushed. Each has a local worktree.**
  - `epic/dsl-v1-loop-protection`, at `/home/znas/memql-projects/epic-dsl-v1-loop-protection`, is the working branch. It holds everything PR #5442 carries:
    - Tasks 0-4 and 8, reviewed;
    - the forge part of Task 9;
    - the regenerated architecture model;
    - the epic 3 merge. memql-22's merge `f79e20823` ported the loop code onto the statement step model, and `3da31799a` and `10444e47b` moved this epic's tests and corpus cells onto epic 3's API and syntax;
    - a merge of `origin/main`, `aa8512f50`.
  - `loops/ui`, at `/home/znas/memql-projects/wt-loops-ui`, holds Task 12's engine half as WIP (`206772687`).
    - It is based on `cb46bf0fb`, BEFORE the epic 3 merge, so merge the epic branch into it first.
    - It is deliberately NOT in PR #5442. Its generated SDK files are stale (it never ran `make sdk-gen`), and `sdk-gen-check` would red the PR.
    - The "Checkpoint (second session)" section of `2026-09-14-dsl-v1-loop-protection-sdd/task-12-report.md` says what is done and what remains, part by part.
- **Every stream branch is merged and deleted, locally and on origin.** Only the two branches above remain. New parallel streams branch off the epic.
- **Recreating the worktrees on a new machine:** `git worktree add -b <local> <path> origin/<branch>`. In a fresh worktree, copy the gitignored assets from a primary checkout before building, or the root package will not build:
  - `bin/tools/` (protoc and its plugins);
  - `component/identity/web/static/app.css`, `favicon.svg`, `fonts/`.
- **Database for db-gated runs:**
  - DSN: `postgres://memql:memql_dev@localhost:15434/memql_loops?sslmode=disable` (container `memql-throwaway-laneb`).
  - Create the database once: `docker exec memql-throwaway-laneb psql -U memql -d postgres -c "CREATE DATABASE memql_loops OWNER memql;"`.
- **The execution ledger** (git-ignored, on this machine only): `/home/znas/memql-projects/epic-dsl-v1-loop-protection/.superpowers/sdd/2026-09-14-dsl-v1-loop-protection/`. It holds `progress.md`, every task brief, report and review package, and `task-12-design.md`.
  - The design and the reports are ALSO committed beside this file under `2026-09-14-dsl-v1-loop-protection-sdd/`, refreshed at this checkpoint, so they survive a new machine.

## State

| Task | Issue | State | Commits |
|---|---|---|---|
| 0 Scaffold (`events.Cause`, `LoopConfig`, `ModeConfig`) | #5382 | done | `e8a2ebb6c` |
| 1 Cause on the envelope and across the mesh | #5382 | done, reviewed clean | `403d3714e` |
| 2 Every publisher stamps the cause | #5382 | done, reviewed clean | `68b920e54` |
| 3 `@loop` / `@mode` surface | #5381 | done, reviewed clean after 1 fix round | `9dab9f1a1`, `2102b61f5` |
| 8 Static graph (report-only) | #5381 | done, reviewed clean after 1 fix round | `1f16dd509`, `3bb32b4a6` |
| 4 Run chain, depth cap, journaled refusal | #5382 | done and merged. Its review was checkpointed before it finished: provisionally Approved, with no Critical or Important findings and 9 Minor (`sdd/task-4-review-partial.md`). The journal-cause follow-up below is NOT done | `f3d5180e9`, merge `a57b89f93` |
| 9, part: forge `routeRequest` first-version filter | #5381 | done; the embedded tree measures 0 problems | `aac4c7066` |
| arch model regenerated for Tasks 0-4 | | done | `3e958ef6d` |
| 13 Merge epic 3, walk the statement form | | done. memql-22's merge ported the graph onto statements: `collectStepFacts` walks the statement body, `logicCalls` reads `fn.LogicBody`, and the legacy switch walk is gone. `3da31799a` ported this epic's tests and corpus cells (`sdd/merge-port-report.md`) and deleted `TestLoopGraph_StepsBuiltInGo`, whose step kinds no longer exist. `10444e47b` rewrote the automation-corpus golden for `routeRequest` | `f79e20823`, `3da31799a`, `10444e47b` |
| 12 OS surface: Cluster > Automations | #5384 | engine half WIP on `loops/ui`: the builtins, two virtual concepts, the query and shape, the seeds, and 16 green tests. Its fan-out gates and the whole OS half are NOT started | `206772687` (on `loops/ui`) |
| 9 the rest: fleet + deploypack filters, the flip, the four carried items | #5381 | not started; the research is in `sdd/task-9-report.md` | |
| 11 Architecture model renders the graph | #5384 | not started; the research and design are in `sdd/task-11-report.md` | |
| 5 Dedup key, narrowed and chain-scoped | #5382 | not started | |
| 6 Per-(automation, row) budget | #5382 | not started | |
| 7 Modes at run time | #5382 | not started | |
| 10 memqllint and the corpus runner run the graph | #5381 | not started | |
| 14 Before-write body | #5383 | not started; UNBLOCKED now that epic 3 is on main | |
| 15 Loop scenarios in the corpus | #5384 | blocked on 14 | |
| 16 Documentation | all | not started | |
| 17 Verification and delivery (PR, queue, close issues, cleanup) | all | not started | |

## What main holds after PR #5442

The owner asked to merge as much as possible to main right away, WITHOUT closing
any issue, and to leave a status comment on each issue for the session that takes
over.

**PR #5442 carries** everything on `epic/dsl-v1-loop-protection` through `aa8512f50`, plus the commit that refreshed this checkpoint:

- Tasks 0-4:
  - the cause on the envelope and across the mesh;
  - every publisher stamping it;
  - `@loop` / `@mode` as annotations the registry, the parser and the load know;
  - the static loop graph, REPORT-ONLY;
  - the run's chain with the depth cap and the journaled `loop_depth_exceeded` refusal.
- The forge fix.
- The regenerated architecture model.
- The port onto epic 3's statement bodies (Task 13).
- This checkpoint, the plan, and the committed reports.

**It does NOT carry** the remaining work below:

- The dedup key (Task 5).
- The per-row budget (Task 6).
- `@mode` at run time (Task 7). Until then, `@mode` parses and validates but has no effect.
- The rest of Task 9, including the flip that makes an uncovered cycle refuse the load.
- memqllint and the corpus (Task 10).
- The architecture model's automation graph (Task 11).
- The OS surface (Task 12), whose engine half waits on `loops/ui`.
- The before-write body (Task 14), the scenarios (15), and the docs (16).

The FINAL PR, from `epic/dsl-v1-loop-protection`, is the one that closes #5380-#5384.

## Task 4 follow-up (do this first)

The journal's own writes (`v1:work:run` / `v1:work:step`) carry the run's cause, because they run under the run's context. The workbench's `releaseWorkspaceOnRunTerminal` triggers on `graph.node.updated.v1:work:run`, so it runs one deeper than the run it releases for.

The consequence: a run at depth exactly 16 that used a workbench would have its workspace release refused as `loop_depth_exceeded`, leaving the workspace leaking.

The fix is to have `journalContext` strip the cause, so journal bookkeeping publishes root events (`events.ContextWithCause(ctx, events.Cause{})` or a dedicated strip). Add a test: a depth-16 run's terminal journal write triggers a journal-reacting automation at depth 1, not 17.

## Next, in order

1. **PR #5442**, if it is still open (see "Resume here").
2. **Finish Task 4.**
   - Apply the journal-cause follow-up above.
   - Then, optionally, finish the review's unreached items. Run `-race` and the db test. Check whether a sandbox dry-run refusal increments the production metric: `stopLoop` counts unconditionally.
3. **Tasks 5, 6, 7**, in that order, on the epic branch.
   - Task 5 uses Task 8's concept-aware `newFunctionSource(fns, registry)`, not the public `NewFunctionSource`.
   - Task 5 must share the per-event fingerprint that Task 4 already computes, rather than computing it a second time.
   - The plan's code blocks predate epic 3. Automation bodies are statements now: `mutate` is `mutation`, and the step structs the plan names may be gone. `memqlmigrate --rewrite=bodies --go-fixtures` converts old Go test literals.
4. **Finish Task 9**, then Task 10. The Checkpoint section of Task 9's report has the research.
   - Forge is DONE (`aac4c7066`). Two cycles remain to fix, with `@filter`:
     - the fleet's five `v1:fleet:instance` automations need status filters (provisioning, suspending, resuming, tearing_down, and running for the welcome), as in the Task 9 brief;
     - deploypack's two automations need status filters (`in_progress` / `succeeded`).
   - Rulings from the implementer's research:
     - The corpus runner builds its loader WITHOUT `Functions`, so a `loop_cycle` negative case reads `load_ok` until Task 10's corpus wiring. Wire the corpus runner's `Functions` inline in Task 9, taking that half of Task 10.
     - The AUTHORED scheduler wires a `@disabled` automation, unlike the core scheduler. So the rule "exclude disabled automations from the graph" applies to SHIPPED automations only. Activation includes every authored automation the authored scheduler would wire, disabled or not; otherwise `@disabled` becomes a way past the check.
     - The scheduler already holds `s.entries`. The entries carry no Origin, and the entry being replaced must be left out.
   - Four carried items:
     - exclude disabled automations from the graph;
     - add an `app/engine.go` wiring gate for `Functions:`, and log "not run" at WARN;
     - activation covers shipped automations plus every active authored one plus the candidate;
     - add a test of a non-`@loop` self-edge inside a covered component.
   - Then flip `checkLoops` from report-only to appending problems.
5. **Tasks 11 and 12**, from their reports' Checkpoint sections.
   - Task 12 is the owner's visible UI ask. On `loops/ui`:
     - merge the epic branch in;
     - run the fan-out gates its report lists (`make sdk-gen`, `sdk/ts` build, root and `scripts/ci` gates);
     - build the OS half to `2026-09-14-dsl-v1-loop-protection-sdd/task-12-design.md`;
     - merge the branch into the epic.
   - Do the real-browser QA pass: screenshots in both themes, populated and empty, at 1280 and 600 wide. Task 12's report says how to dump the real tree's graph rows as a fixture.
6. **Task 14, the before-write body**, to D-M as memql-22 reviewed it:
   - the timing goes on the trigger;
   - `row.<field> = <expr>` is the one new statement kind.
   - Bump GrammarVersion on top of `2026.09-dsl-v1-loop-protection-cf139af9`, with the editor pins and the changelog in the same commit.
7. **Task 15** (the loop scenarios in the corpus), then **Task 16** (the docs).
8. **Ship (Task 17).**
   - One PR from `epic/dsl-v1-loop-protection` into main, with one `Closes #n` line each for #5380, #5381, #5382, #5383 and #5384.
   - Delete this plan, this checkpoint and the `-sdd/` directory in the last commit.
   - Once CI is green, enqueue with a bare `gh pr merge <n> --repo znasllc-io/memql`, or use `scripts/dev/merge-as-owner.sh --pr=<n>`. Watch queue membership over GraphQL.
   - After the merge:
     - close any issue left open;
     - delete `loops/ui` and the epic branch, locally and on origin;
     - remove the worktrees `wt-loops-ui` and `epic-dsl-v1-loop-protection`;
     - never touch memql-22's worktrees.
   - Report to the owner so they can test.

## Rulings made (do not re-litigate; each carries what it costs if wrong)

- **Parallel streams.** Streams run in parallel in separate worktrees, with file ownership split by the plan's stream table. Cost if wrong: small merge conflicts.
- **Task 8's `@loop` tests.** They build `*Automation` in Go and set `Loop` directly. Cost if wrong: one parser-path gap, covered by Task 3's corpus cells.
- **Task 9's fixes use `@filter` only.** A genuine remaining cycle gets `@loop`. Cost if wrong: one extra commit.
- **Task 4's Step 4 is struck.** Task 3 registered `MEMQL_AUTOMATION_MAX_CHAIN_DEPTH`. Cost if wrong: none.
- **The graph judges `args` by declaration.** The filter decider reads `args.<f>` as ABSENT unless the judged automation declares `f`. That matches the runtime binding, and it overrides the plan's wording. Cost if wrong: none; it only keeps edges the runtime can fire.
- **The refusal's fix text** says to declare `firstVersion bool` and then filter on it.
- **Disabled automations are excluded from the graph**, because the scheduler never wires them. This is carried into Task 9.
- **Activation's loop check** covers shipped automations, every active authored one, and the candidate. This is carried into Task 9.
- **`checkLoops` passes depth cap 0 on purpose.** Load's `prepareLoopAndMode` already refuses a `@loop` bound outside `1..maxChainDepth()`.
- **The dedup key (D-E) is chain-scoped:** the correlation is in the key. A root event keeps today's one-execution-per-event identity. A row that returns to an earlier state from a new cause still fires. The literal reading of the record would drop that second toggle through the cluster guard's one-hour claim.
- **Modes are per process** (D-G). The default is unbounded parallel, today's behaviour. The gate runs before the concurrency slot.
- **The before-write body (D-M, reviewed by the epic-3 session 2026-09-14):**
  - `@trigger(before="create"|"update"|"write", concept=...)`, with no wrapper block;
  - `row.<field> = <expr>` is the one new statement kind;
  - refusal codes `before_write_writes`, `before_write_field`, `before_write_outside`, `before_write_trigger`;
  - no run row;
  - its journal shape is to be settled in Task 14.
- **The OS surface** is a new Cluster app section "Automations" (`requires: "app:cluster/automations"`, seeded for owner, developer and admin).
  - It is fed by the `@sdk` builtins `automationGraph` and `automationLoopStops` (`@requiresCapability("read", "app:cluster/automations")`), read with `useReading`.
  - It is plain SVG, one column per stratum.
- **Task 12's engine half, as built on `loops/ui`.**
  - Loop stops are nodes of a SECOND virtual concept, `v1:platform:automationLoopStop`, not `v1:work:run`. A top-level builtin's reply is row-gated by concept on the CALLER, and a stop under `v1:work:run` would reach every developer and admin as an empty list. The stop concept declares no tier; the capability is the wall.
  - The graph row's filter field is `triggerFilter`, because a concept line starting with `filter` trips memqllint's retired-operator gate.
  - The graph source returns an error when there is no graph, so "no graph" never reads as "no automations".
  - The branch stays off the epic until its fan-out gates pass.
- **The partial PR carries the epic 3 port.** Epic 3 merged first (#5441), so #5442 goes in after it, with the port. Cost if wrong: none; main never held a half-ported tree.

## Deferred minor findings (the final whole-branch review triages these)

- **Task 1:** a proto decode allocates an empty non-nil Chain, where Clone keeps it nil. Unreachable in practice.
- **Task 3:**
  - `renameParam` does not avoid capture (`loop_until.go`), so `untilInFilter` can accept a non-negation when a nested lambda binds the renamed-to name. Fix: when a nested lambda binds `to` and its body mentions `from`, report no match.
  - The until-form parse refusal does not name the automation (`parser/loop_mode.go`).
  - Load refusals print the raw until text; use `ast.FormatExpr`.
  - Tests assume `MEMQL_AUTOMATION_MAX_CHAIN_DEPTH` is unset: pin it in `loop_prepare_test.go`, and drop "16" from the corpus loop message.
  - The `parseJSON` path skips `prepareLoopAndMode`. Unreachable today.
- **Task 8:**
  - The graph is rebuilt on every `LoadByName`. Task 12 caches it (on `loops/ui`).
  - The public `NewFunctionSource` has no concepts. Tasks 5 and 11 must use the concept-aware constructor.
  - `v1:memql:automation:step` inserts are an unmodelled engine write. Say so in the header.
  - Number comparison can drift between int64 and float64 above 2^53.
  - The measurement test adds about 7s per run and logs the pre-existing shopifypack `shop` ambiguity.
  - There are three separate "declared args" projections with no shared helper.
  - (Resolved by the epic 3 merge: the graph no longer names `s.Switch` outside a legacy walk.)
- **Task 4, concerns from its report:**
  - A refused run publishes no `automation.failed`, and the scheduler logs a generic ERROR beside the WARN.
  - A sub-automation call without args, and a compiled goal's run, journal no cause, so a resume restarts the chain. That is a break, never a false stop.
- **Task 12, from its report:**
  - The OS imports only the brand FACES, so the design's data tints (`--memql-data-*`) are unreachable from `clients/os`. Carry the data voice with the mono face and existing `--os-*` tokens, or add `--os-data-*` roles as a theme-pack change.
  - `--os-text-xl` does not exist; the stratum numerals use `--os-text-lg`.
  - The section files go under `clients/os/src/apps/cluster/automations/`, not the brief's `src/cluster/automations/`.
  - The detail pane's `@mode` sentences presume Task 7.

## Peers and promises

- **memql-22** (another local session) owned epic 3 (#5370). It merged to main as #5441 (`945d64135`).
  - Their merge branch (`f79e20823`) is this epic's merge of epic 3.
  - Both promises are kept: epic 3 went in first, and epic 5 took their side of the runtime.
  - Nothing is owed either way. They reviewed the before-write shape (D-M).

## Facts that bite

- **Test commands.** Verify with module paths (`go test github.com/znasllc-io/memql/...`), never `go test ./...`. Stage by explicit path. gopls "not included in your workspace" diagnostics in these worktrees are noise.
- **Proto regeneration** must use the primary tree's plugins. Copy `bin/tools/`, or the doc-comment format churns across unrelated generated files.
- **`make os-test` runs `npm ci`.** Only `make os-build` parses the stylesheet. Never run prettier on `clients/os`.
- **The arch model** is 55MB of single-line JSON. Regenerate it LAST, after the final Go edit. On a conflict, take one side and regenerate. Its staleness gate needs `dsl/**` in its CI path filter (Task 11).
- **The trigger filter's `row`** is the stored payload plus intrinsics. `firstVersion` is readable only as `args.firstVersion`, and only when declared.
- **Every write publishes `graph.node.created`; an update also publishes `.updated`.** Nothing emits `.deleted`.
- **Epic 3's statement model:**
  - automation and logic bodies are statements;
  - the step switch, query, inline-mutation, webhook and hook arms are gone;
  - `mutate` is `mutation`;
  - a `publish` statement takes a literal topic. The graph still handles an expression topic, which only a Go-built event step can carry now, as undecided edges;
  - epic 3's automation corpus (`component/automations/steps`, `TestAutomationCorpusRuns`) synthesizes each fixture event from the automation's declared args. Declaring an arg, as `firstVersion` was, changes that automation's golden input: rewrite it with `-update` and check that only the input moved.
- **Unrelated bugs the surveys found**, to file as issues rather than fix here:
  - `deploy/fleet/dsl/fleet/billing.memql` and `trial.memql` automations never load, because the loader reads only `automations.memql`.
  - `dsl/workbench` `releaseWorkspaceOnRunTerminal` passes `reason: "run_terminal"`, which the mutation's enum does not contain.
