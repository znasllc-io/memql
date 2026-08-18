---
title: Repo Cleanup & Docs Update Campaign (pre-MVP)
audience: internal
status: design approved; ready for implementation planning
area: design
date: 2026-08-18
owner: znas
surface: whole repo (docs, root files, CLAUDE.md files, DSL tree, Go comments)
---

# Repo Cleanup & Docs Update Campaign

Bring every prose surface of the memql repo into agreement with what the code
actually does today, remove content and functionality that is dead, and leave
drift gates behind so the failure classes fixed here cannot silently return.
Bugs discovered along the way are recorded as GitHub issues under a findings
epic -- not fixed in this campaign.

## Context and driver

The repo is public and approaching MVP; outside evaluators, users, and
contributors will read it. Meanwhile the docs have drifted hard from the code.
Confirmed examples before any audit ran:

- `README.md` instructs `go test ./...` as the verification command -- the
  exact confidence-increasing trap memql#4032 exists to prevent (it misses the
  engine's own modules entirely).
- `README.md` and likely `CONTRIBUTING.md` instruct committing directly to
  `main`, which the repository ruleset refuses (`push declined due to
  repository rule violations`).
- `README.md` documents macOS/Apple Silicon as the only development hardware;
  primary development currently happens on linux/amd64.
- `README.md` documents "centralized user / partition-access management at
  `/admin/`" -- partitions are retired (#56) and `/admin/` answers `410 Gone`.
- The README's flagship DSL example uses forms (`$args` interpolation,
  `payload.` prefixes, `@version`/`@namespace` on a concept, `@default` on
  concept fields) that do not match the documented current grammar.
- `docs/ci-design.md`, `docs/ci-audit.md`, and `docs/AGENTS.md` sit loose at
  the `docs/` root, violating DOCS_STANDARD's own layout.

DOCS_STANDARD SS4 already states the rule this campaign enforces: "A doc must
not contradict the code."

Decisions taken during brainstorming (2026-08-18):

| Decision | Choice |
|---|---|
| Audience priority | Both internal and external accuracy, public-facing surfaces first |
| Deprecated/dead code | Split by risk: delete only the mechanically verifiable; file the rest |
| Tracking | Two epics (campaign work vs. audit findings) |
| Drift gates | In scope; built early, in the repo's existing gate style |
| Go comment sweep | Targeted factual sweep, not line-by-line |
| Overall shape | Audit first, then fix in lanes |

## Goals

1. Every factual claim on the audited surface is verified against code (or
   removed / corrected / flagged), with evidence recorded.
2. README is rewritten as a truthful front door.
3. DOCS_STANDARD lifecycle is actually enforced over `docs/` (front-matter,
   layout, historical flips, shipped-planning-doc deletion).
4. Stale comments stating now-false facts are gone from the Go and DSL trees.
5. Mechanically-dead code is deleted; risky deletions are filed.
6. Every bug found is a labeled GitHub issue under the findings epic.
7. Gates exist so the mechanical drift classes stay fixed.

## Non-goals (out of scope)

- New features, and fixing any bug found (file only).
- `docs/public/reference/_generated/` -- machine-generated at release; never
  hand-edited.
- `docs/superpowers/` -- point-in-time working records of this process.
- Other repos (memql-cockpit, product template, website). `docs/public/cockpit/`
  in this repo IS in scope.
- Restructuring/trimming the root CLAUDE.md (142KB). Its lane is fact-fixes
  only; a restructure is a design decision filed as a findings-epic issue.
- Docs release/publish work. Public docs reach memql.io at the next release
  bundle; the repo getting clean now is the deliverable.

## Scope: the six lanes

| Lane | Surface | Work |
|---|---|---|
| 1. Root front door | `README.md`, `CONTRIBUTING.md`, `VERSIONING.md`, `COMPATIBILITY.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md` | Full README rewrite; accuracy pass on the rest |
| 2. Public docs | `docs/public/**` (~85 files) + `GLOSSARY.md` | Verify every factual claim; front-matter compliance; DSL examples match current grammar; GLOSSARY index complete and correct |
| 3. Internal docs | `docs/internal/**` (~64 files) + `docs/`-root strays | Lifecycle enforcement: shipped planning docs deleted (live follow-ups become issues first, per DOCS_STANDARD), design docs flipped `status: historical`, strays relocated or deleted |
| 4. CLAUDE.md files | root + `component/`, `component/{architecture,observe,node}/`, `integrations/`, `docs/`, `sdk/go/` | Accuracy pass; these steer every Claude session, so a wrong claim multiplies |
| 5. DSL tree | `dsl/**` doc-comments, `_reference/` skeletons, `@deprecated` constructs | Skeletons match the current parser; doc-comments truthful; retired forms survive only where deliberately kept as don't-do-this examples |
| 6. Code | Go tree (vocabulary grep covers all tracked text) | Targeted factual comment sweep; mechanically-dead code deleted; risky deletions filed |

