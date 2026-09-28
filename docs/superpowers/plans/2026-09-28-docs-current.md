# Docs current -- Implementation Plan

> **For agentic workers:** carry this plan out task by task, one pull request per task. Steps use checkbox (`- [ ]`) syntax for tracking. This plan is deleted in the epic's last merge.

**Goal:** every engine release publishes `docs-<X.Y.Z>.tgz`, and memql.io serves that release's set and nothing else, with no hand step between the tag and the site (epic memql#5712, tasks #5713 to #5719).

**Architecture:** the engine builds the bundle at the release tag (`publish-docs-bundle.yml`) and attaches it to the release. The instance repository that hosts the public site (`memql-znas`, `clients/memql-website`) replaces its one docs set with that asset, builds, and commits to its `main`; its Deployables auto-run publishes the static site. Two checks close the loop: the engine's weekly `scripts/docs/current-check.sh`, and the sync job's live version probe.

**Tech stack:** Go 1.26 workspace (root, `core`, `integrations` modules); bash capability scripts on `scripts/lib/capability.sh`; GitHub Actions; the site is Next.js 16 as a static export served by the engine edge.

**Spec:** `docs/superpowers/specs/2026-09-27-documentation-program-design.md`, section 3 (D1, D12, D13) and section 4, epic 1. Read it before any task.

## Global constraints

- One pull request per task, titled from the task, body ending `Closes #<task>`. Engine branches are `epic/docs-current` (PR 1) and `epic/docs-current-<task>` for the tasks that run beside it; the instance branch is `epic/docs-current` in `memql-znas`.
- Work in scratch worktrees of `origin/main`, never in a shared checkout. Stage by explicit path. Commit messages through a file and `git commit -F`.
- A fresh engine worktree runs `make identity-tailwind` before root-package tests. Any Go change ends with `make arch-model`.
- `/.github/` is code-owned by the lead developers; the owner merges with `scripts/dev/merge-as-owner.sh`, which refuses a branch behind `main`.
- The engine stays product-neutral: no downstream product, customer or operator domain names, here included.

---

## Decisions this plan makes (from the record and the code, made concrete)

