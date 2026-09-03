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
| `deployable_kind_unknown` | `kind` is not one of the three live values |
| `deployable_binding_missing` | A storefront with no `binding` |
| `dsl_domain_reserved` | A `dsl/<domain>/` whose name the engine already owns |
| `dsl_refuses_boot` | The package's DSL does not survive the Init-grade gates; carries the construct-level errors |
| `source_too_large` | Over the per-file, whole-tree or file-count cap |
| `source_unreadable` | Not an archive this cluster can open, or a repository it cannot reach |
| `bundle_path_invalid` | An archive entry escaping the package root |
| `go_pack_not_deployable` | Reported, **not fatal** -- the rest of the package deploys |
| `dsl_requires_cluster_owner` | A DSL-carrying deploy by a non-cluster-owner |
| `package_has_active_deployables` | Archiving a package whose sites are not all archived |

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
  runbook describes.
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
| `MEMQL_PACKAGES_BUILD_TIMEOUT_SECONDS` | `900` | How long ONE deployable's build may run before it is stopped, process group and all |
| `MEMQL_PACKAGES_HEARTBEAT_SECONDS` | `15` | How often a running deploy says its node is alive |
| `MEMQL_PACKAGES_ABANDONED_AFTER_SECONDS` | `90` | How stale that heartbeat must be before the sweep closes the run `abandoned`. Clamped up to three heartbeats |
| `MEMQL_PACKAGES_BUILD_UID` | `10001` | The uid a build's command runs as. `0` runs it as the engine's own user, which is an explicit choice |

A cap that is set and unparseable, or non-positive, falls back to its DEFAULT
rather than to "no limit".

`MEMQL_PACKAGES_ROLL_TARGETS` is deliberately not defaulted: a roll restarts
pods, so its blast radius is a decision somebody made rather than whatever
happens to be running. Only clusters hosting DSL-carrying packages need it.

## Builds

A deployable whose built output is **already in the source** (`dist/index.html`
present) deploys with no build at all -- no build surface, no network, nothing
to configure. That covers every tree whose own CI already builds it, which is
what the memql-project template produces, and it is still the fastest path.

