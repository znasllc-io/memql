# DSL v1 foundations (memql#5356) -- resume checkpoint

Written 2026-09-14, when the session that ran this epic ran low on tokens. A
new session resumes from here. **Delete this file together with the plan**
(`2026-09-13-dsl-v1-foundations.md`) in the last commit before the merge.

## What the owner asked for

1. All seven issues (#5356-#5362) in ONE PR, merged to main.
2. The issues and the PR closed.
3. Local cleanup: stale branches and worktrees removed.
4. Excellent UX for the user-facing parts.
5. Tell the owner when it is completely done, so they can test it.

## Where things are

**The epic branch.** `epic/dsl-v1-foundations`, on origin. Its content tip is
`a919fa86f`:

- all six tasks are merged, each reviewed clean after its fix rounds;
- `origin/main` is merged in at `456623efe`, and main has not moved since (checked 2026-09-14 09:00);
- it was verified there: the whole tree, the db-gated trees, all eight node tags, the generators, the extension tests and packaging (0.4.0).

**The worktree.** `/home/znas/memql-projects/epic-dsl-v1-foundations`.

**The recovery map** is git-ignored and lives on this machine at
`/home/znas/memql-projects/epic-dsl-v1-foundations/.superpowers/sdd/2026-09-13-dsl-v1-foundations/`:

| File | What it is |
|---|---|
| `progress.md` | The ledger: every task, commit range, ruling, deferred minor and coordination note. |
| `final-fix-A-brief.md`, `final-fix-B-brief.md` | The final fix wave's requirements. They restate every finding of the final review, with file:line. |
| `final-fix-A-report.md`, `final-fix-B-report.md` | The implementers' reports: per item, done, partial or not started, and what remains. |
| `pr-body.md` | The PR description draft. It needs updating for the fix wave; see step 8. |
| `verify-final.sh` | CI's lanes, run locally in CI's own commands: `bash verify-final.sh <worktree> <out-file>`. |
| `gaps-issue.md` | The engine-gaps follow-up, filed as **memql#5426**. |
| `final-review-*.diff`, `review-*.diff` | The review packages. |

**The final whole-branch review is done.** It was opus, over range
`456623efe..61044661c`. Verdict: "With fixes": 0 critical, 4 important, 8 minor.
Every finding is restated in the two briefs.

## In flight: the final fix wave

Two implementers ran in parallel worktrees off `a919fa86f`. At the checkpoint
they were told to commit their work, WIP included, and write their reports.
Both branches are pushed to origin.

**A: engine and docs.**

- Worktree: `/home/znas/memql-projects/epic-dsl-v1-final-engine`
- Branch: `wt/dsl-v1-final-engine`
- Items A1-A9: the `-w` in the refusal, `dslimports.Load` reporting, the runbook upgrade order, the non-fatal unread manifest, the matrix doc claims, the `import ( ... )` block, the migrator and the lint root, concept annotations at parse time, stale docs and dead code.
- Done before the checkpoint:
  - `474aa3383` A1
  - `198c97b6a` A2
  - `defef8261` A7
- A6 and the rest were in progress. Read `final-fix-A-report.md`.

**B: editor.**

- Worktree: `/home/znas/memql-projects/epic-dsl-v1-final-editor`
- Branch: `wt/dsl-v1-final-editor`
- Items B1-B6: refused-domain diagnostics, the quick fix that writes `memql.toml`, the extension watching `**/memql.toml`, the notification, the CHANGELOG, the lockfile version.
- Done before the checkpoint:
  - `3845e0c58`
  - `ff8fd9604`
  - `ae68066d0`
- Read `final-fix-B-report.md`.

## Remaining steps, in order

1. **Finish A and B** from their reports: each unfinished item names what remains.
2. **Integrate.** Merge `wt/dsl-v1-final-engine`, then `wt/dsl-v1-final-editor`, into `epic/dsl-v1-foundations`. If A moved `parser.GrammarVersion` (item A8), move the pins: `editors/vscode/package.json` `memql.grammarVersion` and the grammar line of the CHANGELOG 0.4.0 section. `cmd/memql-lsp/editorparity_test.go` names anything stale.
3. **Run one scoped re-review** over `a919fa86f..<integrated tip>`, against the two briefs. Fix what it finds and adjudicate the rest.
4. **Send memql-10 the final tip** and `git diff --name-only a919fa86f..<tip> -- test/conformance`. This was promised: memql-10 is merging its `expr/` cells with ours on its flip branch.
5. **Delete** the plan and this file in the last commit.
6. **Bring main in.** `git fetch`, and merge `origin/main` if it moved. If `make arch-model-check` is red, take one side of `topology.model.json` and regenerate.
7. **Verify the final head.**
   - Run `verify-final.sh` against it.
   - Run the db-gated trees against a real Postgres. The private container `memql-b1-dbtest` is on port 55471; recreate it if gone:
     ```bash
     MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:55471/memql?sslmode=disable' \
       go test -count=1 -p 3 $(cat /tmp/.../scratchpad/dbpkgs.txt)
     ```
     If the list is gone, use `scripts/ci/db-gated-packages.sh --trees`. `component/packages` can hit a 10.5 s read timeout under `-p 3`; re-run it alone.
8. **Open ONE PR** against main. Take the body from `pr-body.md`, adding the fix wave:
   - the upgrade order now in the runbooks;
   - the editor quick fix and notification;
   - the retired `import ( ... )` block;
   - concept annotations checked at parse;
   - the new GrammarVersion, if it moved.

   Keep one `Closes #N` line per issue, each on its own line. After opening, check that `closingIssuesReferences` lists all seven.
9. **Watch CI, then merge** with `scripts/dev/merge-as-owner.sh --pr=<n>` (the bypass merges at once) or with bare `gh pr merge <n> --repo znasllc-io/memql`.
   - Never pass `--delete-branch` or `--merge`.
   - If the PR is `BEHIND`: `gh pr update-branch <n>`, then wait for CI again.
10. **After the merge:**
    - Check that #5356-#5362 are closed, and tick the epic's task list.
    - Comment on the four product adoption issues with the merge SHA, the command `memqlmigrate --rewrite=language-line -w <root>`, and the safe order: add the file first (earlier engines ignore it), redeploy staged packages, then roll the engine. The issues are:
      - memql-project#58
      - memql-znas#113
      - memql-fylo#12
      - memql-casera#2
    - Send memql-10 and memql-22 the merge SHA.
    - Fast-forward the local main in `/home/znas/memql-projects/memql`.
11. **Clean up:**
    - `git worktree remove` these, after merge only, and only once each is clean:
      - `epic-dsl-v1-{line,registry,editor,matrix,cells,final-engine,final-editor}`
      - the scratchpad `trial-merge`
      - `epic-dsl-v1-foundations`
    - Delete the local `wt/dsl-v1-*` branches and the remote `wt/dsl-v1-final-{engine,editor}`.
    - Remove memql-21's merged, clean scratchpad worktrees `wt-design` and `wt-dsl`, under `/tmp/claude-1000/-home-znas-memql-projects-memql/0b898739-fbc9-49c1-983d-875f7892040c/scratchpad/`. Their PRs #5354 and #5355 are merged.
    - Run `docker rm -f memql-b1-dbtest`.
    - Remove the SDD workspace.
    - **Do not touch** `dslv1-w-*`, `epic-dsl-v1-bodies` or `epic-dsl-v1-expressions`. They are memql-10's and memql-22's.
12. **Report to the owner:**
    - that it is done;
    - how to test it: `make vscode-install` or install the VSIX, and a local cluster via `make dev`;
    - the rulings;
    - the deploy order.

## Rulings a resumer must not re-litigate

The full list with reasons is in `progress.md`.

**Structure**

- The language-line resolver lives in `component/language/parser`, because `component/memql` would be a module cycle. `component/memql` re-exports it.
- A domain whose language line is refused is skipped WHOLE by every loader.
- The migrator's domain definition is the resolver's.

**Narrowings**

- An unknown top-level construct keyword is refused as `construct_unknown`.
- Logic and automation bodies refuse an unrecognised clause, and mutation bodies refuse a stray line. This is accepted.

**Registry and matrix**

- `@trigger(on=...)` is accepted as a synonym for `event=`, not retired.
- `@timeout`, `@retry`, `@idempotent`, `@audit` and `@async` are retired with their memql#989 hint.
- Matrix cells carry the argument-form word. Rows are alphabetical, and the "AI" family is named "Model access".
- The corpus gates read only their own subtree (`cells/` or `expr/`).

**Final review**

- Important 1: the engine side only (the runbooks, the PR body, the refusal's command). The four product-repo PRs are those repos' own adoption epics (`epic:dsl-v1-adoption`), not this session's. Merging changes no running cluster: cloud overlays pin engine digests, and no engine overlay mounts product DSL.
- Minor 7 (body-clause refusals carry no code) goes to epic 3, dsl-v1-bodies.
- Minor 8 is taken: concept annotations are checked in the parse Sense shares.
- Important 4 is taken with two additions: the watcher on `memql.toml`, and the quick fix.

## Other sessions

- **memql-10**: epic 2 (expressions), branch `epic/dsl-v1-expressions`. It adds tiers position `queryRefine` and is merging its `expr/` corpus cells with ours.
- **memql-22**: epic 3 (bodies), branch `epic/dsl-v1-bodies`, stacked on memql-10's. It owns the `statements/` corpus subtree and the `body_*` refusal codes. It edited four of our tests on its own branch, and we did not object.
- **memql-21**: owns the k3d cluster `memql`. Never touch it.

## Facts that bite

- Verify with `go test github.com/znasllc-io/memql/...` or `make test`, never with `go test ./...` from the root.
- Stage files by explicit path; never `git add -A`.
- Always run the root package unfiltered (`go test -count=1 .`). A doc edit can red its gates.
- A fresh worktree needs `bash scripts/identity/build-css.sh` before the root package builds.
- The extension's tests need `make vscode-deps` and `npm ci --prefix editors/vscode`.
