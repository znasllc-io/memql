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

## Resume here

- **Read these first:**
  - this file;
  - the plan, from its "Decisions this plan makes" section (D-A to D-M), then the task you are resuming;
  - the design record `docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md`, section 4 epic 5 and D18-D20.
- **Epic branch:** `epic/dsl-v1-loop-protection`, pushed to origin.
  - Local worktree: `/home/znas/memql-projects/epic-dsl-v1-loop-protection`.
  - It holds Tasks 0-3 and 8, merged and reviewed clean.
- **Stream branches are all merged or empty.** Continue on the epic branch itself, or branch new streams off it:
  - `loops/s1-cause`, `loops/s2-annotations`, `loops/s3-graph` and `loops/s4-runtime` are merged into the epic.
  - `loops/s5-surface` and `loops/s6-arch` carry no commits beyond `3564d0672`, because their tasks never started.
  - Their worktrees are removed at the checkpoint. Only `loops/s4-runtime` was pushed, and it is deleted with the others at the end.

- **Recreating the worktrees on a new machine:** `git worktree add -b <local> <path> origin/<branch>`. In a fresh worktree, copy the gitignored assets from a primary checkout before building, or the root package will not build:
  - `bin/tools/` (protoc and its plugins);
  - `component/identity/web/static/app.css`, `favicon.svg`, `fonts/`.
- **Database for db-gated runs:**
  - DSN: `postgres://memql:memql_dev@localhost:15434/memql_loops?sslmode=disable` (container `memql-throwaway-laneb`).
  - Create the database once: `docker exec memql-throwaway-laneb psql -U memql -d postgres -c "CREATE DATABASE memql_loops OWNER memql;"`.
- **The execution ledger** (git-ignored, on this machine only): `/home/znas/memql-projects/epic-dsl-v1-loop-protection/.superpowers/sdd/2026-09-14-dsl-v1-loop-protection/`. It holds `progress.md`, every task brief, report and review package, and `task-12-design.md`.
  - The design and the reports are ALSO committed beside this file under `2026-09-14-dsl-v1-loop-protection-sdd/`, so they survive a new machine.

## State