Everything else **builds on the workbench** (epic memql#4900).

### Where a build runs, and what it can see

A package's build script is somebody else's code: `npm ci` runs whatever a
dependency put in a `postinstall` hook. So the command never runs in an engine
process. It runs on a **workbench node**, in a directory that exists for the
length of one build, under an environment this cluster **constructs**:

| The build gets | It does not get |
|---|---|
| `PATH`, a `HOME` and `TMPDIR` inside its own directory, the locale, `CI=true`, npm's cache pointed inside the directory | Any variable from the node's own environment -- the database DSN, the master key, the storage connection string, every vendor key |
| `MEMQL_BUILD_DEPLOYMENT_ID` and `MEMQL_BUILD_DEPLOYABLE`, so a build can name itself in its own logs | A cluster credential of any kind, in any form |

Constructing the environment covers what the command is **handed**. It runs as
**uid 10001** as well, which covers what it can go and **read**: the engine runs
as root, and a build running as root could read `/proc/1/environ` and take
everything the pod holds. `MEMQL_PACKAGES_BUILD_UID` names the uid.

That is a boundary, not a jail. A build can still reach the network and spend
the pod's CPU; those are bounded by the pod's own limits, by the timeout, and by
the directory being destroyed when the call returns -- whether the build
succeeded, failed or timed out.

### The image

The `workbench` node type is the only one whose image carries a Node toolchain
and `git` (the `workbench-runtime` stage in the `Dockerfile`). Node comes from
the same pinned `node:22` image this repo builds its own SPAs with, so a package
builds against the toolchain the platform's own bundles do.

### Which node builds

The bff serves `packageDeploy`, and it forwards the build to a workbench replica
over `NodeService.Stream` -- `MEMQL_WORKBENCH_REMOTE=1` plus
`MEMQL_WORKER_PEERS=workbench=workbench:50060`, both set in
`deploy/k8s/components/engine-bff`. **With no reachable workbench peer a build is
refused** (`no_workbench_peer`), never run on the bff: running somebody else's
build script in the process holding the cluster's front door is not the
isolation that flag asks for.

The forward carries a **system-class assertion** -- this cluster's engine acting
for itself -- and the workbench refuses a build request carrying any other. That
is what keeps the build entry unreachable from an agent's tool loop, which
re-asserts its own caller's class and can never mint a system one.

The apps of one package prefer the **same replica**: the first app's node is
passed as the second's affinity pin, so one run reads as one place in the logs.
`builtOn` on the deployment row records the surface and the node.

### When a build goes wrong

| Code | What it means | Whose repair |
|---|---|---|
| `deployable_build_failed` | The command exited non-zero. The log tail is on the row | the author's |
| `deployable_build_timeout` | The command outlived `MEMQL_PACKAGES_BUILD_TIMEOUT_SECONDS` and was stopped, process group and all | the author's, or an operator raising the cap |
| `build_output_missing` | The command succeeded and wrote no output directory | the author's |
| `no_workbench_peer` | This cluster has no workbench to build on | an operator's |

Every one of them leaves **every site serving exactly what it was serving**: the
build stage is before publish in the D6 order.

### Builds on your own machine

A target can declare that its kind builds on a machine in the owner's own Fleet
-- an iOS or macOS build needs Xcode, which needs macOS. The route ships as the
**selection mechanism and its hop test, with no registered kind that reaches
it**: `targets.go` maps every offered kind to the workbench, and a test pins
that. What is real is choosing the machine (by exact label, under the owner,
with `no_worker_available` naming every machine considered and why each was
ruled out); what is deliberately refused is the dispatch, because building on
somebody's laptop is a computer-use act and a deploy has no approved plan to
hang that consent on. The first target that needs a Mac brings that decision
with it.

## When a node is lost mid-deploy

A deploy is one call on one node. Until epic memql#4900 a node lost mid-run left
its row at a non-terminal status forever, and the surface said "building" for as
long as anybody looked.

A running pipeline now writes `heartbeatAt` every
`MEMQL_PACKAGES_HEARTBEAT_SECONDS`, and a sweep every two minutes closes any run
whose heartbeat is older than `MEMQL_PACKAGES_ABANDONED_AFTER_SECONDS` with the
terminal status **`abandoned`**.

- **Never `failed`.** The sweep does not know whether the build was about to
  succeed; it knows the node stopped answering. The error it stamps names the
  node and when it was last heard from, and the OS says "this cluster lost the
  node that was running it; nothing was published".
- **The threshold is six heartbeats, not two.** A run that misses one had a slow
  moment. Six in a row is a node that is gone. The threshold is clamped up to
  three heartbeats if configured below that, because a cluster that sweeps its
  own healthy deploys presents as a broken build surface rather than as a
  setting.
- **Retry deploys what the lost run was deploying.** `packageDeploy` takes
  `fromDeploymentId`, and a run started that way re-analyses the snapshot the
  earlier run stored rather than fetching the source again -- so a Retry does not
  quietly deploy whatever the branch has moved to since. A run that kept no
  snapshot (every run from before this epic) is refused with
  `snapshot_unavailable`, and the sentence says to deploy again instead.

The sweep runs on the **cron leader only**, so each stranded row is closed once,
and under the maintenance actor, because it reads every owner's runs.

## Auto-deploy

A per-source switch: **deploy the update by itself when the plan is unchanged**.
Off by default and off for every source that has never been switched.

With it on, the update feeds start the run they have never been allowed to start
-- requested by the source's **owner**, marked `automatic` on the row -- and the
run **confirms itself only when the new analysis plans exactly what the last
successful deploy planned**: the same apps by name, kind, path, build command and
output directory, the same MemQL domains. Anything else parks at the confirm
gate exactly as a person's deploy does, and the surface says why.

The gate is not skipped. It is answered, and only by a plan somebody already
said yes to. A changed build command is the case that matters most -- it is
somebody else's shell command arriving on your cluster -- and it always parks.

Two other rules:

- **Never more than one auto-run live per source.** Two pushes seconds apart
  compose the same deployment id, so the second lands on a row that already
  exists and the append-only rule refuses to reopen it.
- **A cluster-owned source cannot auto-deploy.** There is nobody to run as.

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
| The build surface's entry | `integrations/workbench/build.go` |
| The binding, and the two translations | `component/packages/build_workbench.go` |
| The abandoned sweep and the heartbeat | `component/packages/sweep.go` |
| The auto-deploy switch and the plan comparison | `component/packages/autodeploy.go` |
| The Fleet route (a seam, with no consumer) | `component/packages/fleetbuild.go` |
| The surface | `clients/os/src/apps/deployables/packages/` |
