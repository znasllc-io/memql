# Cutting a MemQL release from MemQL itself -- owner-gated, audited, riding the existing CI cascade

- **Date:** 2026-08-23
- **Status:** approved (autonomous run: recommendations selected by Claude at the owner's standing instruction)
- **Sub-project N** of the 2026-08-23 backlog brief (VS Code + install/release batch)
- **Owner ask:** "We need to create an automation using MemQL to cut a new
  version of MemQL itself -- only the owners (role) have permissions to cut a
  new version."

## Current release mechanics (verified 2026-08-23)

The cascade already exists; cutting is the only manual half:

1. A human creates git tag `vX.Y.Z` and publishes a **GitHub Release** for it.
2. `.github/workflows/dispatch-engine-images-on-release.yml` (#2519) fires on
   `release: published` and re-dispatches `build-engine-images.yml` with the
   BARE version (it strips the `v` -- memql#4061's two-conventions rule: git
   tags carry `v`, image tags do not).
3. `build-engine-images.yml` (workflow_dispatch on main, GitHub OIDC -> ACR
   `acrmemql`; installs pull the same build from `ghcr.io/znasllc-io`,
   `stackPin.ts:235`) builds every node type as product-agnostic images,
   release tags immutable.

Other facts that bound the design: the repo-root `VERSION` file is
deliberately stale and NOTHING here touches it; `scripts/release/release.sh`
`--push` is break-glass (memql#4116) and stays so; `main` refuses direct
pushes (repository ruleset), so anything requiring a COMMIT goes through a PR
and the merge queue.

So "cut a new version" == "create the tag and publish the Release"; CI does
the rest. That is exactly the surface the automation drives -- it does NOT
build images, push images, or bypass any gate.

## Decisions

### D1 -- A Go integration drives GitHub; DSL exposes it; the owner role gates it

New integration `integrations/release/` (self-registering plug-in,
`memql.RegisterPlugin`, HTTP client shaped like `integrations/shopify/admin.go`),
exposing two builtins declared in `dsl/cluster/builtins.memql`:

- `releaseCut` -- args `bump enum("major","minor","patch") @required`,
  `notes string` (optional prepended prose; GitHub's
  `generate_release_notes: true` supplies the body either way). Computes the
  next version, creates the tag + published Release, records the row (D4),
  returns `{version, releaseUrl, baseSha}`.
- `releaseCutStatus` -- args `version string @required`. On-demand outcome
  check (D5). No poller.

**Gating is double-walled, like every credential surface here:**
- DSL wall: the portal reaches these through owner-gated constructs; the
  read query (D4) filters on the existing `requiresOwner` context-spec
  (`dsl/deployment/specs.memql:37`, mirrors `auth.IsOwner`).
- Go wall: the builtin itself re-checks the actor (`auth.IsOwner` /
  `actor.isClusterOwner`) FIRST and refuses otherwise -- builtins are not
  covered by the query/mutation row-authz conformance buckets, so the Go check
  is the load-bearing one and a unit test pins it (a non-owner actor gets a
  typed refusal before any HTTP happens; the memory rule applies: assert
  against a real engine actor, not a mocked one).

The user's word "automation" means the platform does it end to end, not that a
`@trigger` fires it. No scheduled automation is seeded -- an unattended release
cut with nobody watching is not wanted; the construct is there for one to call
later if the owner ever asks for it, and the spec says so to stop a future
reading of "automation" from adding a cron.

### D2 -- Version arithmetic reads GitHub, never the database or a local clone

`releaseCut` lists `refs/tags` via the GitHub API, parses `vX.Y.Z` ONLY (same
rules as the extension's `parseSemver`, `editors/vscode/src/install/tags.ts:88` --
`v` required, no pre-releases), takes the numeric max, applies `bump`. The tag
targets the sha of `GET /repos/{repo}/commits/main` at call time, and the row
records that `baseSha`. Refusals, all typed: next tag already exists (GitHub's
atomic ref-create is the concurrency gate -- two owners racing means the second
gets `ref_exists`, no advisory lock needed), `main`'s sha already carries a
release tag (`already_released_at_head`), token missing/expired
(`credential_unavailable` naming the secret to seed), GitHub 5xx
(`github_unreachable`, nothing created).

Order of operations: create tag ref -> create Release (published,
`generate_release_notes: true`, name `vX.Y.Z`) -> write the row. A tag created
but Release refused is reported as `tag_created_release_failed` with the tag
name -- half-done is stated, never hidden, and `releaseCutStatus` on that
version says the same until a human finishes or deletes the tag.

### D3 -- The credential is a fine-grained GitHub token in `globalSecret`; the repo is configuration

- Token: fine-grained PAT (or GitHub App installation token) with **Contents:
  read/write** on the one repo, seeded as `v1:platform:globalSecret` row
  `MEMQL_GITHUB_RELEASE_TOKEN` (the provider-auth resolver chain
  globalSecret -> globalVariable -> env, `component/memql/ai_providers.go:917`,
  is the precedent; this integration resolves through the same secret helper).
  It is never returned to any client and never logged; refusals name the
  VARIABLE, not the value.
- Repo: `MEMQL_RELEASE_REPO` (`v1:platform:globalVariable`, `owner/name` form),
  with **no compiled-in default** -- the engine is product-agnostic and must not
  carry `znasllc-io/memql` as a literal (product-neutrality doctrine); an
  instance that wants the button seeds the variable. Unset ->
  `release_repo_unconfigured`, stated on the portal card instead of the button.

### D4 -- An append-only record: `v1:cluster:releaseCut`

New concept in `dsl/cluster/concepts.memql` beside the deploy pair:
`version` (row id is the version -- one timeline per cut), `bump`, `baseSha`,
`requestedBy`, `status enum("dispatched","tag_created_release_failed",
"images_available","failed")`, `releaseUrl`, `error`, timestamps. Cluster-owner
tier (the `sendJob`/`suppression` pattern: engine-owned, admin-bucket reads).
Writes go through `@serverOnly` mutations (`createReleaseCut`,
`updateReleaseCutStatus`) called only by the integration. A
`v1:identity:auditEvent` row (`release_cut`, memql#4328's DECISIONS log) is
written alongside -- who cut what, from which sha.

Read surface: query `releaseCuts` (newest first, `requiresOwner` conjunct) for
the portal list.

### D5 -- Status is checked on demand against the artifact, not the pipeline

`releaseCutStatus(version)` answers "do the images for this version exist
yet": an anonymous GHCR manifest check
(`ghcr.io/v2/znasllc-io/memql-bff/manifests/<bare-version>` via the public
token dance -- the images are public, `stackPin.ts:229-233`) for a
representative node set. All present -> row moves to `images_available`;
absent -> still `dispatched` with age; check errored -> the error is SHOWN and
the status is NOT guessed (verify the artifact, not the action -- the run
could have failed after the Release published, and only the registry knows).
The workflow-run mapping is deliberately not attempted: the Actions list API
does not expose dispatch inputs, and the registry is the truth the operator
actually needs. The GHCR repo path derives from `MEMQL_RELEASE_REPO`'s owner --
no second literal.

### D6 -- Portal surface: a Releases card on the Deployments page, owner-only

- Shows: newest existing tag (from the last `releaseCut` row or a
  `releaseCutStatus`-style tags read), the cut form (bump radio defaulting to
  `patch`, optional notes, and a typed confirm phrase `cut-a-release` -- the
  capability-script convention for a consequential action, carried into the
  UI), and the `releaseCuts` list with per-row "Check images" invoking
  `releaseCutStatus`.
- Rendered only for `actor.isClusterOwner`; a non-owner sees nothing (not a
  disabled button -- `instanceActions.ts`'s doctrine: never offer a button
  whose only outcome is a refusal).
- Wire path: ordinary gRPC-over-WS constructs -- no new HTTP endpoint
  (endpoint policy: gRPC-first; nothing here is an exception).

### D7 -- One follow-on the cut can open, as a PR, never a push

After a successful cut, `releaseCut` optionally (arg `bumpExtensionPin bool`,
default false; the portal card offers a checkbox) opens a PR bumping
`editors/vscode/src/install/stackPin.ts`'s `DEFAULT_STACK_TAG` to the new tag
-- the release step `stackPin.ts` says keeps the pin current, and sub-project
M narrows that pin to offline-fallback duty. Branch + PR via the same token
(Contents + Pull requests scopes); the merge queue applies as to any PR; a PR
that cannot be opened degrades to a row note, never a failure of the cut.

## Testing

- Integration unit tests against a fake GitHub server: version arithmetic
  (max/bump/no-prerelease), every typed refusal in D2, the half-done path,
  owner-gate refusal for a non-owner actor (real engine actor, not a mock),
  secret-miss message naming `MEMQL_GITHUB_RELEASE_TOKEN`.
- GHCR checker against a fake registry: present/absent/error -> the three
  honest statuses.
- DSL conformance: `releaseCuts` classifies admin; new mutations are
  `@serverOnly`; concept loads under strict boot.
- Portal: card renders owner-only; confirm phrase required; list reads.
- NOT tested against real GitHub in CI (no live tags from CI, ever); a
  `--dry-run` arg on the builtin returns the computed plan without writes, and
  the runbook uses it for the first live validation.

## Out of scope

- Building or pushing images from the cluster (the CI cascade owns it).
- Touching `VERSION`, `release.sh`, or the dispatch workflows.
- Cutting releases of OTHER repos (bundle/SPA repos have their own dispatch
  paths; the repo variable is singular on purpose).
- A scheduled auto-release automation (explicitly not seeded, D1).
- VS Code extension surface for cutting (the portal is the operator console;
  the extension's "Check For New Releases" already reads tags).

## Risks

- A leaked `MEMQL_GITHUB_RELEASE_TOKEN` can tag/publish in the one repo.
  Bounded by: fine-grained single-repo scope, globalSecret encryption at rest,
  owner-only reach, the audit row, and GitHub's own tag-protection rules
  (recommend enabling tag protection for `v*` so only the token's identity and
  maintainers may create them -- runbook step).
- GHCR anonymous-check plumbing can change; D5's honest-error rule means that
  degrades to "dispatched + check failed", never to a false green.
- Two sources of "latest version" (GitHub tags vs releaseCut rows) could
  disagree if a human cuts by hand; the portal card reads TAGS for "newest
  existing" and rows only for history, so a hand cut simply appears as newest
  with no row -- correct and stated on the card.

## Task breakdown (preview; tasks carry the acceptance criteria)

1. `integrations/release/`: GitHub client, version arithmetic, tag+Release,
   typed refusals, owner gate, dry-run.
2. Concept + `@serverOnly` mutations + `releaseCuts` query + audit row +
   conformance.
3. `releaseCutStatus` GHCR checker + row transitions.
4. Portal Releases card (owner-only, confirm phrase, list + check-images).
5. Pin-bump PR follow-on (D7) + runbook
   (`docs/public/operate/release-cutting.md`: token mint, tag protection,
   first dry-run).
