# Repo Cleanup & Docs Update Campaign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **This campaign is executed across multiple sessions via GitHub issues.** Each task below maps 1:1 to a task issue under the campaign epic. Before starting a task: claim its issue (assign yourself or comment), re-read this plan's section for it, and work in a git worktree. Do not start a task whose "Depends on" tasks have unclosed issues.

**Goal:** Bring every prose surface of the memql repo into agreement with the code, remove dead content and mechanically-dead functionality, leave drift gates behind, and record every bug found as a labeled issue under a findings epic.

**Architecture:** Audit-then-fix. Cross-cutting mechanical gates land first (their red output is audit input), a read-only audit produces a per-file ledger and per-lane work lists, then six fix lanes execute in parallel as claimable issues. Spec: `docs/superpowers/specs/2026-08-18-repo-cleanup-docs-update-design.md`.

**Tech Stack:** Go tests in `package main` at the repo root (the existing gate style), `git ls-files` enumeration, `component/memql/dslimports` for DSL snippet validation, `gh` for issues.

**Campaign tracking:** Epic A (campaign) #4087 · Epic B (findings) #4088 · T1 #4089 · T2 #4090 · T3 #4091 · T4 #4092 · T5 #4093 · T6 #4094 · T7 #4095 · T8 #4096 · T9 #4097.

## Global Constraints

Every task implicitly includes all of these.