| Task | Issue | State | Commits |
|---|---|---|---|
| 0 Scaffold (`events.Cause`, `LoopConfig`, `ModeConfig`) | #5382 | done | `e8a2ebb6c` |
| 1 Cause on the envelope and across the mesh | #5382 | done, reviewed clean | `403d3714e` |
| 2 Every publisher stamps the cause | #5382 | done, reviewed clean | `68b920e54` |
| 3 `@loop` / `@mode` surface | #5381 | done, reviewed clean after 1 fix round | `9dab9f1a1`, `2102b61f5` |
| 8 Static graph (report-only) | #5381 | done, reviewed clean after 1 fix round | `1f16dd509`, `3bb32b4a6` |
| merges into epic | | stream 1 `8f7908cd5`, stream 2 `d8f335822`, stream 3 `6cdcfc500`, fixup `3564d0672` | |
| 4 Run chain, depth cap, journaled refusal | #5382 | done; merged into the epic (`a57b89f93`). The review was checkpointed before it finished: provisionally Approved, no Critical or Important findings, 9 Minor (`sdd/task-4-review-partial.md`) | `f3d5180e9` |
| 9, part: forge `routeRequest` first-version filter | #5381 | done on the epic (`aac4c7066`); the embedded tree now measures 0 problems | `aac4c7066` |
| arch model regenerated for Tasks 0-4 | | done (`3e958ef6d`) | |
| 9 the rest: fleet + deploypack filters, the flip, the four carried items | #5381 | NOT STARTED: the implementer checkpointed during research. Its findings are in `sdd/task-9-report.md` | |
| 11 Architecture model renders the graph | #5384 | NOT STARTED (research only; the design is in `sdd/task-11-report.md`) | |
| 12 OS surface: Cluster > Automations | #5384 | NOT STARTED (research only, in `sdd/task-12-report.md`; the design is in `sdd/task-12-design.md`) | |
| 5 Dedup key, narrowed and chain-scoped | #5382 | not started (needs Task 4 reviewed) | |
| 6 Per-(automation, row) budget | #5382 | not started | |
| 7 Modes at run time | #5382 | not started | |
| 10 memqllint and the corpus runner run the graph | #5381 | not started | |
| 13 Merge epic 3, walk the statement form | | blocked on epic 3 (#5370) merging to main | |
| 14 Before-write body | #5383 | blocked on 13 | |
| 15 Loop scenarios in the corpus | #5384 | blocked on 13 and 14 | |
| 16 Documentation | all | not started | |
| 17 Verification and delivery (PR, queue, close issues, cleanup) | all | not started | |

## The partial merge to main (owner's instruction, 2026-09-14)

The owner asked to merge as much as possible to main right away, WITHOUT closing
any issue, and to leave a status comment on each issue for the session that takes
over.

**What the partial PR carries** (from `epic/dsl-v1-loop-protection`):

- Tasks 0-4:
  - the cause on the envelope and across the mesh;
  - every publisher stamping it;
  - `@loop` / `@mode` as annotations the registry, the parser and the load know;
  - the static loop graph, REPORT-ONLY;
  - the run's chain with the depth cap and the journaled `loop_depth_exceeded` refusal.
- The forge fix.
- The regenerated architecture model.
- This checkpoint and the plan.

**What it does NOT carry**, the remaining work below:

- The dedup key (Task 5).
- The per-row budget (Task 6).
- `@mode` at run time (Task 7). Until then, `@mode` parses and validates but has no effect.
- The rest of Task 9, including the flip that makes an uncovered cycle refuse the load.
- memqllint and the corpus (Task 10).
- The architecture model's automation graph (Task 11).
- The OS surface (Task 12).
- Everything in Phase B.

**Before the next PR.** After the partial PR merges, `main` holds everything above. `epic/dsl-v1-loop-protection` stays the working branch: merge `origin/main` into it (a no-op or trivial), then continue from "Next, in order" below. The FINAL PR is the one that closes #5380-#5384.

**What memql-22 was told.** Epic 5's first half lands on main BEFORE their epic 3. That breaks this checkpoint's earlier promise not to enqueue first, at the owner's instruction. Their merge picks up, and must resolve:

- `publishEvent(ctx, ...)` in `executor.go` and `resume.go`;
- the new `loop_*.go` call sites in `executeWithEvent`;
- `LoaderOptions.Functions` plus the loop check in `unified_loader.go`;
- the `@loop` / `@mode` placements in `registry.go`;
- the editor pins, with GrammarVersion `2026.09-dsl-v1-loop-protection-930e046b` and editor 0.5.0, which theirs must now move past.

## Task 4 follow-up found at the checkpoint (do this first in stream 4)

The journal's own writes (`v1:work:run` / `v1:work:step`) carry the run's cause, because they run under the run's context. The workbench's `releaseWorkspaceOnRunTerminal` triggers on `graph.node.updated.v1:work:run`, so it runs one deeper than the run it releases for.

The consequence: a run at depth exactly 16 that used a workbench would have its workspace release refused as `loop_depth_exceeded`, leaving the workspace leaking.

The fix is to have `journalContext` strip the cause, so journal bookkeeping publishes root events (`events.ContextWithCause(ctx, events.Cause{})` or a dedicated strip). Add a test: a depth-16 run's terminal journal write triggers a journal-reacting automation at depth 1, not 17.

## Next, in order

1. **Finish Task 4.** It is merged into the epic; its review was provisionally Approved with 9 Minor findings (`sdd/task-4-review-partial.md`).
   - First apply the journal-cause follow-up above.
   - Then, optionally, finish the review's unreached items: run `-race` and the db test, and check whether a sandbox dry-run refusal increments the production metric (`stopLoop` counts unconditionally).
2. **Tasks 5, 6, 7**, in that order, on `loops/s4-runtime` or on the epic after the merge.
   - Task 5 uses Task 8's concept-aware `newFunctionSource(fns, registry)`, not the public `NewFunctionSource`.
   - Task 5 must share the per-event fingerprint that Task 4 already computes, rather than computing it a second time.
3. **Finish Task 9** on the epic branch. Its report's Checkpoint section has the research.
   - Forge is DONE (`aac4c7066`). Two cycles remain to fix, with `@filter`:
     - the fleet's five `v1:fleet:instance` automations: status filters (provisioning, suspending, resuming, tearing_down, and running for the welcome), as in the Task 9 brief;
     - deploypack's two automations: status filters (`in_progress` / `succeeded`).
   - Two rulings from the implementer's research:
     - The corpus runner builds its loader WITHOUT `Functions`, so a `loop_cycle` negative case reads `load_ok` until Task 10's corpus wiring. Wire the corpus runner's `Functions` inline in Task 9, taking that half of Task 10.
     - The AUTHORED scheduler wires a `@disabled` automation, unlike the core scheduler. So the rule "exclude disabled automations from the graph" applies to SHIPPED automations only. Activation includes every authored automation the authored scheduler would wire, disabled or not; otherwise `@disabled` becomes a way past the check.
     - The scheduler already holds `s.entries`. The entries carry no Origin, and the entry being replaced must be left out.
   - Four carried items:
     - exclude disabled automations from the graph;
     - add an `app/engine.go` wiring gate for `Functions:`, and log "not run" at WARN;
     - activation covers shipped automations plus every active authored one plus the candidate;
     - add a test of a non-`@loop` self-edge inside a covered component.
   - Then flip `checkLoops` from report-only to appending problems.
   - Finally, Task 10 (memqllint and the corpus runner) on the same branch.
4. **Finish Tasks 11 and 12** from their reports' Checkpoint sections.
   - Task 12 is the owner's visible UI ask. Build to `2026-09-14-dsl-v1-loop-protection-sdd/task-12-design.md`.
   - Do the real-browser QA pass: screenshots in both themes, populated and empty, at 1280 and 600 wide.
   - The QA harness, if one was started, was copied to `/tmp/claude-1000/-home-znas-memql-projects-memql/d5c8d310-fa73-4a0a-a885-f54c7f160257/scratchpad/automations-qa-harness/`. That path exists on this machine only.
5. **Merge streams 3, 5 and 6 into the epic.** Resolve conflicts by hand, except `topology.model.json`: take one side, then regenerate.
6. **Phase B, once memql-22's epic 3 (#5370) is on origin/main:** Tasks 13 to 17 of the plan.
   - Merge `origin/main` into the epic. Take THEIR side of `executor.go`, `loader.go`, `types.go`, `resume.go` and `registry.go`, then re-apply this epic's call sites.
   - Delete the graph's legacy switch walk.
   - Build the before-write body to D-M as memql-22 reviewed it: timing on the trigger, and `row.<field> = <expr>` as the one new statement kind.
   - Bump GrammarVersion on top of theirs.
7. **Ship (Task 17).**
   - One PR from `epic/dsl-v1-loop-protection` into main, with one `Closes #n` line each for #5380, #5381, #5382, #5383 and #5384.
   - Delete this plan and checkpoint in the last commit.
   - CI green, then enqueue with a bare `gh pr merge <n> --repo znasllc-io/memql`. Watch queue membership over GraphQL.
   - After the merge: close any issue left open; delete the `loops/*` branches locally and on origin, and remove the worktrees `wt-loops-*` and `epic-dsl-v1-loop-protection`. Never touch memql-22's worktrees.
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

## Deferred minor findings (the final whole-branch review triages these)

- **Task 1:** a proto decode allocates an empty non-nil Chain, where Clone keeps it nil. Unreachable in practice.
- **Task 3:**
  - `renameParam` does not avoid capture (`loop_until.go`), so `untilInFilter` can accept a non-negation when a nested lambda binds the renamed-to name. Fix: when a nested lambda binds `to` and its body mentions `from`, report no match.
  - The until-form parse refusal does not name the automation (`parser/loop_mode.go`).
  - Load refusals print the raw until text; use `ast.FormatExpr`.
  - Tests assume `MEMQL_AUTOMATION_MAX_CHAIN_DEPTH` is unset: pin it in `loop_prepare_test.go`, and drop "16" from the corpus loop message.
  - The `parseJSON` path skips `prepareLoopAndMode`. Unreachable today.
- **Task 8:**
  - `loop_graph.go` names `s.Switch` outside `legacySwitchSteps`. Fix it at the Task 13 merge.
  - The graph is rebuilt on every `LoadByName`. Task 12 caches it.
  - The public `NewFunctionSource` has no concepts. Tasks 5 and 11 must use the concept-aware constructor.
  - `v1:memql:automation:step` inserts are an unmodelled engine write. Say so in the header.
  - Number comparison can drift between int64 and float64 above 2^53.
  - The measurement test adds about 7s per run and logs the pre-existing shopifypack `shop` ambiguity.
  - There are three separate "declared args" projections with no shared helper.
- **Task 4, concerns from its report:**
  - A refused run publishes no `automation.failed`, and the scheduler logs a generic ERROR beside the WARN.
  - A sub-automation call without args, and a compiled goal's run, journal no cause, so a resume restarts the chain. That is a break, never a false stop.

## Peers and promises

- **memql-22** (another local session) owns epic 3 (#5370): flip steps F5 (legacy runtime deleted), F6 (`mutate` becomes `mutation`) and F7 (editor pins).
  - They promised to message the epic-3 merge SHA. If a new session cannot receive it, poll `gh pr list --repo znasllc-io/memql --state all --search "dsl-v1-bodies"`.
  - This epic promised not to enqueue before theirs merges, and to take their side of `executor.go`, `loader.go`, `types.go`, `resume.go` and `registry.go` on merge, then re-apply its call sites.
  - They reviewed the before-write shape (D-M) and are waiting on nothing from us.

## Facts that bite

- **Test commands.** Verify with module paths (`go test github.com/znasllc-io/memql/...`), never `go test ./...`. Stage by explicit path. gopls "not included in your workspace" diagnostics in these worktrees are noise.
- **Proto regeneration** must use the primary tree's plugins. Copy `bin/tools/`, or the doc-comment format churns across unrelated generated files.
- **`make os-test` runs `npm ci`.** Only `make os-build` parses the stylesheet. Never run prettier on `clients/os`.
- **The arch model** is 55MB of single-line JSON. Regenerate it LAST, after the final Go edit. On a conflict, take one side and regenerate. Its staleness gate needs `dsl/**` in its CI path filter (Task 11).
- **The trigger filter's `row`** is the stored payload plus intrinsics. `firstVersion` is readable only as `args.firstVersion`, and only when declared.
- **Every write publishes `graph.node.created`; an update also publishes `.updated`.** Nothing emits `.deleted`.
- **Unrelated bugs the surveys found**, to file as issues rather than fix here:
  - `deploy/fleet/dsl/fleet/billing.memql` and `trial.memql` automations never load, because the loader reads only `automations.memql`.
  - `dsl/workbench` `releaseWorkspaceOnRunTerminal` passes `reason: "run_terminal"`, which the mutation's enum does not contain.