1. **VERSION equals the tag through a prep pull request, and the cut refuses otherwise (#5714).** `main` refuses direct pushes, so no cut can write a release commit. `VERSION` reaches `main` through a normal pull request before the cut, and both the Go cut (`integrations/release`) and `scripts/release/release-engine.sh` refuse with `version_file_stale` when `VERSION` at the commit being tagged differs from the tag. The bundle build refuses the same mismatch. The v0.21.x and v0.22.10-12 tags, whose `VERSION` lagged, can no longer be backfilled. That is accepted, because D1 serves the current release only.
2. **The site uses trailing slashes (#5715).** The acceptance paths (`out/docs/index.html`, `out/docs/overview/quickstart/index.html`) need `trailingSlash: true`. Every docs URL becomes `/docs/<slug>/`. The old URLs get one release cycle of `noindex` meta-refresh stubs, written as files, because the edge serves files and cannot redirect.
3. **Links leaving the bundle render as text until contract v2 (#5715, #5717).** The site's stopgap rewrites in-bundle `.md` links to routes and renders the rest unlinked. Contract v2 moves the rewrite into the bundle and fails the build on a link that leaves `docs/public`.
4. **The old sync workflow goes with the cut (#5715).** It reads files the cut deletes and could only push orphan branches. Its replacement is #5716.
5. **The sync job's writes need a GitHub App (#5716, owner action).** The instance ruleset has no bypass actor, and neither `GITHUB_TOKEN` nor any installed App can push to `main` or send `repository_dispatch`. The job mints an App token from `vars.DOCS_SYNC_APP_ID` and `secrets.DOCS_SYNC_APP_PRIVATE_KEY`, and fails loudly whenever a sync is needed and the App is absent. The owner creates the App, installs it on the instance repository only, and adds it as the ruleset's bypass actor (D13). The App-token pull request is the fallback only.
6. **Contract v2 ships after the site reads it (#5717).** Dropping `nav` from the manifest breaks the current site readers. So the v2 manifest keeps a derived `nav` until the site reads `pages` (#5718). The `nav` saves the sidebar only. A v2 page links other pages by absolute route (`/docs/<slug>/`), and the site's stopgap reader (`resolveBundleHref`, from #5715) resolves only relative `.md` links and renders every other in-docs href as plain text. So the first engine release cut after #5717 merges would unlink every cross-page link on memql.io. **#5718 merges before that release**, or at the least a reader change that follows `/docs/` hrefs does; and the #5716 sync refuses a manifest `schema` the site does not read, so a release cut out of order cannot reach the site.

## Tasks

### PR 1 -- #5713 the verify step, the guard, the backfill

- [x] The verify step greps a captured listing (`publish-docs-bundle.yml`).
- [x] `scripts/release/workflow_tar_pipe_test.go` refuses `grep -q` on a tar pipe in any workflow or composite action, with a detector test and a seeded check.
- [ ] Merged; `gh workflow run publish-docs-bundle.yml -f version=0.23.5`; `gh release view v0.23.5` lists `docs-0.23.5.tgz`.

### PR 2 -- #5714 VERSION equals the tag

- [x] `version_file_stale` in the Go cut and in `release-engine.sh`, dry runs included, documented in `release-cutting.md` section 7.
- [x] `build-docs-bundle.sh` refuses a version that is not `X.Y.Z` or differs from `VERSION`; `engineVersion` leaves the manifest and `DOCS_STANDARD.md`.
- [x] `scripts/docs/current-check.sh` (capability `docs.currentCheck`) with fake-`gh` tests, and a weekly `docs-current-check.yml`.
- [x] `VERSIONING.md` states the new meaning of `VERSION`; the deprecation-window version test follows it.

### PR 3 -- #5715 the site cut to current-only (memql-znas)

- [ ] One `src/app/docs/[...slug]` route over `src/content/docs/current/`; the docs home renders `overview/index.md`.
- [ ] The `memql-docs-version` meta on every docs page and the version in the chrome.
- [ ] The version registry, per-version trees, sync scripts, `install.sh`, site-authored pages and the dropdown deleted.
- [ ] Stubs for `/docs/latest/*` and `/docs/0.21.25/*`; Get started to `/docs/overview/quickstart/`.
- [ ] The 0.23.5 bundle unpacked byte-for-byte; `npm run typecheck` and `npm run build` green; the output assertions pass.

### PR 4 -- #5716 the sync job (memql-znas)

- [ ] Dispatch, hourly and manual triggers; the newest release's asset replaces `current/` wholesale; typecheck and build in the job.
- [ ] Refuse a manifest whose `schema` the site does not read yet (schema 2 until #5718 merges), failing loudly (decision 6).
- [ ] The coherence check against `ENGINE_REF`; the ten-minute live probe.
- [ ] The orphan `docs-sync/0.22.8` branch deleted.
- [ ] One run brings memql.io to the current release, or the deployable is run by hand and the auto-run gap filed.

### PR 5 -- #5717 bundle contract v2

- [x] A `cmd/docs-gen bundle` subcommand replaces `_bundle.py`: public, stable, engine selection; slug links; a link leaving `docs/public` fails; `llms.txt`, `llms-full.txt`, a sitemap fragment, the version file; `--check`.
- [x] `build-docs-bundle.sh` becomes a thin capability script; `docs_public_boundary_test.go` and `docs_bundle_build_test.go`.
- [x] Links read the way CommonMark reads them (titles, angle brackets, nested images, code across lines), held to goldmark by `docs_links_commonmark_test.go`.
- [x] `DOCS_STANDARD.md` section 5 and the `VERSIONING.md` documentation section rewritten.
- [ ] Not released before #5718 merges (decision 6).
- [ ] The `repository_dispatch` step once the App exists (decision 5).

### PR 6 -- #5718 site copy and search (memql-znas)

- [ ] Home, /about, /cockpit and the README from `what-is-memql.md`; the L1 diagram; Mermaid; `/llms.txt`; a search index; the brand gate with the verbatim proving caveat.
- [ ] The manifest `pages` reader, merged before the first engine release after #5717 (decision 6): in-page `/docs/<slug>/` hrefs followed, `nav` no longer read.
- [ ] The generated badge read from `pages[].generated`, not from `_generated` in the path: the concept catalog moved from `reference/_generated/concepts.md` to `reference/concepts.md` (#5717).
- [ ] A `moved` entry in `scripts/legacy-docs-slugs.json`, `reference/_generated/concepts` to `reference/concepts`, so the catalog's old URL stubs to the page rather than to `/docs/`.

### Housekeeping -- #5719 (no code PR)

- [ ] The predecessor site repository archived with a README pointer, its deploy workflow retired, its open issues closed as superseded.
- [ ] The cockpit repository description changed the day PR 6 merges.
- [ ] The dispatch answer recorded on the epic: no installed App covers `repository_dispatch` (decision 5).

## Owner actions this epic needs

- Merge each pull request with `scripts/dev/merge-as-owner.sh` in both repositories.
- Create the docs-sync GitHub App (contents write, installed on the instance repository only), store its id and key as `DOCS_SYNC_APP_ID` and `DOCS_SYNC_APP_PRIVATE_KEY` there and on the engine for the dispatch step, and add it as the instance ruleset's bypass actor.
- Confirm Deployables auto-deploy is on for the instance package, or run the site's deployable after each merge until it is.