## GitHub structure: two epics

**Epic A -- "Repo cleanup & docs update (pre-MVP)".** Labels: `epic`,
`type:epic`, `documentation`, `audit`. Task issues:

- Task 0: mechanical gates + full audit + findings ledger.
- One task per lane (1-6).
- One task for the cross-cutting drift gates that are not owned by a single
  lane (front-matter lint, link check, snippet validation).

Task issues are claimable across sessions (claim on GitHub before starting
work). The epic closes when all its tasks ship.

**Epic B -- "Cleanup audit findings".** Labels: `epic`, `type:epic`. Receives
every bug, risky deletion, and should-be-generated-doc opportunity the audit
surfaces. Each child issue is labeled individually (`bug`, `security`, area
labels). Epic B stays open after the campaign and drains through the normal
ship-issue loop; `security`-labeled issues jump that queue.

Issue hygiene: each closing PR carries its own `Closes #N` per issue (a list
after one keyword closes only the first); non-closing references use
`Refs #N`, never "does not close #N" (the negation is ignored).

## Audit method

### Verification authorities

Ground truth comes from code, never from other prose. A claim in one doc is
never verified by citing another doc or a code comment.

| Claim about | Verified against |
|---|---|
| Commands / workflows | `Makefile`, `scripts/**`, and running the safe ones (`make -n`, `--help`) |
| Test & CI reality | `.github/workflows/**` + `scripts/ci/**` (CI is wider than `go test`: drift gates, JS lanes, module-boundaries) |
| DSL grammar & examples | The real parser: fenced `memql` blocks extracted and fed through `cmd/memqllint` / the loader; fragments that cannot stand alone checked construct-by-construct against `component/memql/dslgate` + conformance rules |
| Wire surface | `component/grpc/memql.proto`, `component/server` path declarations, the front-door generators |
| Env vars | The `component/envregistry` manifest |
| Topology / deploy | `deploy/k8s/**` + the k3d scripts |

### Retired-vocabulary grep list

Seeds the mechanical half of the sweep. Built from the repo's recorded
reversals; every term is confirmed in code before anything is judged by it
(CLAUDE.md is itself an audited surface), and terms with legitimate survivors
carry explicit exemptions.

Seed list (task 0 refines it):

