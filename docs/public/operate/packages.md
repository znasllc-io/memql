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

**Confirming ADVANCES that run** -- `packageDeploy(confirm: true, deploymentId:
<the parked run>)` -- rather than starting a second one, and the resumed run
re-reads the snapshot it already stored (memql#4954, memql#4955). One row per
attempt is what makes the timeline readable, and it is what makes a Retry's
promise survive the click that acts on it.

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

### Applying the components

Three components, and the order is load-bearing:

```yaml
# The layer that LABELS -- its own kustomization, consumed as a resource.
# labels/patches run AFTER components, so a `labels:` block beside the
# `components:` block below would be applied too late and every patch would
# match nothing (memql#4933). It renders, it applies, every pod is healthy,
# and not one of them has an init container or MEMQL_DSL_PATH.
patches:
  - target: { kind: Deployment, name: "^(bff|agent|cognition|planner|workbench)$" }
    patch: |
      - op: add
        path: /metadata/labels/memql~1product-dsl
        value: "true"
```

```yaml
# ...and the layer that consumes it.
components:
  - ../../components/dsl-mount         # the shared volume, mount and MEMQL_DSL_PATH
  - ../../components/dsl-bundle        # optional: a product's own DSL image
  - ../../components/dsl-packages      # the fetcher for staged package DSL
  - ../../components/packages-roll-rbac # permission to restart the roll targets
```

`dsl-mount` owns the shared substrate and is applied **exactly once**;
`dsl-bundle` and `dsl-packages` each add one init container, and their order in
the list is the order those containers run. Both used to build the substrate
for themselves, which made them mutually exclusive -- two volumes of one name
is a Deployment the API server refuses -- and made `dsl-packages` silently
dependent on `dsl-bundle` having created the `initContainers` list first.

Two committed examples render the working shapes and are gated by
`deploy/k8s/components/dsl-mount/component_test.go`:
`components/examples/dsl-packages-only` and
`components/examples/dsl-bundle-and-packages`.

**Label only the nodes that load DSL.** A blanket label reaches `redis`, which
has no `volumes:` key, and the first patch fails the whole render.

### Two things the fetcher and the roll each need

**The fetcher needs to know which blob container to read.** It reads
`MEMQL_AZURE_BLOB_CONTAINER`, which is not a secret and is not in
`memql-secrets`; the component takes it from an **optional ConfigMap named
`memql-storage`**:

```yaml
apiVersion: v1
kind: ConfigMap
metadata: { name: memql-storage, namespace: memql }
data: { MEMQL_AZURE_BLOB_CONTAINER: "memql" }
```

Without it the fetcher has nowhere to look, and says so and refuses -- it used
to answer "no package pointer" instead, which is the ordinary state of a
cluster with no packages, so the node booted healthy serving none of its
packages' DSL.

**The roll needs permission to restart pods.** It patches each workload named
by `MEMQL_PACKAGES_ROLL_TARGETS` as the node's own ServiceAccount
(`memql-engine`), which by design holds no Kubernetes API privilege. The
`packages-roll-rbac` component ships a Role pinned by `resourceNames` plus its
binding. **Its names and `MEMQL_PACKAGES_ROLL_TARGETS` must agree** -- nothing
can check that for you, and a name in one and not the other is either a 403 at
the last stage of a deploy that otherwise succeeded, or a permission nothing
uses.

### The fetcher's exit codes are the contract

`memql dsl-fetch` runs as an init container, so its exit code decides whether
the pod starts.

- **0** -- the trees are in place, **or there is no pointer at all**. A cluster
  that has never deployed a package has no `active.json`, and that is the
  ordinary state rather than a fault; refusing to boot over it would mean the
  component could never be applied before the first deploy.
- **1** -- a pointer EXISTS and could not be honoured, **or this container was
  never told where to look**. Booting anyway would bring the node up with
  silently-missing product DSL, which presents as a healthy cluster answering
  "function not found" to every call it used to serve.

An unconfigured fetcher is the second case and not the first, and it used to be
read as the first (memql#4933). "Nothing found, nothing wrong" is the right
reading for a long-running engine process; for a container whose only job is to
fetch, it is not -- a fetcher that cannot say where to look has not found an
empty cluster, it has failed to look.

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
| `MEMQL_PACKAGES_BUILD_TIMEOUT_SECONDS` | `900` | How long ONE deployable's build may run before it is stopped, process group and all |
| `MEMQL_PACKAGES_HEARTBEAT_SECONDS` | `15` | How often a running deploy says its node is alive |
| `MEMQL_PACKAGES_ABANDONED_AFTER_SECONDS` | `90` | How stale that heartbeat must be before the sweep closes the run `abandoned`. Clamped up to three heartbeats |
| `MEMQL_PACKAGES_BUILD_UID` | `10001` | The uid a build's command runs as. `0` runs it as the engine's own user, which is an explicit choice |

A cap that is set and unparseable, or non-positive, falls back to its DEFAULT
rather than to "no limit".

`MEMQL_PACKAGES_ROLL_TARGETS` is deliberately not defaulted: a roll restarts
pods, so its blast radius is a decision somebody made rather than whatever
happens to be running. Only clusters hosting DSL-carrying packages need it.
**Every name in it must also appear in the roll Role's `resourceNames`** --
setting the variable alone leaves the roll failing on a 403, with fetch,
analyze, build and stage all green behind it. See
`deploy/k8s/components/packages-roll-rbac`.

Sealing and unsealing a source credential uses `MEMQL_MASTER_KEY`, which every
node already has; a ciphertext this node cannot unseal is refused as
`source_unreadable` naming that variable, because the repair is an operator's.

## Builds

A deployable whose built output is **already in the source** (`dist/index.html`
present) deploys with no build at all -- no build surface, no network, nothing
to configure. That covers every tree whose own CI already builds it, which is
what the memql-project template produces, and it is still the fastest path.
The Build stop draws itself skipped, with the reason: its built output is in
the source.

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
- **And it survives the confirm.** The retry parks with its report, and the
  click that answers the gate used to be a fresh call carrying no
  `fromDeploymentId` at all -- so a person retrying a lost deploy of commit A,
  after commit B had landed, got B while reading a report describing A
  (memql#4955). The retry's own row records both what it was retried FROM and
  the snapshot it ran against, and the confirmation resumes that row.

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
| The build surface's entry | `integrations/workbench/build.go` |
| The binding, and the two translations | `component/packages/build_workbench.go` |
| The abandoned sweep and the heartbeat | `component/packages/sweep.go` |
| The auto-deploy switch and the plan comparison | `component/packages/autodeploy.go` |
| The Fleet route (a seam, with no consumer) | `component/packages/fleetbuild.go` |
| The surface | `clients/os/src/apps/deployables/` |
