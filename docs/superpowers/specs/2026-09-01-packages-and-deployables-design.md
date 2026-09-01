# Packages and Deployables -- Design

- **Date:** 2026-09-01
- **Status:** approved (multi-round in-session Q&A with the owner; every fork
  below records the choice that was made and why)
- **Scope:** `dsl/platform/` (two new concepts + queries/mutations/builtins),
  a new `integrations/packages/` pipeline integration, `component/memql/`
  (site status guard extension, analysis over the offline Init gates),
  `component/edge` (unchanged serving; the publisher is reused),
  `deploy/k8s/` (the dsl-bundle init-container gains a blob-fetcher mode),
  `clients/os/` (the Deployables app grows a Packages section + per-site
  lifecycle parity), the memql-project template (gains a manifest). No new
  HTTP routes; the pipeline is graph mutations + existing seams end to end.
- **The wave this belongs to:** Epic A of three. Epic B (the Accounts app)
  and Epic C (custom domains with DNS verification + automatic certificates)
  get their own design rounds; this spec deliberately leaves their hooks out
  rather than half-designing them.
- **Follow-ups filed, not built here:** the Bin app (#4784), the OS Nexus
  app + planner goals-to-automations refactor (#4785), Go-pack delivery,
  archive purge/retention, auto-deploy-on-update.

## Why

Owner's brief, condensed: MemQL instances become a hosting platform managed
through MemQL itself -- a Lovable-class loop where a person (or an agent)
hands the cluster an app and it is live at a hostname in seconds, versioned,
rollback-able, isolated from the platform when it breaks. The unit is not a
file: it is the **package**, the memql-project-shaped tree a client repo
already is -- DSL domains, client apps, maybe Go integrations -- arriving
either as a Git repo or as a zip of that same tree, interchangeably. Nothing
becomes a package by claiming to be one: an **analysis** decides, before
anything deploys, and refuses with reasons a person can act on. A repo-sourced
package treats the repo as the source of truth: the platform notices when
something newer exists and offers to deploy it. Deployables get full lifecycle
management (draft, live, disabled, archived -- with the archive visible, and
purge explicitly deferred). The MemQL OS is the surface; the portal is
maintenance-only.

## Locked decisions

| # | Decision | Choice (owner-approved) |
|---|---|---|
| D1 | The unit | The **package**: a memql-project-shaped tree. Two source forms, interchangeable and equally validated -- a Git repo, or a zip of the same tree. Mandatory analysis before any deploy; no static-vs-DSL split in the UX -- a package contains what it contains |
| D2 | The manifest | `memql-package.yaml` at the tree root, `formatVersion: 1`. **Deployables are declared** (name, path, kind, optional build command/output, storefront binding) because kind and binding cannot be inferred; **DSL domains are discovered** from `dsl/<domain>/` exactly as the engine's own mount discovers them. Hostnames/slugs are NOT manifest data: the manifest describes the software, the deploy describes the placement (slug chosen at first deploy, remembered after) |
| D3 | Whole-package model | DSL + SPAs deploy through this path. A Go pack (`bff/` with `go.mod`) is **detected and reported, deferred**: it appears in the report with a typed per-half refusal pointing at the existing CI/image path, and the rest of the package deploys. Full Go delivery is its own future epic |
| D4 | Builds | **In-cluster, on the workbench surface**: sandboxed, resource-capped, no cluster credentials, per-deployable `npm ci && npm run build` (manifest-overridable) with `dist` as default output. Fast-path: prebuilt output already present in the snapshot skips the build. No dependency on GitHub Actions or per-repo CI secrets |
| D5 | DSL delivery | **DSL is data, not an image.** Staged as content-addressed trees in blob storage; a well-known active-set pointer document (`blob://packages/active.json`, domain -> content-addressed prefix) is read by the dsl-bundle init-container (fetcher mode) into the shared `MEMQL_DSL_PATH` volume; nodes boot exactly as today. The roll drives `DeployControlService.RolloutAction`. No registry, no image builds, no CI |
| D6 | Ordering | `stage -> roll -> publish`, reversed on rollback, so app and schema never disagree in either direction. A package with no DSL (or unchanged DSL) **skips stage+roll entirely** -- SPA-only deploys stay restart-free and land in seconds |
| D7 | The spine | Two new concepts: `v1:platform:package` (the tracked source) and `v1:platform:packageDeployment` (append-only timeline, one row per attempt). Sites join by `packageId` + `packageDeployableName` on `v1:platform:site`; first deploy creates the site (draft), later deploys republish it. The graph's history IS the version list; package-level rollback restores a prior deployment row's (dslVersion + site bundleRefs) tuple. Hand-made sites (no `packageId`) remain fully supported |
| D8 | Files overlap | Resolved at the Library, once: **every source snapshot is a content-addressed Library artifact** with provenance (repo fetch or uploaded zip). Files stores and identifies; packages consume. A zip row in Files gets an "Analyze as package" action (coordinated with #4721's row-actions work, loosely coupled). Re-analysis never refetches |
| D9 | Trust | One UX, gated by **contents**: a deploy whose package carries DSL requires the cluster-owner actor in v1 (typed refusal names it -- DSL is server-side authority and multi-tenant DSL sandboxing is explicitly not this epic); an SPAs-only package deploys under the caller's own authority, preserving memql#4344 per-user site ownership. Invisible to the owner operating their own instance |
| D10 | Lifecycle | `draft -> live <-> disabled -> archived`. Archive requires disabled first, plus a typed-name confirmation; archived rows are **visible** behind an Archived filter (an archive is a place, not a void). A package with non-archived sites refuses archive (`package_has_active_deployables`). Purge/retention: deliberately deferred. **System-owned rows (the seeded portal and OS sites) are exempt from the lifecycle entirely -- always live, never disabled, never archived**: the memql#3717 delete guard's prior-row semantics extend to status writes, and the OS renders no lifecycle controls on them (presentation over a server-side law) |
| D11 | Updates | The repo is the **tracked source of truth**. Webhook preferred: GitHub push/release into the existing `POST /inbound/{source}` seam (deny-by-default allowlist + per-source HMAC), matched by repo URL. Scheduled-automation polling as fallback. Both only write `latestKnownVersion` + `updateAvailable` on the package row, which broadcasts and lights the OS cue; deploying the update is a one-click NEW deployment run (through analyze + confirm again). Auto-deploy is deferred |
| D12 | Analysis | Runs the **same Init-grade gates strict boot runs** (the memqllint machinery), offline, before anything deploys -- "this DSL would refuse boot" is an analysis refusal, never a crashlooping node. The report is first-class on the deployment row and renders at an always-present confirm gate (one click on redeploys). Every refusal is a stable machine-readable code |
| D13 | Surface | The MemQL OS Deployables app (portal is maintenance-only): a new Packages section, plus per-site parity migration -- version history + rollback, enable/disable, archive -- of the four portal features, leaving raw `bundleRef` pointing operator-side |
| D14 | Secrets | `repoTokenRef` **names** a `v1:platform:globalSecret` (the storefront-token pattern); the value never lands on a row and is resolved only at fetch time. Private repos work day one |

## A. What exists today (the ground this builds on)

Verified in-session, so tasks inherit facts rather than assumptions:

- `v1:platform:site` is already the deployable (memql#4344): per-user
  ownership (`@rowAuthz(owner="ownerUserId", clusterOwner)`), the Go-side
  hostname policy with a derived reserved set, the `*.<domain>` wildcard
  Ingress rule + DNS-01 wildcard certificate -- a fresh site is live over TLS
  with no operator step and no restart.
- Versioning/rollback is implemented in the portal: `siteById` re-issued
  under successive `asOf` timestamps is the version list; rollback is
  `updateSiteBundle` pointing back; content-addressed prefixes keep old
  versions servable (`clients/portal/src/deployables/calls.ts`).
- `sitePublishFromArtifact` (`component/sitepublish/`) validates a Library
  zip (root `index.html` for spa/storefront, per-file 25 MB / total 500 MB /
  20000 files, traversal refused), publishes atomically (version written,
  then the row flips), stamps `artifactId` provenance, audits both outcomes,
  and refuses with ~18 stable codes. The CI door `POST /sites/{id}/bundles`
  is untouched by this epic.
- All four site mutations exist: `createSite`, `updateSiteBundle`,
  `updateSiteStatus`, `deleteSite` (soft; `deleted` rows are filtered from
  every list today -- which is exactly why D10 adds a visible archive).
- The systemOwned **delete** guard exists and is bypass-proof against a
  same-delta `systemOwned:false` flip (memql#3717,
  `component/memql/platform_site_delete_guard.go`); **status has no guard**
  -- the D10 gap. Both the portal and the OS site rows are boot-re-seeded
  (`osSiteSeedName = "os"`, `frontdoor.OsSite`).
- `MEMQL_DSL_PATH` already mounts N product domains by directory scan, with
  strict-boot validation and core-domain collision skip
  (`dsl.MountRuntimeDomainsFromEnv`); delivery today is a single data-only
  bundle image via the `deploy/k8s/components/dsl-bundle` init-container --
  the one mechanism D5 generalizes.
- `DeployControlService` (identity node; bff-forwarded with the caller as a
  verified `ForwardedAuthority`) exposes `Deploy` / `Rollback` /
  `RolloutAction` / `GetDeploymentStatus` over the capability scripts -- the
  roll seam exists.
- The workbench is the platform's sandboxed exec surface (per-plan
  workspaces, cluster mode with replica affinity, typed refusals) -- the
  build seam exists.
- The inbound webhook seam exists (`POST /inbound/{source}`, memql#2957:
  deny-by-default source allowlist, per-source HMAC).
- The OS Deployables app is live: list + 2D map + detail (broadcast rows,
  arrival cues, bundle-flip marker), create form (slug -> hostname preview,
  kind, storefront binding), publish-from-Library picker. Status changes,
  rollback and deletion are deliberately deferred to this epic
  (`actions/ActionsSection.tsx`).
- The memql-project template carries `dsl/__PRODUCT__/`, `clients/web`,
  `deploy/Dockerfile.bundle` and `publish-images.yml` -- the tree shape D1
  standardizes on, and the repo that gains the D2 manifest.

## B. The manifest

```yaml
formatVersion: 1                 # versioned so the format can grow
name: acme-storefront
deployables:
  - name: storefront             # unique within the package
    path: clients/web
    kind: shopify_storefront     # spa | static | shopify_storefront
    build:                       # optional; these ARE the defaults
      command: "npm ci && npm run build"
      output: dist
    binding:                     # storefront kind only
      storeDomain: acme.myshopify.com
      storefrontTokenRef: shopify-storefront-token
```

Rules, all enforced by analysis with typed refusals:

- No manifest, no package (`package_manifest_missing`); unparseable or
  unknown `formatVersion` is `package_manifest_invalid`.
- Every declared `path` must exist and be a directory inside the tree;
  `kind` must be one of the three live values; a storefront must carry its
  binding; deployable names must be unique.
- DSL domains are discovered, not declared; a domain colliding with a core
  engine namespace is `dsl_domain_reserved` -- a refusal here, never the
  engine's silent mount-time skip.
- The zip form is a zip of exactly this tree, manifest at zip root; held to
  publisher-grade limits (env-tunable for packages).
- The template repo gains this file (stamped by init like the rest); the
  template-CI lints-against-latest-engine coupling is a known coordination
  point for the release that lands the format.

## C. Concepts

`v1:platform:package` -- the tracked source (`@rowAuthz(owner="ownerUserId",
clusterOwner)`, broadcast):

| Field | Notes |
|---|---|
| `ownerUserId` | stamped from the actor, memql#4344 style |
| `name` | from the manifest at first analysis |
| `sourceKind` | `repo` \| `artifact` |
| `repoUrl`, `repoRef` | ref defaults to the default branch when empty |
| `repoTokenRef` | NAMES a globalSecret; empty = public (D14) |
| `artifactId` | zip-source provenance (Library) |
| `deployedVersion` | commit SHA / content hash currently live |
| `latestKnownVersion`, `updateAvailable` | written only by the D11 feeds |
| `status` | `active` \| `archived` |

`v1:platform:packageDeployment` -- the append-only timeline (same tier,
broadcast): `packageId`, `sourceVersion`, `status` (`analyzing`,
`awaiting_confirm`, `building`, `staging`, `rolling`, `publishing`,
`succeeded`, `refused`, `failed`), `report` (object -- section E),
`dslVersion` (content-addressed prefix, empty when the package has none),
per-deployable outcomes (name, siteId, bundle version, or per-half
refusal), `error` (typed), `requestedBy`. Rows are never updated after a
terminal status; a retry is a new row.

`v1:platform:site` gains two fields: `packageId`,
`packageDeployableName` (+ a `references`-type relationship to `package`).
Empty on hand-made sites.

## D. The pipeline

Stages advance by writing `packageDeployment.status`; every stage is
idempotent, every write is a `@serverOnly` mutation under stamped internal
origin, and stage handoffs are row events with routing rules -- the
multi-node rules apply (explicit plumbing, in-process hop tests).

| Stage | Seam it rides | Notes |
|---|---|---|
| fetch | GitHub tarball API (+ PAT via D14) or Library zip bytes | snapshot stored as a content-addressed Library artifact (D8) |
| analyze | offline Init gates + manifest/tree walk | report or typed refusal; Go packs marked deferred (D3) |
| confirm | OS renders the report | hostname picker on first deploy; one click on redeploys |
| build | workbench exec per deployable | prebuilt fast-path; bounded log tail captured onto the row |
| stage | blob storage, content-addressed | only when DSL present and changed |
| roll | active-set pointer flip + `DeployControlService.RolloutAction` | rolling restart; old pods serve until new pods are healthy; deploy-gate refuses red |
| publish | the `sitePublishFromArtifact` publisher | per deployable: create site (draft) on first deploy, republish after; a live site stays live with the new bundle |

DSL delivery mechanics (D5): the publish half of the pipeline writes the
tree under `blob://packages/<domain>/<contentHash>/`, then atomically
rewrites the active-set pointer document; the dsl-bundle init-container's
fetcher mode reads the pointer and copies the trees into the shared
`MEMQL_DSL_PATH` volume before the node boots. Rollback of the DSL half is
pointing back + roll -- the same shape as a site rollback.

## E. Analysis report and refusal codes

The report carries: source version; each deployable (name, kind, path,
build plan or "prebuilt output found -- build skipped"); each DSL domain
with construct counts; any Go pack ("reported, not deployable through this
path"); every problem found. Refusal codes (stable, machine-readable, in
the `sitePublishFromArtifact` tradition): `package_manifest_missing`,
`package_manifest_invalid`, `deployable_path_missing`,
`deployable_kind_unknown`, `deployable_binding_missing`,
`dsl_domain_reserved`, `dsl_refuses_boot` (carrying the construct-level
errors strict boot would print), `source_too_large`, `bundle_path_invalid`,
`go_pack_not_deployable` (per-half), `dsl_requires_cluster_owner` (D9),
`package_has_active_deployables` (D10), `workspace`/build failures surfaced
with the workbench's own typed codes. Success and refusal both audit to
`v1:identity:auditEvent` (`action: package_deploy`).

## F. Update detection

Two feeds, one effect (D11): set `latestKnownVersion` + `updateAvailable`,
broadcast, light the OS cue. The webhook feed matches packages by repo URL
inside the existing inbound-delivery handler; the polling feed is a
scheduled automation walking repo-sourced packages and comparing upstream
HEAD/latest release via the API under `repoTokenRef` where set. Neither
feed deploys anything: deploying the update is a person's click starting a
new deployment run at the new version.

## G. The OS surface

- **Packages section**: live list (name, source, deployed vs latest,
  update cue); package detail (source facts, latest report, deployment
  timeline, deployables linking to their site rows, Deploy / Redeploy,
  archive). New-package flow: GitHub URL + ref + optional secret name, or
  pick a zip from Files.
- **Per-site parity** (the portal analysis): version history + rollback,
  enable/disable, archive -- with the D10 state machine enforced in the
  flow and the server; Archived filter makes the archive visible. Raw
  `bundleRef` pointing stays out (operator tool).
- **First deploy**: per-deployable hostname picker with live slug
  validation (the existing `CreateSite` policy mirror).
- System-owned rows render no lifecycle controls (D10).
- House rules throughout: LiveList over the one Connection, arrival cues
  fingerprint change (never liveness), errors in-surface with the server's
  sentence verbatim, no toasts.

## H. Security

- Both new concepts: composite owner tier; all pipeline writers stamped
  internal origin (the `call_origin.go` allowlist grows deliberately).
- D9 contents gate at deploy start, before any build or stage.
- Builds: workbench isolation, no cluster credentials in the environment,
  bounded resources; snapshot zips held to publisher-grade limits.
- Secrets only ever named (D14); the fetcher resolves at fetch time.
- Roll safety is layered: analysis already ran boot's own gates; the
  restart is rolling; the deploy-gate refuses on red; break-glass is
  "revert the pointer, roll again".

## I. Error handling

Typed reason at every refusal; the OS renders the server's sentence
verbatim. Failures before `publish` leave everything serving the old
version -- ordering makes partial deploys structurally impossible. Build
failures capture a bounded log tail onto the deployment row so "why did my
build fail" is answered inside the OS. An analysis pass is repeatable from
the stored snapshot without refetching.

## J. Testing

- Analysis against fixture packages (valid / broken manifest / DSL that
  refuses boot / traversal zip / Go pack / prebuilt fast-path) -- pure Go,
  no cluster, fast.
- Pipeline state-machine unit tests; **in-process hop tests** for every
  cross-node stage handoff (the memql#4352 pattern -- a live-cluster-only
  gate guards nothing); one cluster-e2e lane exercising a full fixture
  deploy including the roll on the parity cluster.
- The D10 status-guard extension gets the same bypass tests the delete
  guard has (same-delta flips).
- Conformance: new concepts classified by the dslconformance gate; the
  generated-artifact fan-out (SDK gen, arch model, env registry, embed
  counts, memqllint) is expected work, not a surprise.
- OS: the live-list retain()/arrival-cue contract per `clients/os/README.md`;
  vitest per-file isolation.

## K. Out of scope, and neighbors

Out of scope for Epic A: Go-pack delivery; archive purge/retention (the
owner notes this eventually becomes a cluster-owner scheduled automation --
see the goals-to-automations direction on #4785); auto-deploy-on-update;
account ties (Epic B adds the fields); custom domains + DNS verification +
automatic certificates (Epic C); instance-as-a-deployable dogfood; DSL
hot-mount (the roll IS the mechanism); Bin cross-surfacing of archived
deployables (#4784's recorded open question).

Neighbors: the Files epic (#4721) owns row-actions machinery the "Analyze
as package" action plugs into; the Bin app (#4784) and the OS Nexus epic
(#4785) are filed follow-ups; the memql-project template gains the
manifest; Epics B and C follow this spec's approval as their own design
rounds.