- Partition as tenancy (#56). Exemption: the `partition="*"` automation kwarg
  remains required until #56 phase 8.
- Genesis / sealed envelope (superseded by `component/envregistry`, #3963).
- Staging/production as an environment dimension; `if env ==` branches
  (#3943; engine code already gated by `TestNoEnvironmentBranchingInEngineCode`).
- `go test ./...` as a verification command (#4032).
- Commit-directly-to-`main` guidance (ruleset refuses it).
- The `/admin/` management app (410 Gone; the portal owns admin surfaces).
- Retired DSL author forms: `func (Query|Mutation|...)` receiver wrapping,
  the `@use*` family, `@concepts(...)` / `@shape("...")` bindings, `@input`,
  `include` in shapes, `;`-AND / `,`-OR separators, `has`, `?.`, `ctx.`,
  `@description` / `@default` on args fields, `coalesce()` longhand,
  `EvaluatePolicy` / decision-policy tier. Exemption: `dsl/_reference/`
  keeps retired forms deliberately as don't-do-this skeletons.
- Superseded deploy paths: `az acr build`, `make release`, hand-pushed release
  images, `make deploy VERSION=X` (verify whether the target still exists).
- macOS/Apple Silicon as the standardized dev hardware (verify current
  reality; the root CLAUDE.md may be the stale surface here).
- `MEMQL_MASTER_KEY` used as an authentication credential (#3519 split it
  from `MEMQL_OPERATOR_KEY`).
- Two-environments-in-one-cluster (#3748, reversed by #3943).
- Node-type lists missing `identity` / `edge` / `workbench` / `mcp`.
- "memQL is a database" phrasing (existing gate `TestNoDatabaseProductClaims`;
  positioning is TSL-load-bearing).

### The ledger

Every audited file gets a row -- including "verified clean" -- so thoroughness
is measurable. A finding row is:

```
file -> quoted claim -> reality (evidence: file:line or command output) -> disposition
```

Dispositions: `fix-in-lane-N` | `delete` | `flip-historical` | `add-gate` |
`file-issue(Epic B)` | `verified-true`.

The ledger is a working file during the audit; its durable record is posted to
the Task 0 issue on completion (DOCS_STANDARD: ephemera live in issues, not in
`docs/`).

### Findings protocol

A bug -- code behaving wrongly against its own spec/gate/doc, a risky
deletion, a security observation -- is filed the moment it is confirmed, not
batched: evidence quoted in the body, labeled, attached under Epic B.

Docs-vs-code conflicts default to "the doc is wrong" only after the code's
behavior is confirmed intended (gates, tests, issue history). Where the code
looks wrong, that is an Epic B bug, and the doc is annotated to match current
reality in the meantime. Security findings are additionally flagged to the
repo owner directly.

## README rewrite outline

The README's job is to be a truthful front door that routes people onward, not
a mirror of the docs.

1. Keep: lockup, badges, one-line positioning, honest alpha status banner,
   the "What is / Why" prose (tightened). Positioning stays inside the TSL
   guardrail: *built on* a time-series memory graph, never "is a database".
2. Replace the DSL example with constructs that verify against the current
   parser -- ideally lifted from the live `dsl/` tree (DOCS_STANDARD: real
   identifiers, not placeholders).
3. Every command copy-paste-true on a fresh clone: `make test` (never
   `go test ./...`); prerequisites reflecting Linux + macOS reality; branch +
   PR + merge queue; deploy section describing build-server images +
   digest-pin + ArgoCD.
4. Shrink by delegation: Auth, Environments, Testing, and Local Cluster
   sections collapse to short accurate summaries plus links into
   `docs/public/`. Project structure updated to today's tree (`clients/`,
   `cmd/`, `core/`, `scripts/` are currently missing).

## Gates

Built early, as audit tooling: each mechanical gate is written first and run
red over the current tree; its output enumerates part of the ledger; lanes fix
to green; the gate stays. Build each gate by probing the real boundary first
(run the actual parser/matcher/walker over the real tree before freezing
assertions).

| Gate | Catches | Scope |
|---|---|---|
| Test-command coverage extended to `README.md` + `CONTRIBUTING.md` (extends the memql#4032 gate) | `go test ./...` reappearing in the front door | Lane 1 |
| Front-matter + layout lint | Missing/invalid `audience`/`status`/`area`/`sinceVersion`/`owner`; strays at `docs/` root; unknown areas | all of `docs/` (superpowers tree exempt) |
| Relative-link resolution | In-repo markdown links pointing at moved/deleted files | `docs/**` + root md files |
| DSL snippet validation | Fenced `memql` blocks that no longer parse; explicit opt-out marker keeps don't-do-this examples possible | `docs/public/language/**` + README |
| Retired-vocabulary sweep | Curated banned-terms list with explicit per-term exemptions | README + `docs/public/` only; `status: historical` docs exempt |
| Positioning claims | Existing `TestNoDatabaseProductClaims`; verify it covers README, extend if not | root + public |

The judgment half of the audit (is this factual claim still true?) cannot be a
gate and stays manual.

## Execution

### Ordering

1. Create Epic A + Epic B + task issues immediately after plan approval (the
   backlog is currently empty; these become the only open issues).
2. Task 0: build the mechanical gates (red), fan out the read-only audit
   (parallel subagents per lane, shared method doc), produce the ledger, file
   Epic B issues continuously.
3. Fix lanes, public-first: root front door -> `docs/public` -> DSL tree ->
   CLAUDE.md files -> `docs/internal` -> code comments + dead-code deletions.
   The audit resolves all facts up front, so lanes are fix-only and
   parallelizable across sessions.

### Per-lane workflow

Claim the issue on GitHub -> worktree branch -> PR -> CI green ->
`gh pr merge <n>` bare (merge queue; enqueue-not-merge; watch for `DIRTY`
after sibling merges and rebase). Stage files by explicit path. Local
verification is `make test` with `-count=1`; CI's drift gates are the real
surface.

### Constraints

- The root CLAUDE.md is both an audited surface and the live instructions for
  every running session: its lane is fact-fixes only, one focused PR.
- "Mechanically dead" has a high bar in a 49-module workspace with build
  tags: unreferenced across all workspace modules and all build-tag
  combinations, with gates green after removal. Anything short of the bar is
  an Epic B issue.

## Done criteria

- Every Epic A task issue closed; Epic A closed.
- All new gates green in CI.
- Ledger posted on the Task 0 issue covering 100% of the enumerated surface
  (every file has a row, even "verified clean").
- Epic B populated, cross-linked from Epic A, and left open to drain through
  the normal loop.

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Retired-vocab gate false positives | Curated per-term exemptions; `status: historical` docs exempt; probe the matcher over the real tree before freezing it |
| Snippet gate brittle on fragments | Explicit fragment/opt-out marker convention, decided in the implementation plan |
| Deleting code a build tag still compiles | The mechanical bar above; workspace-wide reference search (never single-module `./...`); anything uncertain filed instead |
| CLAUDE.md edits destabilize concurrent sessions | One focused fact-fix PR; no restructuring |
| Concurrent feature work racing docs PRs | Lanes are mostly file-disjoint; merge-queue DIRTY watch; claim-first convention |
| Audit misses a surface | Ledger completeness rule (a row per file) is checked against a generated file inventory, not memory |
