# Docs for their readers -- Implementation Plan

> **For agentic workers:** carry this plan out task by task, one pull request per task. Steps use checkbox (`- [ ]`) syntax for tracking. This plan is deleted in the epic's last merge.

**Goal:** a reader who arrives at `README.md`, the docs home or memql.io meets one positioning sentence and one license claim, rendered from constants; every public page declares what it is and every fact on it resolves against the code; the evaluator's path is six pages long; the contributor material leaves `CLAUDE.md` for a public `contribute/` area; the event kind strings match the proto (epic memql#5720, tasks #5721 to #5725).

**Architecture:** constants in `core/positioning`, written into pages by a test-as-renderer (the `deprecation_window_docs_test.go` pattern); front-matter keys and a facts gate in the root package; new areas added to the closed set and to the bundler without renaming the old ones (D3 Phase 1).

**Tech stack:** Go 1.26 workspace; root-package markdown gates; the TypeScript SDK, MemQL OS and the VS Code extension for the kind rename.

**Spec:** `docs/superpowers/specs/2026-09-27-documentation-program-design.md`, section 3 (D3, D4, D5, D9, D17, D18, D21, D22, D23) and section 4, epic 2. Read it before any task.

## Global constraints

- One pull request per task, titled from the task, body ending `Closes #<task>`. The first branch is `epic/docs-readers`; later tasks use `epic/docs-readers-<task>` when they run beside an open one.
- Scratch worktrees of `origin/main`; stage by explicit path; commit messages through a file. `make identity-tailwind` in a fresh worktree; `make arch-model` last after any Go change.
- A docs change also runs the `docs/public` readers outside the root package when it touches `language/`, `concepts/` or the proving pages (`component/language/...`, `test/conformance/...`, `component/proving/...`).
- Customer names and cost text belong to the docs-safety epic; the cockpit area belongs to the docs-machines epic. The engine stays product-neutral.

---

## Decisions this plan makes

1. **The Kind column keeps the literal `si_*` strings until #5725 (#5721).** Upper-case `SI_COMPLETION_*` names neither the proto enum nor what `Kind.String()` sends, so `events.md` prints the lower-case wire string. The vocabulary gate refuses only the upper-case form until #5725 renames the kinds.
2. **"Portal" stays only where it names something else (#5721).** Third-party portals and the reserved hostname label keep the word, with the gate's reasoned line marker. Every other mention outside a historical page goes.
3. **The bundler strips HTML comments (#5721).** Gate markers are for the tree, not the site. The comment strip moves into the `cmd/docs-gen` bundler with bundle contract v2 (#5717).
4. **Pages move once, in Phase 1 areas (#5723, #5724).** `architecture/` and `contribute/` are added; `overview/`, `concepts/` and `operate/` keep their names until Phase 2, one release after the visit.
5. **The threat model waits for the owner (#5724).** `auth-threat-model.md` is published only after the owner's redaction pass, and the pull request asks before it ships the page.

## Tasks

### PR 1 -- #5721 the prose truth sweep

- [x] Deploy control is the VS Code Deployments panel over `DeployControlService`, described as the panel draws it.
- [x] `events.md` prints the `ai.completion.*` topics; `why-memql-harness.md` drops the retired bullets and says hop budget.
- [x] The proving log and its assets deleted; "portal" out of non-historical pages; hand "Last Updated" lines gone.
- [x] Dead variables, an absent script, stale pack paths, Cockpit framing, carrier-repo pointers and the brand spelling fixed.
- [x] Seven retired-vocabulary patterns, each probed against the whole scope, with a test pinning what they catch.

### PR 2 -- #5722 core/positioning, the page contract, the facts gate

- [ ] `core/positioning` with the sentence and `LicenseClaim`; `docs_positioning_test.go` renders them into `README.md`, `what-is-memql.md`, `CLAUDE.md` line 1, the VS Code site, the bundle manifest and the compliance note.
- [ ] `README.md` led by the why; "5 minutes" retired.
- [ ] `description`, `kind` and `exposure` on every public page; `docs_facts_test.go` resolves env vars, node types, topics, capabilities, make targets and OS app names.
- [ ] Drafts, hand dates and footers refused under `docs/public`; `status` required on every design record; the proving caveat required on every proof claim.

### PR 3 -- #5723 the evaluator's path

- [ ] `overview/how-memql-is-organized.md` and `overview/maturity.md`.
- [ ] The `architecture/` area (index, nodes, mesh, parity, api-surface, engine-and-products, component-bus; modules and build-tags moved in).
- [ ] `concepts/architecture.md` cut to a hub; the compiler tour to `docs/internal`; every inbound link updated.

### PR 4 -- #5724 audience moves and promotions

- [ ] `contribute/` from `CLAUDE.md`'s workflow, testing, image-build and branch rules, with the moved operate pages; `CLAUDE.md` links instead of restating.
- [ ] `build/product-repo.md`, `language/argument-resolution.md`, the dependency-tree page, `language/actor-envelope.md`, `build/forge.md`.
- [ ] Internal-only pages demoted; the design records of D22 promoted; the threat model only after the owner's pass.
- [ ] `GLOSSARY.md` regenerated from descriptions; `DOCS_STANDARD.md` sections 1 to 4 final.

### PR 5 -- #5725 the kind rename

- [ ] `Kind.String()` returns `ai_event` and `ai_completion_*`; the step parser, the `eventKind` stamp, `events.md` and the `clients/os`, `sdk/ts`, `sdk/go` and `editors/vscode` consumers in one commit, with the frontend note in its body.