- **Verification is `make test`, never `go test ./...`** (memql#4032: the bare pattern misses the engine's own modules). When running a package directly, add `-count=1` (the test cache serves stale PASSes). Root-gate runs: `go test -count=1 -run '<TestName>' github.com/znasllc-io/memql`.
- **CI is wider than local go test**: Makefile drift gates, JS lanes, `module-boundaries`. A green local run is not a green PR; watch `gh pr checks`.
- **Git workflow**: claim the issue first -> worktree (`git worktree add .claude/worktrees/<name> -b <branch> origin/main`) -> stage by explicit path (never `git add -A`) -> PR -> CI green -> `gh pr merge <n> --repo znasllc-io/memql` **bare** (no `--merge`, no `--delete-branch`; "already queued" is confirmation, not an error; a queued PR going `DIRTY` after a sibling lands needs rebase + force-push). Each closing PR carries its own `Closes #N` per issue; non-closing references use `Refs #N` (never "does not close #N").
- **New repo-sweeping tests enumerate via `git ls-files -z`** (the pattern in `lifecycle_docs_conformance_test.go`). A `filepath.WalkDir` without `core/repowalk.SkipDir` trips `TestRepoWalkersShareOneSkipList`.
- **Root gates carry the house comment style**: a long doc comment stating why the gate exists (issue refs), what it deliberately does NOT catch, and the false-positive escape hatch. Probe the real boundary before freezing patterns: run the candidate regex/walker over the tree, triage every hit, then commit the pattern (per `~` house rule; four review rounds on the lifecycle gate proved phrase lists fitted to known sites miss the next paraphrase).
- **`docs/superpowers/**` is exempt from every new gate** — specs and plans quote banned forms by design. `docs/public/reference/_generated/**` is machine-generated: exempt from front-matter, never hand-edited.
- **DOCS_STANDARD.md governs docs**: front-matter keys, the public/internal split, lifecycle (shipped planning doc -> delete after extracting follow-ups to issues; shipped design doc -> `status: historical` + banner). No emojis anywhere. `docs/public/language/memql.md` is `//go:embed`-ed (`docs/embed.go`) — do not move it.
- **Positioning**: memQL is *built on* a time-series memory graph, never "is a database" (TSL-load-bearing; `TestNoDatabaseProductClaims` sweeps every tracked file including README).
- **No behavior changes** except deletions meeting Task 8's dead-code bar. Bugs are filed, not fixed: `gh issue create` the moment a bug is confirmed, labeled (`bug`/`security` + area + `epic:cleanup-findings`), evidence quoted; add it to the findings epic's task list. Security findings additionally get flagged to the repo owner in the session summary.

---

### Task 1: Cross-cutting docs gates (front-matter + layout, relative links, front-door test command)

**Files:**
- Create: `docs_front_matter_test.go` (repo root, `package main`)
- Create: `docs_relative_links_test.go` (repo root, `package main`)
- Modify: `claude_md_test_command_test.go` (add a front-door sibling test)
- Modify: `README.md`, `CONTRIBUTING.md` (minimal command substitution only — full rewrite is Task 3)
- Modify: the ~14–19 `docs/**.md` files missing front-matter keys; `git mv` the `docs/`-root strays (`docs/ci-design.md`, `docs/ci-audit.md`, `docs/AGENTS.md`)

**Interfaces:**
- Produces: three green gates later tasks must keep green; the front-matter contract (six keys: `title`, `audience`, `status`, `area`, `sinceVersion`, `owner`; closed sets below); a clean `docs/` root layout Task 7 relies on.

- [ ] **Step 1: Probe front-matter reality** (expect ~14 files missing the block, ~19 missing at least one key):

```bash
for f in $(git ls-files 'docs/**/*.md' 'docs/*.md' | grep -v '^docs/superpowers/' | grep -v '_generated/'); do
  head -1 "$f" | grep -q '^---$' || echo "NO-BLOCK $f"
done
for k in title audience status area sinceVersion owner; do
  for f in $(git ls-files 'docs/**/*.md' | grep -v superpowers | grep -v _generated); do
    head -20 "$f" | grep -q "^$k:" || echo "MISSING-$k $f"
  done
done | sort | uniq -c | sort -rn | head -40
```

- [ ] **Step 2: Write `docs_front_matter_test.go`.** House comment first (cites DOCS_STANDARD §2, this campaign's spec, and the escape hatches). Two tests:

```go
// TestDocsFrontMatterConformsToStandard: every tracked docs/**.md
// (excluding docs/superpowers/** and docs/public/reference/_generated/**)
// starts with a `---` front-matter block carrying all six required keys,
// with closed-set values:
//   audience: public|internal|ops   status: stable|draft|historical
//   area: overview|concepts|language|ai|operate|build|cockpit|design|planning|ops
// TestDocsRootLayoutIsClosed: the only tracked *.md directly under docs/
// are DOCS_STANDARD.md and CLAUDE.md.
```

Enumerate with `exec.Command("git", "ls-files", "-z", "docs")`, parse the first block line-by-line (`key: value` split on first `:`), collect all failures before `t.Errorf` so one run lists every file. If Step 1's triage shows a key requirement is genuinely impractical (e.g. `sinceVersion` on historical design docs), amend DOCS_STANDARD.md §2 *explicitly in this PR* rather than silently weakening the gate — the standard and the gate must state the same rule.

- [ ] **Step 3: Run it red; fix every hit.** Missing keys: add truthful values (`owner: znas`; `sinceVersion`: the release the subject shipped in, or amend per Step 2; `status: historical` for shipped design docs you are certain about — uncertain ones stay as-is for Task 7). Strays: read each, then `git mv` per DOCS_STANDARD §3 (`ci-design.md`/`ci-audit.md` are CI design/audit records -> `docs/internal/ops/` unless reading them says `design/`; `AGENTS.md` -> wherever its content actually belongs, likely `docs/internal/`), and add front-matter to them.

- [ ] **Step 4: Run green**: `go test -count=1 -run 'TestDocsFrontMatter|TestDocsRootLayout' github.com/znasllc-io/memql`. Commit: `git add docs_front_matter_test.go <each moved/edited file>` + `git commit -m "test(docs): gate front-matter and docs-root layout on DOCS_STANDARD"`.

- [ ] **Step 5: Probe links, then write `docs_relative_links_test.go`.** Probe with a throwaway scan before freezing the regex. The test: for every tracked `*.md` at the repo root and under `docs/` (same exemptions), strip fenced code blocks (split on ```` ``` ```` fence lines, keep even-indexed segments), extract inline links/images with `\[[^\]]*\]\(([^)\s]+)\)`, skip targets starting with `http://`, `https://`, `mailto:`, `#`, or containing `://`; strip any `#fragment` suffix; resolve the rest relative to the file's directory and require the file to exist (`os.Stat`). Anchor validity within files is out of scope (document that in the comment). Collect-all-then-fail like Step 2.

- [ ] **Step 6: Run red; fix every dead link** (update the path or delete the sentence if the target is gone for a reason — check `git log --follow` on the missing target before deciding). Run green. Commit: `test(docs): gate in-repo relative links`.

- [ ] **Step 7: Extend the memql#4032 gate to the front door.** In `claude_md_test_command_test.go`, add `TestFrontDoorDocsDoNotTeachSingleModuleTestSweep`: read `README.md` and `CONTRIBUTING.md`; fail on any match of `go test\s+(-[^\s]+\s+)*\./(\.\.\.|component|cmd)` (the single-module sweep shapes). Comment explains the shared rationale with `TestDocumentedTestCommandCoversTheEngine` and why README/CONTRIBUTING get the simpler literal ban (no Testing-section structure to parse). Run red (README currently has multiple hits).

- [ ] **Step 8: Minimal mechanical substitution in README/CONTRIBUTING**: replace each `go test ./...` (and `go test -v/-cover ./...`) occurrence with `make test`, and nothing else — the full rewrite is Task 3. Run green. Commit: `docs: stop teaching the single-module test sweep in the front door (Refs memql#4032)`.

- [ ] **Step 9: Full check + PR.** `make test`, then push branch, open PR titled `test(docs): cross-cutting docs drift gates + mechanical conformance`, body `Closes #4089`. Enqueue with bare `gh pr merge`.

### Task 2: Full-surface audit + findings ledger

Read-only (plus issue-filing). No repo edits. Depends on: Task 1 merged (its gates and moves define the baseline).

**Files:** none in-repo. Working ledger in the session scratchpad; durable outputs are GitHub issue comments.

**Interfaces:**
- Produces: (a) the ledger, posted as comment(s) on this task's issue; (b) a per-lane work list posted as a comment on each of Tasks 3–8's issues (the lane executor's checklist); (c) Epic B child issues, filed immediately as found.

- [ ] **Step 1: Build the file inventory** (the completeness denominator — the ledger must end with one row per file):

```bash
git ls-files '*.md' | grep -v '^docs/superpowers/' | grep -v '_generated/' > /tmp/inventory-md.txt
git ls-files 'dsl/**/*.memql' > /tmp/inventory-dsl.txt
wc -l /tmp/inventory-*.txt
```

- [ ] **Step 2: Run the mechanical seeds.** (a) The Task 1 gates are green — their historical red lists are already fixed; note that in the ledger. (b) The retired-vocabulary grep, one term at a time, triaging every hit into the ledger (the term list, exemptions and all, is in the spec's "Retired-vocabulary grep list" section — partitions-as-tenancy, genesis/sealed envelope, staging/production-as-environments, `/admin/` app, `MEMQL_MASTER_KEY`-as-auth, `az acr build`, the `deploy VERSION=` make target, macOS-only hardware, retired DSL author forms, missing node types). Example shape:

```bash
git grep -n --heading -i "sealed envelope\|genesis" -- '*.md' '*.go' ':!docs/superpowers' ':!docs/internal/design'
```

Measure before judging: "staging" alone hits 31 public files — most will be legitimate (drafts staging, historical narration). Every hit gets a ledger row with a disposition; a term whose hits are all legitimate is recorded as gate-unsuitable (audit-only).

(c) DSL snippet probe: extract all fenced `memql` blocks (139 in `docs/public`, 2 in README) and run each through `go run ./cmd/memqllint <tmpfile>` (memqllint handles single-file targets; imports into namespaces absent from the lint root are treated as external and skipped, so authoring-form snippets with `use` lines validate). Record per-block: parses / fails(reason) / is-deliberately-retired / is-fragment. This table is Task 3+4's snippet work list.

- [ ] **Step 3: Fan out the judgment audit per lane** (parallel read-only subagents, one per lane, each given: its file list from Step 1, the verification-authorities table from the spec — Makefile/scripts for commands, workflows for CI, dslimports/dslgate for grammar, memql.proto + `component/server` for wire, envregistry manifest for env vars, deploy/k8s for topology — and the ledger row format). Every factual claim is verified against its authority, never against other prose. Ledger row: `file | claim (quoted) | reality (evidence: file:line or command output) | disposition`, dispositions `fix-in-lane-N | delete | flip-historical | add-gate | file-issue | verified-true`. A clean file gets one `verified-true` row.

- [ ] **Step 4: File findings as confirmed, not batched.** For each `file-issue` disposition: `gh issue create` with quoted evidence, labels (`bug` or `security` or `enhancement`, area label, `epic:cleanup-findings`), body starting `Part of #4088.`; then edit Epic B's task list to append `- [ ] #<n>`. Risky code deletions (fail Task 8's bar) and should-be-generated-docs opportunities are filed the same way.

- [ ] **Step 5: Post the outputs.** Per-lane work lists -> comment on each lane issue (checklist of `file: fix` items). Full ledger -> comment(s) on this issue, with the completeness check stated: `ledger rows >= inventory lines, every inventory file present`. Close this issue with a summary comment (counts per disposition).

### Task 3: README rewrite + root governance files (+ snippet/vocab gates at README scope)

Depends on: Tasks 1, 2. This is the campaign's headline surface — it ships before the wide public sweep.

**Files:**
- Rewrite: `README.md`
- Modify: `CONTRIBUTING.md` (align with branch+PR+merge-queue reality), `VERSIONING.md`, `COMPATIBILITY.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md` (fixes per Task 2's work list only)
- Create: `docs_memql_snippets_test.go`, `docs_retired_vocabulary_test.go` (repo root, `package main`, scoped to README here; Task 4 widens)

**Interfaces:**
- Consumes: Task 2's work-list comment on this issue; the snippet probe table.
- Produces: the two gate files Task 4 widens (keep the scope list a package-level `var snippetScope = []string{"README.md"}` / `var vocabScope = []string{"README.md"}` so widening is a one-line change); the fence-marker convention: bare ```` ```memql ```` = validated standalone; ```` ```memql fragment ```` = skipped (incomplete by design); ```` ```memql retired ```` = skipped (don't-do-this example). GitHub highlights by the first token, so marked blocks still render as memql.

- [ ] **Step 1: Write `docs_memql_snippets_test.go`.** Extract fenced blocks whose info string's first token is `memql` from every file in `snippetScope`; skip `fragment`/`retired`-marked blocks; write each survivor to `t.TempDir()/<n>.memql`; validate through the same single-file pipeline `cmd/memqllint/main.go` runs (read its file-target path first and mirror it — the core call is `dslimports.Load`, `component/memql/dslimports/dslimports.go`; if the logic is not cleanly importable from a test, shell `go run ./cmd/memqllint` and document why in the comment). Failures report file, block index, first diagnostic.

- [ ] **Step 2: Write `docs_retired_vocabulary_test.go`.** Data-driven:

```go
var retiredVocabulary = []struct{ pattern, reason, ref string }{
    {`(?i)partition-access|per-partition (isolation|scope)`, "partition tenancy retired", "memql#56"},
    {`(?i)management at .?/admin/`, "/admin/ app retired; portal owns admin", "memql#3943-era"},
    {`(?i)sealed (genesis )?envelope`, "superseded by component/envregistry", "memql#3963"},
    {`MEMQL_MASTER_KEY`, "master key decrypts; operator key authenticates — docs must not present it as a credential", "memql#3519"},
    {`az acr build|make release\b`, "hand-built release images superseded by the build server", "CLAUDE.md image-build rule"},
    // extend ONLY with patterns whose full current-tree hit list you have
    // personally triaged (probe first; see the gate comment).
}
```

Scope: `vocabScope` files only; `status: historical` front-matter exempts a file (read the block, check `status:`). The comment documents the probe-first rule and the escape hatch (reword, or in a historical doc, nothing — it is exempt). Patterns from the spec list that triage shows are unmatchable without false positives (likely `staging`) are recorded in the comment as deliberately absent, audit-only.

- [ ] **Step 3: Run both red against the current README** (its example uses `$args` interpolation and `payload.` prefixes; expect snippet failures).

- [ ] **Step 4: Rewrite README.md** per the spec's outline: keep lockup/badges/positioning line/alpha banner/"What is-Why" (tightened, TSL-safe); replace the DSL example with constructs lifted from the live `dsl/` tree (or authored fresh and proven by the snippet gate); Quick Start with `make up` / `make dev` / `make test`; prerequisites naming Linux and macOS both (docker, k3d, kubectl; note Apple-Silicon and linux/amd64 are both exercised); project structure matching today's tree (`app/ clients/ cmd/ component/ core/ dsl/ integrations/ deploy/ docs/ scripts/ sdk/`); development workflow = branch -> PR -> CI -> merge queue (quote the bare `gh pr merge` form); deploy section = build-server images + digest pin in the one cloud overlay + ArgoCD (drop the `deploy VERSION=X` make target if Task 2 confirmed it retired — check `grep -n "^deploy:" Makefile`); Auth/Environments/Testing/Local-Cluster sections collapse to a short paragraph + link each into `docs/public/`. Every remaining command must be one you ran during this task.

- [ ] **Step 5: Apply the Task 2 work list to the other five root files.** CONTRIBUTING.md gets the same branch+PR+merge-queue truth and `make test`.

- [ ] **Step 6: Verify.** `go test -count=1 -run 'TestDocsMemqlSnippets|TestRetiredVocabulary|TestFrontDoorDocs|TestNoDatabaseProductClaims' github.com/znasllc-io/memql` green; `make test` green; read the rendered README once top-to-bottom for narrative coherence. Commit in two: gates (`test(docs): validate memql snippets and retired vocabulary, README scope`), rewrite (`docs: rewrite README as a truthful front door; Closes #4091`). PR, enqueue.

### Task 4: docs/public sweep (+ widen the gates)

Depends on: Task 3 (it owns the two gate files this task widens).

**Files:**
- Modify: `docs_memql_snippets_test.go`, `docs_retired_vocabulary_test.go` (widen `snippetScope`/`vocabScope` to include `docs/public/**` — enumerate via `git ls-files` inside the test rather than a literal list)
- Modify: `docs/public/**` per Task 2's work list; `GLOSSARY.md`

**Interfaces:**
- Consumes: Task 2's work list + snippet table; Task 3's fence-marker convention.

- [ ] **Step 1: Widen `snippetScope`/`vocabScope`** to `docs/public/**` + README; run both gates red; reconcile the hit list against Task 2's snippet table (the table says which of the ~139 blocks fix vs. mark `fragment` vs. mark `retired` — a *wrong teaching example* is always fixed to current grammar, never marked).

- [ ] **Step 2: Work the lane list file-by-file** (batch commits per area: `overview/`, `concepts/`, `language/`, `ai/`, `operate/`, `build/`, `cockpit/`). For each file: apply the ledger fixes, re-verify any command you touch by running it, check front-matter *values* are honest (`status: stable` only if the content now is), keep the language reference consistent with `dsl/_reference/` (Task 5 owns that side — coordinate via the ledger, and where the two disagree the parser is the authority for both).

- [ ] **Step 3: GLOSSARY.md**: one entry per public doc, titles matching, no entries pointing at deleted/moved files (the Task 1 link gate enforces resolution; completeness is judgment — compare against `git ls-files 'docs/public/**/*.md'`).

- [ ] **Step 4: Verify + PR.** All docs gates + `make test` green. One PR (`docs(public): align the public tree with the engine; Closes #4092`), enqueue. If the sweep splits into multiple PRs, only the last carries `Closes`.

### Task 5: DSL tree doc surface

Depends on: Task 2.

**Files:**
- Modify: `dsl/**` doc-comments (`///`) and `_reference/*.memql` per the work list.

- [ ] **Step 1: `_reference/` skeletons.** For each of the five (`_concept`, `_shape`, `_spec`, `_trait`, `_agent`): the live-form sections must parse today (probe by copying live-form examples to a temp file and running `go run ./cmd/memqllint` on it), and the deliberately-retired sections must be explicitly labeled as retired forms (they are the sanctioned home for those spellings). Fix live-form drift; never "fix" a labeled retired example into live form — that destroys the lesson.

- [ ] **Step 2: Doc-comment sweep** over `dsl/**` per the ledger (claims about behavior, defaults, callers that are no longer true). Bare grammar is already gate-covered (`dslgate`, conformance) — this step is about the *prose* in `///` comments and `@description` strings.

- [ ] **Step 3: `@deprecated` inventory**: `git grep -n "@deprecated" -- 'dsl/**'`. Each hit either has a removal/decision issue under Epic B (file it if missing) or a comment naming why it stays. Remember `@disabled` is NOT deprecated (the `AttrEnabled`/`AttrDisabled` doc in `component/language/ast/ast.go` is canonical) — do not touch `@disabled` constructs.

- [ ] **Step 4: Verify + PR.** `make dsl-lint` green, `make test` green (the DSL conformance suite runs there). PR `dsl: truthful doc-comments and reference skeletons; Closes #4093`, enqueue.

### Task 6: CLAUDE.md accuracy pass

Depends on: Task 2. One focused PR; the root file is live instructions for running sessions.

**Files:**
- Modify: `CLAUDE.md`, `component/CLAUDE.md`, `component/architecture/CLAUDE.md`, `component/observe/CLAUDE.md`, `component/node/CLAUDE.md`, `integrations/CLAUDE.md`, `docs/CLAUDE.md`, `sdk/go/CLAUDE.md`

- [ ] **Step 1: Root CLAUDE.md, fact-fixes only** per the ledger — no restructuring, no section moves (a trim/restructure is an Epic B issue for the owner). Known candidates to verify hard: the "Development is standardized on macOS / Apple Silicon" hardware claim (primary dev box is linux/amd64 — rewrite to name both), stale counts (module/package numbers — re-measure with `go list -m | wc -l` before writing any number), any command you can run cheaply.
- [ ] **Step 2: The seven directory CLAUDE.md files** — verify each claim against its own directory's code (exports, file names, build tags).
- [ ] **Step 3: Verify + PR.** `make test` green (the CLAUDE.md test-command gate parses the Testing section — keep its structure). PR `docs(claude): fact-fix the CLAUDE.md tree; Closes #4094`, enqueue.

### Task 7: docs/internal lifecycle enforcement

Depends on: Task 2 (strays already relocated by Task 1).

**Files:**
- Modify/Delete: `docs/internal/**` per the work list.

- [ ] **Step 1: `internal/planning/`** — for each doc: shipped? (verify the feature in code, not by the doc's own claim). If shipped: extract still-live follow-ups into issues first (Epic B, or standalone with area labels if they are ordinary feature work), then `git rm` the doc. If in-flight: leave, but fix false claims.
- [ ] **Step 2: `internal/design/`** — shipped/superseded docs get `status: historical` + the one-line banner (`> Historical: shipped in X.Y.Z; kept for rationale.`); do not rewrite their content (the rationale is the value; historical docs are vocab-gate-exempt for exactly this reason).
- [ ] **Step 3: `internal/ops/`** — runbooks: verify referenced commands/paths still exist (`make -n <target>`, `ls` the scripts); fix or file.
- [ ] **Step 4: Verify + PR.** Docs gates + `make test` green. PR `docs(internal): enforce the DOCS_STANDARD lifecycle; Closes #4095`, enqueue.

### Task 8: Go comment sweep + mechanically-dead code

Depends on: Task 2.

**Files:** per the work list (comment fixes across the Go tree; deletions only where the bar is met).

- [ ] **Step 1: Comment fixes** per the ledger (comments stating now-false facts). Do not restyle comments that are merely verbose; this lane is truth, not taste.
- [ ] **Step 2: For each dead-code candidate, apply the bar** — ALL of: (a) `git grep -n "<symbol>"` across the whole workspace shows no non-test reference outside its own declaration; (b) no build-tag-gated consumer: check every node-type build compiles after removal — `for tag in "" bff voice cognition agent planner edge workbench mcp identity; do go build ${tag:+-tags $tag} ./... || break; done` run from the owning module, plus `make test`; (c) it is not a registration reached by side effect (`init()`, `RegisterPlugin`, `RegisterRoutingRule`, DSL `@executor` strings — grep the symbol's string form too); (d) no `.memql` or manifest references it (`git grep` across `dsl/`, `deploy/`). Anything failing any prong: file under Epic B instead, with the evidence gathered.
- [ ] **Step 3: Delete survivors one candidate per commit** (revertability), `make test` after each.
- [ ] **Step 4: Verify + PR.** `make test` + full CI. PR `chore: remove mechanically-dead code and false comments; Closes #4096`, enqueue. Wire-contract touches are not expected in this lane; if one somehow appears, stop and re-scope — that is not cleanup.

### Task 9: Campaign close-out

Depends on: Tasks 1–8 all closed.

- [ ] **Step 1: Re-run every campaign gate** by name plus `make test` on a fresh `origin/main` checkout; confirm green.
- [ ] **Step 2: Ledger completeness** — confirm the Task 2 issue carries the final ledger and its inventory check; append a closing comment with final counts (files audited, fixed, deleted, flipped historical, issues filed).
- [ ] **Step 3: Epic hygiene** — every Task 1–9 box checked on Epic A; Epic B's task list matches the issues labeled `epic:cleanup-findings` (`gh issue list --label epic:cleanup-findings`); cross-links present both ways. Close Epic A with a summary comment. Epic B stays open and drains via the normal loop (security first).
