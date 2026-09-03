---
title: Packages and deployables
audience: public
status: stable
area: operate
sinceVersion: 0.19.0
owner: znas
---

# Packages and deployables

A **package** is the memql-project-shaped tree a client repo already is: DSL
domains, client apps, maybe a Go pack. Hand one to a cluster and it is live at
a hostname in seconds -- versioned, rollback-able, and isolated from the
platform when it breaks.

A package is a **source**. The things it produces are **deployables**, and the
flow that composes one, gives it an address and manages it afterwards is
[deployables.md](deployables.md). This page is the source half: the manifest,
what the analysis refuses, the pipeline's order, how DSL is delivered, and the
full refusal catalogue.

Design records:
[packages and deployables](../../superpowers/specs/2026-09-01-packages-and-deployables-design.md)
(epic memql#4794), then
[the Deployables program](../../superpowers/specs/2026-09-02-deployables-program-design.md)
and [Compose](../../superpowers/specs/2026-09-02-deployables-compose-design.md)
(epic memql#4885), which folded the three sections into one flow and replaced
the named cluster secret with a personal credential.

## The short version

1. Pick a source: a `github.com` URL, a zip already in Files, or your own CI
   pushing bundles. Nothing deploys.
2. **Analyze.** The cluster fetches, analyzes offline and parks a run at the
   confirm gate with a report you can read.
3. **Deploy.** Confirm it, with the address each new app goes to. It builds,
   stages, rolls and publishes -- in that order, always.
4. The repo stays the source of truth: when something newer lands, the source
   says so, and deploying the update is one click that starts the same run
   again at the new version.

Two clicks on a first deploy, one after. The surface is the MemQL OS
Deployables app; the portal is maintenance-only.

## The manifest

`memql-package.yaml`, at the root of the tree:

```yaml
formatVersion: 1
name: acme-storefront
deployables:
  - name: storefront             # unique within the package
    path: clients/web
    kind: shopify_storefront     # spa | static | shopify_storefront
    build:                       # optional; these ARE the defaults
      command: "npm ci && npm run build"
      output: dist
    binding:                     # shopify_storefront only
      storeDomain: acme.myshopify.com
      storefrontTokenRef: shopify-storefront-token
```

Two halves, and the asymmetry is deliberate:

- **Deployables are DECLARED.** A directory with a `package.json` could be a
  site, a component library or tooling, and its `kind` -- whether a mistyped
  path 404s or falls back to `index.html` -- is a fact no walk can recover.
- **DSL domains are DISCOVERED** from `dsl/<domain>/`, exactly as the engine's
  own `MEMQL_DSL_PATH` mount discovers them.

**There is no hostname here.** The manifest describes the software; the deploy
describes the placement. A hostname is chosen once, at a deployable's first
deploy, and remembered on its site row -- so the same manifest deploys to two
clusters without an edit, and renaming a site needs no pull request against the
package. The placement itself is `placements` on `packageDeploy`
([deployables.md](deployables.md)).

A `bff/` with a `go.mod` is **detected, reported and deferred**: it appears in
the report saying where Go delivery happens today (engine images built by CI),
and every other half of the package deploys around it.

## What the analysis checks, and what it refuses

The analysis runs **offline**, before anything is fetched to a workbench or
written to storage, and it runs **the same gates a node runs at boot**. "This
DSL would refuse boot" is an answer you get here, not a crashlooping pod later.

Every refusal is a stable machine-readable code
(`component/packages/refusal.go`). The OS renders its own sentence for the ones
it knows and the server's own sentence verbatim for the rest -- inventing a
friendly sentence for an unknown failure is how a real fault gets mistaken for
somebody's mistake.

### Fatal to the whole package

| Code | What it means |
|---|---|
| `package_manifest_missing` | No `memql-package.yaml` at the tree root |
| `package_manifest_invalid` | Unparseable, an unknown key, a missing name, an unknown `formatVersion`, or two deployables sharing a name |
| `deployable_path_missing` | A declared `path` is not a directory in the tree |
| `deployable_kind_unknown` | `kind` is a value nobody has heard of -- not one of the three live values, and not one of the known-but-unoffered ones below |
| `deployable_binding_missing` | A storefront with no `binding`; also a never-deployed app whose placement names no hostname |
| `dsl_domain_reserved` | A `dsl/<domain>/` whose name the engine already owns |
| `dsl_refuses_boot` | The package's DSL does not survive the Init-grade gates; carries the construct-level errors |
| `source_too_large` | Over the per-file, whole-tree or file-count cap |
| `source_unreadable` | Not an archive this cluster can open, or a repository or a GitHub it cannot reach |
| `bundle_path_invalid` | An archive entry escaping the package root |
| `dsl_requires_cluster_owner` | A DSL-carrying deploy by a non-cluster-owner. Raised at the START of the run, before any build |
| `package_has_active_deployables` | Archiving a source while one of its apps is still `live`. Names the live hostnames, and writes nothing |
| `archive_confirmation_mismatch` | The typed confirmation does not match the package's stored name. Verified by the server, not just the form |

### The source and its credential

| Code | What it means |
|---|---|
| `source_host_unsupported` | The repository (or a credential) names a host this cluster does not fetch from -- only github.com today, or upload a zip of the tree instead. `sourceProbe` answers it as a REASON rather than an error |
| `credential_not_found` | The package names a credential its OWNER cannot read: it does not exist, or it belongs to somebody else. Refused by name, before any request leaves the cluster |
| `credential_revoked` | The credential the source fetches under was revoked; every source fetching under it refuses until it is switched to another one |

### Reported, and NOT fatal

| Code | What it means |
|---|---|
| `go_pack_not_deployable` | A `bff/` with a `go.mod`. The rest of the package deploys |
| `deployable_target_not_offered` | `kind` is one the target model knows and does not offer yet (`ios`, `android`, `macos`). Scoped to that app -- reported with "iOS is not offered on this cluster yet", skipped with the build plan "skipped -- not offered on this cluster yet", and recorded on the deployment row with no site id, while the rest of the package deploys around it |

### Raised during the run

| Code | What it means |
|---|---|
| `deployable_build_failed` | The build stage refused. Scoped to that app and fatal to the run; every site is still serving what it was serving, and the build output is on the row |
| `deployable_publish_failed` | The publish of one app's bundle failed. Fatal to the run |
| `deploy_failed` | Not a refusal -- a store or a storage call that broke mid-run. Nothing about the source was changed, and a new attempt is the way forward, because the timeline is append-only |
| `deployable_account_refused` | The `accountId` half of a placement was refused by the account write's own guard. Recorded on that app's OUTCOME, **not fatal**: the site is live at its cluster address either way |
| `deployable_domain_refused` | The `ownDomain` half was refused -- a hostname under the cluster's own domain, a collision, or the per-site cap. Recorded the same way, **not fatal** |

An **unknown `formatVersion` refuses** rather than parsing the subset it
recognises, and so does an **unknown KEY**: `deployabels:` parses fine and
describes a package that deploys nothing while reporting success.

## Who may deploy what

One UX, gated by CONTENTS:

- A package of **web apps only** deploys under the caller's own authority, and
  the sites it creates are theirs (the memql#4344 per-user ownership model).
- A package that ships **any MemQL DSL** requires the **cluster-owner** actor,
  and is refused with `dsl_requires_cluster_owner` **at the start of the run**
  -- before any build and before anything is staged. The What-it-is stop says
  so before the click rather than after it.

DSL is server-side authority for the whole cluster; multi-tenant DSL sandboxing
is not built. An operator running their own instance never sees this gate.

## The order, and why it never changes

```
fetch -> analyze -> confirm -> build -> stage -> roll -> publish
```

reversed on rollback:

```
publish back -> pointer back -> roll
```

Forward, the schema has to arrive before the app that uses it. Backward, the
app has to stop using the schema before the schema changes. **A failure
anywhere before `publish` leaves every site serving exactly what it was
serving** -- ordering is what makes a partial deploy structurally impossible
rather than merely unlikely.

**A package with no DSL, or whose DSL is byte-identical to what is already
staged, skips `stage` and `roll` entirely.** Nothing restarts and the deploy
lands in seconds. The surface draws those skipped stages with their reason
rather than omitting them, so a fast deploy is legible as a fast deploy -- and
a person counting missing steps has no way to tell "nothing had to restart"
from "it stopped here".

**The confirm gate lives on the ROW**, at `status: "awaiting_confirm"`, not in
a browser. Somebody who closed the window finds their run exactly where they
left it, and the list marks the deployable "a deploy is waiting for you" from
`packageDeploymentsAwaitingConfirm`.

## Private sources

A source fetches under a personal credential its **owner** holds:
`v1:platform:sourceCredential`, sealed server-side and named by the package's
`credentialId`. The token is never on the package row, in a snapshot, or in a
log line, and it is resolved under the PACKAGE OWNER's actor at fetch time --
so a package naming somebody else's credential is refused
`credential_not_found` before any request leaves the cluster.

Add one on the Source stop while composing, or in the **Sources group** in the
Deployables app's Settings section, which lists every credential you hold with
the sources fetching under it. Rotation is adding a new credential and
switching the source to it (`updatePackageSource`); revoking is one-way, and
the row is kept because it is the history of what fetched under it.

The whole shape -- the fields, the resolution rule, the cluster-owner case, and
what replaced the old cluster-wide named secret -- is in
[deployables.md](deployables.md). There is no cluster-wide source credential
any more, and there is no shim: a package row still carrying a secret NAME from
before reads as "no credential" and is asked for one.

## Delivering DSL: the pointer, the fetcher, the roll

DSL is **data, not an image** -- there is no registry, no image build and no CI
in this path.

1. **stage** writes each domain's tree to
   `blob://packages/<domain>/<contentHash>/`. Content-addressed, so re-staging
   an unchanged tree writes the same bytes to the same place.
2. **roll** rewrites one small pointer document,
   `blob://packages/active.json` (`domain -> prefix`), and then restarts the
   DSL-consuming workloads.
3. On boot, the `dsl-packages` init container (`memql dsl-fetch`) reads that
   pointer and copies the named trees into the shared `MEMQL_DSL_PATH` volume.
   **The node then boots exactly as it does today.**

Rollback of the DSL half is *pointing the document back and rolling again* --
the same shape as a site rollback, and safe because an old prefix's bytes are
still there.

Apply the component in an instance overlay:

```yaml
components:
  - ../../components/dsl-packages
labels:
  - pairs: { memql/product-dsl: "true" }
    includeSelectors: false
```

### The fetcher's exit codes are the contract

`memql dsl-fetch` runs as an init container, so its exit code decides whether
the pod starts.

- **0** -- the trees are in place, **or there is no pointer at all**. A cluster
  that has never deployed a package has no `active.json`, and that is the
  ordinary state rather than a fault; refusing to boot over it would mean the
  component could never be applied before the first deploy.
- **1** -- a pointer EXISTS and could not be honoured. Booting anyway would
  bring the node up with silently-missing product DSL, which presents as a
  healthy cluster answering "function not found" to every call it used to
  serve.

### Break-glass

If a roll leaves the cluster unhappy: **put the pointer back and roll again.**
That is one blob write plus one restart, and it is the same operation the
rollback button performs.

## Update detection

Two feeds, one effect. Both write exactly `latestKnownVersion` and
`updateAvailable` on the package row, and **neither ever deploys anything**.

- **Webhook (preferred).** GitHub push and release events arrive through the
  existing `POST /inbound/{source}` seam -- deny-by-default source allowlist
  plus per-source HMAC (memql#2957). No new HTTP route. Point a repository
  webhook at `https://api.<domain>/inbound/github`, add `github` to
  `MEMQL_INBOUND_SOURCE_ALLOWLIST`, and set its HMAC secret the way that
  runbook describes. A cluster running [GitHub Connect](github-connect.md)
  configures ONE webhook on the app instead of one per repository, and its
  secret is the app's own.
- **Polling.** A scheduled automation every ten minutes, for clusters no
  webhook can reach. It reads each repo-sourced package's upstream head under
  **that package's own credential**, resolved at call time under the package
  owner's actor and stored nowhere. A package whose credential cannot be
  resolved -- revoked, or not its owner's -- is **skipped with a warning
  rather than polled anonymously**, so a private repository never reads as
  unchanged because the request answered 404.

An upstream that has NOT moved is not written, so the OS cue does not flicker
and the mesh does not carry an event per package per poll.

## The lifecycle, and the archive

```
draft -> live <-> disabled -> archived
```

- **Archive requires disabled first.** Archiving is the end of a deployable's
  life, and pausing is the step that gives anyone still using it a chance to
  notice.
- **Archived restores to disabled**, never straight to live: publishing again
  is its own decision.
- **An archived site answers 404.** To the internet it is gone, not paused. A
  PAUSED one answers 503, which is what keeps a deliberately paused site
  distinguishable from a typo.
- **An archived row stays listed** behind the Archived filter. An archive is a
  place, not a void -- which is exactly what the older soft-delete was not.
- **The cluster's own portal and OS sites are exempt from all of it.** They are
  re-seeded live at every boot; a status write on one is refused whoever asks,
  including a cluster owner, and the OS renders no controls on them at all.

**Archiving a source archives every app it produced.** `packageArchive` refuses
with `package_has_active_deployables` -- naming only the LIVE hostnames, and
before any site is touched -- while any app is still serving; pausing first
stays the person's decision. Otherwise every paused app is archived, a
never-published (draft) app is walked through `disabled` first, and each write
goes through the same guarded status write `siteArchive` uses: sites first, the
package last, and a write the guard refuses surfaces and stops the cascade
there. Apps already archived are left alone, and the reply names the hostnames
that were archived. `packageRestore` does **not** cascade back: restoring a
source is not a decision to redeploy it.

Archiving asks you to type a confirmation -- the site's hostname for
`siteArchive`, the package's own name for `packageArchive`. The **server
verifies it**, not just the form (`archive_confirmation_mismatch`), and a
mismatch writes nothing.

## Configuration

| Variable | Default | What it does |
|---|---|---|
| `MEMQL_PACKAGES_MAX_SOURCE_BYTES` | `524288000` (500 MB) | Cap on an expanded source tree |
| `MEMQL_PACKAGES_MAX_FILE_BYTES` | `26214400` (25 MB) | Cap on one file inside it |
| `MEMQL_PACKAGES_MAX_FILE_COUNT` | `20000` | Cap on the file count |
| `MEMQL_PACKAGES_ROLL_TARGETS` | *(unset)* | Comma-separated workloads a DSL roll restarts. **Unset means the roll refuses** rather than guessing |
| `MEMQL_PACKAGES_WEBHOOK_SOURCE` | `github` | Which `/inbound/{source}` segment carries package webhooks |

A cap that is set and unparseable, or non-positive, falls back to its DEFAULT
rather than to "no limit".

`MEMQL_PACKAGES_ROLL_TARGETS` is deliberately not defaulted: a roll restarts
pods, so its blast radius is a decision somebody made rather than whatever
happens to be running. Only clusters hosting DSL-carrying packages need it.

Sealing and unsealing a source credential uses `MEMQL_MASTER_KEY`, which every
node already has; a ciphertext this node cannot unseal is refused as
`source_unreadable` naming that variable, because the repair is an operator's.

## Builds: what ships today

A deployable whose built output is **already in the source** (`dist/index.html`
present) deploys with no build at all -- no build surface, no network, nothing
to configure. That covers every tree whose own CI already builds it, which is
what the memql-project template produces. The Build stop draws itself skipped,
with the reason: its built output is in the source.

A deployable that needs a build in-cluster is **refused with a typed message
naming the command it would have run**, and the two ways forward: commit the
built output, or wait for the workbench binding. The reasoning is in
`component/packages/builder.go` and it is short: a package's build script is
somebody else's code, `npm ci` runs whatever a dependency put in a
`postinstall` hook, and the sandbox that exists for exactly this
(`workbench_use`) reaches its isolation through a per-Plan workspace that a
package deploy does not have. Running it in the engine process instead would
deliver the feature by deleting the property that made it safe.

Workbench builds are the Build epic of
[the Deployables program](../../superpowers/specs/2026-09-02-deployables-program-design.md).
They change what the Build stop SAYS -- progress on the surface that built it,
and the log -- not where the stop is.

## Where the pieces live

| Piece | Path |
|---|---|
| Manifest, analysis, refusal catalogue | `component/packages/` |
| The Init-grade DSL gates, isolated | `component/memql/package_gates.go` |
| The pipeline and its stages | `component/packages/pipeline.go`, `stages.go` |
| Source credentials and the two probes | `component/packages/credentials.go`, `probe.go` |
| Update feeds | `component/packages/feeds.go` |
| Concepts, queries, mutations, capabilities | `dsl/platform/` |
| The D10 lifecycle law | `component/memql/platform_site_status_guard.go` |
| The boot-time fetcher | `subcommand_dsl_fetch.go`, `component/packages/fetch_active_set.go` |
| The kustomize component | `deploy/k8s/components/dsl-packages/` |
| The surface | `clients/os/src/apps/deployables/` |
