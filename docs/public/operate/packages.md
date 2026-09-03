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

Design record:
[2026-09-01-packages-and-deployables-design.md](../../superpowers/specs/2026-09-01-packages-and-deployables-design.md)
(epic memql#4794). This is the operator's half.

## The short version

1. Add a package: a GitHub URL, or a zip already in Files. Nothing deploys.
2. Deploy it. The cluster **analyzes first** and shows you what it found.
3. Confirm. It builds, stages, rolls and publishes -- in that order, always.
4. The repo stays the source of truth: when something newer lands, the package
   says so, and deploying the update is one click that starts the same run
   again at the new version.

The surface is the MemQL OS Deployables app, Packages section. The portal is
maintenance-only.

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
clusters without an edit.

A `bff/` with a `go.mod` is **detected, reported and deferred**: it appears in
the report saying where Go delivery happens today (engine images built by CI),
and every other half of the package deploys around it.

## What the analysis checks, and what it refuses

The analysis runs **offline**, before anything is fetched to a workbench or
written to storage, and it runs **the same gates a node runs at boot**. "This
DSL would refuse boot" is an answer you get here, not a crashlooping pod later.

Every refusal is a stable machine-readable code
(`component/packages/refusal.go`). The OS renders its own sentence for the ones
it knows and the server's own sentence verbatim for the rest.

| Code | What it means |
|---|---|
| `package_manifest_missing` | No `memql-package.yaml` at the tree root |
| `package_manifest_invalid` | Unparseable, an unknown key, a missing name, an unknown `formatVersion`, or two deployables sharing a name |
| `deployable_path_missing` | A declared `path` is not a directory in the tree |
| `deployable_kind_unknown` | `kind` is a value nobody has heard of -- not one of the three live values, and not one of the known-but-unoffered ones below |
| `deployable_target_not_offered` | `kind` is one the target model knows and does not offer yet (`ios`, `android`, `macos`). Scoped to that app and **not fatal** -- the app is reported with "iOS is not offered on this cluster yet" and the rest of the package deploys around it |
| `deployable_binding_missing` | A storefront with no `binding`; also a never-deployed app whose placement names no hostname |
| `dsl_domain_reserved` | A `dsl/<domain>/` whose name the engine already owns |
| `dsl_refuses_boot` | The package's DSL does not survive the Init-grade gates; carries the construct-level errors |
| `source_too_large` | Over the per-file, whole-tree or file-count cap |
| `source_unreadable` | Not an archive this cluster can open, or a repository or a GitHub it cannot reach |
| `source_host_unsupported` | The repository (or a credential) names a host this cluster does not fetch from -- only github.com today, or upload a zip of the tree instead. The source probe answers it as a reason rather than an error |
| `credential_not_found` | The package names a credential its OWNER cannot read: it does not exist, or it belongs to somebody else. Refused by name, before any request leaves the cluster |
| `credential_revoked` | The credential the package fetches under was revoked; every source fetching under it refuses until it is switched to another one |
| `bundle_path_invalid` | An archive entry escaping the package root |
| `go_pack_not_deployable` | Reported, **not fatal** -- the rest of the package deploys |
| `dsl_requires_cluster_owner` | A DSL-carrying deploy by a non-cluster-owner |
| `package_has_active_deployables` | Archiving a package while one of its apps is still serving (`live`). Pause it first; every paused or never-published app is archived with the package |

An **unknown `formatVersion` refuses** rather than parsing the subset it
recognises, and so does an **unknown KEY**: `deployabels:` parses fine and
describes a package that deploys nothing while reporting success.

## Who may deploy what

One UX, gated by CONTENTS:

- A package of **web apps only** deploys under the caller's own authority, and
  the sites it creates are theirs (the memql#4344 per-user ownership model).
- A package that ships **any MemQL DSL** requires the **cluster-owner** actor,
  and is refused with `dsl_requires_cluster_owner` **at the start of the run**
  -- before any build and before anything is staged.

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
lands in seconds. The Packages surface draws those skipped stages with their
reason rather than omitting them, so a fast deploy is legible as a fast deploy.

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
  that package's own named secret.

An upstream that has NOT moved is not written, so the OS cue does not flicker
and the mesh does not carry an event per package per poll.

## Private repositories

`repoTokenRef` **names** a `v1:platform:globalSecret`; it never holds a token.
The value is resolved at the moment of a fetch and lands on no row, no
snapshot and no log line. Store the token once:

```
memql secret set acme-repo-token <the token>   # or the Settings surface
```

then put `acme-repo-token` in the package's secret-name field. Leave the field
empty for a public repository.

## The lifecycle, and the archive

```
draft -> live <-> disabled -> archived
```

- **Archive requires disabled first.** Archiving is the end of a deployable's
  life, and pausing is the step that gives anyone still using it a chance to
  notice.
- **Archived restores to disabled**, never straight to live: publishing again
  is its own decision.
- **An archived site answers 404.** To the internet it is gone, not paused.
- **An archived row stays listed** behind the Archived filter. An archive is a
  place, not a void -- which is exactly what the older soft-delete was not.
- **A package refuses to archive while any of its sites is not archived**
  (`package_has_active_deployables`).
- **The cluster's own portal and OS sites are exempt from all of it.** They are
  re-seeded live at every boot; a status write on one is refused whoever asks,
  including a cluster owner, and the OS renders no controls on them at all.

Archiving asks you to type the name or hostname. The **server verifies it**,
not just the form.

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

## Builds: what ships today

A deployable whose built output is **already in the source** (`dist/index.html`
present) deploys with no build at all -- no build surface, no network, nothing
to configure. That covers every tree whose own CI already builds it, which is
what the memql-project template produces.

A deployable that needs a build in-cluster is **refused with a typed message
naming the command it would have run**, and the two ways forward: commit the
built output, or wait for the workbench binding. The reasoning is in
`component/packages/builder.go` and it is short: a package's build script is
somebody else's code, `npm ci` runs whatever a dependency put in a
`postinstall` hook, and the sandbox that exists for exactly this
(`workbench_use`) reaches its isolation through a per-Plan workspace that a
package deploy does not have. Running it in the engine process instead would
deliver the feature by deleting the property that made it safe.

## Where the pieces live

| Piece | Path |
|---|---|
| Manifest, analysis, refusal catalogue | `component/packages/` |
| The Init-grade DSL gates, isolated | `component/memql/package_gates.go` |
| The pipeline and its stages | `component/packages/pipeline.go`, `stages.go` |
| Update feeds | `component/packages/feeds.go` |
| Concepts, queries, mutations, capabilities | `dsl/platform/` |
| The D10 lifecycle law | `component/memql/platform_site_status_guard.go` |
| The boot-time fetcher | `subcommand_dsl_fetch.go`, `component/packages/fetch_active_set.go` |
| The kustomize component | `deploy/k8s/components/dsl-packages/` |
| The surface | `clients/os/src/apps/deployables/packages/` |
