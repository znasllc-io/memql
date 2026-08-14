---
title: VS Code Deployments Surface -- Instances, Runs, and the Plugin/Portal Split
audience: internal
status: design approved; ready for implementation planning
area: design
date: 2026-08-14
owner: znas
surface: VS Code extension (editors/vscode)
---

# VS Code Deployments Surface

Give the extension a Deployments view whose top level is a memQL **instance**
and whose second level is a **run** that changed that instance's deployed
state. Move the install/repair/uninstall entry points out of the Clusters `+`
menu into it. Delete the topology view, because topology is cluster state and
cluster state belongs to the portal. Leave Clusters as what its name says:
connections.

This is sub-project **C** of a four-way decomposition. The other three are
named in [Relationship to the other sub-projects](#relationship-to-the-other-sub-projects)
and are explicitly out of scope here.

---

## 1. Problem

Four failures, each observed in the tree rather than assumed.

1. **Installing a cluster is filed under "add a cluster".** The 15-step
   install graph (`scripts/install/graph/install.json`) is reached from the
   Clusters `+` quick pick. Installing memQL on a machine and registering a
   connection to one are different acts with different failure modes, and
   filing the first under the second makes the destructive one an incidental
   branch of the benign one.

2. **The plugin renders cluster state the portal owns.** `memql.cluster.open`
   ("Open Cluster Topology and Deployments") opens an 894-line webview drawing
   a pod grid, orphan and under-replica verdicts, and deployment history.
   `clients/portal/src/` already carries `cluster/` and `deploy/`. Two
   surfaces answering one question diverge on the day the second one ships.

3. **A local cluster that exists cannot be reconnected without re-typing it.**
   `memql.clusters.remove` correctly removes only the registry row -- the
   cluster keeps running -- but nothing says so, and getting back requires
   walking the add-a-cluster form and re-entering values the machine already
   knows. `src/clusters/presence.ts` computes the evidence
   (`installed-healthy` / `installed-unreachable`) and no action consumes it.

4. **There is no history of what was deployed.** The install receipt is a
   single current-state document; `v1:cluster:deployment` rows exist only for
   remote clusters. An operator cannot answer "what happened to this instance,
   and when".

---

## 2. Constraints discovered in the tree

Findings, not assumptions. Each one closed off a direction.

### 2.1 `deploycontrol` refuses local clusters, by design

`component/deploycontrol/driver.go:35` rejects the `docker-local` provider with
*"local clusters are operated via `make up` (k3d + ArgoCD), not the deploy
console"*, and `ConsoleEnvFor` maps `development` to `""`. A local instance has
no `v1:cluster:deployment` rows and will not acquire any on this path. Any
design that gives local and remote instances the *same* children is wrong.

### 2.2 The orchestration script this repo shells out to is gone

`992deb41` ("remove the product deploy/release estate") deleted
`scripts/release/promote.sh`, `releases/`, and the ArgoCD Applications, moving
them to the product repo. `component/deploycontrol/executor.go:91` still joins
that path. What remains here are the capability scripts a **deploy pack**
composes (`pin-overlay-digests.sh`, `argo-sync.sh`, `revert-overlay.sh`), and
`examples/deploypack` is an example.

Consequence: **an engine-only cluster has no deploy pipeline**, and every
deploy-control action against it refuses. That is the common case for anyone
running this repo, so it is a state to render, not an error to report.

### 2.3 A wizard-installed local cluster is a pinned release checkout

`install.cloneStack` leaves a detached checkout at a release tag
(`src/install/stackPin.ts`, `v0.17.0` today) and ArgoCD reconciles the local
overlay from it. So a local instance genuinely has a *version*, and moving it
to another tag is a coherent deployment -- which is what makes one uniform
"a deployment moves an instance to a version" model possible at all.

`stackPin.ts` also argues, at length, why the tag is a reviewed pin rather than
"the newest tag". That reasoning carries into §5.3.

### 2.4 The install receipt is not a run log

`src/install/receipt.ts` documents two load-bearing properties: it is rewritten
atomically after **every** step, and it preserves the pre-existence verdict that
stops an uninstall deleting a developer's own k3d cluster. It is one document
describing what is on the machine **now**, and it is uninstall's input. It
cannot also be a history without making that question ambiguous.

### 2.5 The TS SDK exposes no `asOf`

`v1:cluster:deployment` is an append-only timeline -- status transitions append
payload versions under one id -- but `sdk/ts/src/client/` has no `asOf`. A
remote run's transitions are therefore not readable from the extension. Its
readable body is its per-tier `v1:cluster:deploymentNodeSpec` rows.

### 2.6 `src/state/` and `src/deploy/` may not import `vscode`

Enforced mechanically by `cmd/memql-lsp/vscodeimportrule_test.go`. It is what
lets an operator's whole path through an install run under bare `node --test`
with no workbench and no cluster. Every new pure module here obeys it.

### 2.7 The renderer is view-kit, not React

`@znasllc-io/memql-view-kit` returns an HTML string, has no DOM dependency and
no inline event handlers (the webview CSP forbids them). It already ships
`renderChecklist` and `install.ts`. New panels render this way.

---

## 3. The boundary rule

One rule, stated once, from which every decision below follows:

> **The plugin owns what is on your machine and what you can reach.
> The portal owns what is inside a cluster.**

| Question | Lives in | Surface |
|---|---|---|
| What instances do I operate, at what version? | machine + registry | plugin -- Deployments |
| Install / upgrade / repair / uninstall / roll out | machine + deploy-control bridge | plugin -- Deployments |
| Which clusters can I reach, as whom? | `clusters.yaml` + SecretStorage | plugin -- Clusters |
| Which pods run, which are orphaned, which tier is under-replicated | cluster state | **portal** |
| Integrations, identity, sites, accounts | cluster state | **portal** |
| What does this construct do, what rows exist | cluster state, but *authoring* | plugin -- Concepts / Runs |

Topology is cluster state. That is why it goes, and the rule is the
justification -- not a preference about which UI is nicer.

Views after this work:

```
memQL (activity bar)
├── Deployments   NEW
├── Clusters      slimmed to connections
├── Concepts      unchanged
└── Runs          unchanged
```

---

## 4. Data model

### 4.1 Instance -- derived, not declared

```ts
interface Instance {
  name: string;               // clusters.yaml slot key, or "local"
  kind: "local" | "remote";
  domain?: string;
  presence: PresenceVerdict;  // absent | installed-healthy | installed-unreachable
  version?: string;           // local: the pinned tag, from the receipt's stackCheckout entry
                              // remote: current deployment's version, via deploymentHistory
  connected: boolean;
}
```

Local is resolved by `src/clusters/presence.ts` (install receipt + a
`local: true` registry entry + the WebSocket probe). Remote is every other
`clusters.yaml` entry.

**Why there is no new registry file.** The install graph is local-only --
Docker, k3d, mkcert, a marked block in the system hosts file. The plugin cannot
create a remote instance, so a "declared but not installed" remote row would be
a row you can only look at. And one local install per machine, because the
receipt path, the hosts block and the k3d cluster name are each singular.

### 4.2 Run -- one shape, two sources

```ts
interface Run {
  id: string;
  instance: string;
  kind: "install" | "upgrade" | "repair" | "uninstall" | "rollout";
  fromVersion?: string;
  toVersion?: string;
  startedAt: string;
  finishedAt?: string;
  status: "running" | "succeeded" | "failed" | "cancelled"
        | "superseded" | "rolled_back";      // the tail is remote-only
  items: RunItem[];
}

interface RunItem {
  label: string;   // an install-graph step id, or a nodeType
  status: "pending" | "running" | "ok" | "failed" | "skipped" | "preserved";
  detail?: string;
  at?: string;
}
```

The asymmetry is **stated, not hidden**: a local run's items are capability-script
executions; a remote run's are per-tier specs (§2.5). They share a header and a
renderer; they are not the same granularity, so the panel labels them "Steps"
and "Node types" respectively.

The two sources also differ in where a run's identity comes from and who
writes it. A **local** run gets a plugin-minted `id` and is written to the run
log (§4.3) by the extension as the run proceeds. A **remote** run's `id` is the
`deploymentId` and nothing local is written at all -- it is read from
`v1:cluster:deployment` and `v1:cluster:deploymentNodeSpec` as ordinary concept
rows, the same path `deploymentHistory.ts` already uses.

`preserved` is carried over from the install executor's six-state model. It
means the uninstall kept something the operator already had, and it cannot be
folded into success or failure.

### 4.3 The local run log -- a new artifact

`~/.memql/runs/<runId>.json`, one file per run, rewritten atomically per step
via temp-file-and-rename, pruned to the 50 most recent on write. History is a
directory listing.

That is exactly the receipt's discipline, deliberately: a run killed at any
point leaves a record naming precisely the steps that completed.

**It is not appended to the receipt** (§2.4). Two documents, two questions:
the receipt says what is on this machine now, the run log says what happened.

---

## 5. The Deployments surface

### 5.1 Tree

Instances at the top, runs beneath, newest first.

```
DEPLOYMENTS
├─ local  ● healthy · v0.17.0
│  ├─ upgrade   v0.16.1 → v0.17.0   succeeded   2d ago
│  └─ install                        succeeded   9d ago
└─ staging  ● healthy · v0.9.2
   ├─ rollout v0.9.2   succeeded     1d ago
   └─ rollout v0.9.1   rolled_back   3d ago
```

An instance with no runs renders with its actions and no children -- not as an
empty state, because "installed, never upgraded" is the normal case.

A machine with no local cluster still shows the `local` row, as
`○ not installed`, carrying `Create deployment` as its only action. That row is
the entry point the Clusters `+` menu used to hide, and it is why the view is
useful on a machine where nothing has been installed yet.

### 5.2 Actions by instance state

| Action | local, absent | local, installed | remote |
|---|---|---|---|
| Create deployment | full install graph | `stackCheckout`@tag + `clusterUp` | deploy-control `Deploy` |
| Repair | -- | re-run graph (verify-then-skip) | -- |
| Uninstall | -- | uninstall graph + preview | -- |
| Cut version | -- | -- | deploy-control, developer+ |
| Promote | -- | -- | deploy-control, admin+ |
| Rollout action | -- | -- | deploy-control, admin+ |
| Rollback | -- | -- | deploy-control, owner only |

Local repair and uninstall are the existing `memql.clusters.{repair,uninstall}`
**re-parented, not rewritten**.

Re-running the install graph *is* the repair: every step verifies first and
skips when already satisfied.

### 5.3 Choosing a target version

`git ls-remote --tags` against the checkout's origin, filtered to semver tags,
newest first; degrading to a free-text field when the network is unavailable.

It never auto-selects the newest. §2.3's pin argument applies with equal force
when the operator is choosing: a version somebody picked off a list is a fact
they can be held to, and one the plugin picked silently is not.

### 5.4 Three deploy-pipeline states, none of them errors

The instance page probes `GetDeploymentStatus` when it opens and renders
exactly one of:

- **pipeline present** -- the actions in §5.2;
- **no deploy pipeline configured** -- the engine's own refusal text (§2.2);
- **status not visible at your role** -- `GetDeploymentStatus` is owner/admin
  gated (#728, parity preserved by memql#3311), so a developer legitimately
  cannot see it while still seeing history, which is ordinary concept rows.

A row of buttons that turn out to be refused would be the error. Naming the
state is not.

---

## 6. The Clusters surface

### 6.1 What changes

- The tree keeps its shape and gains an inline **Open Portal**.
- `memql.clusters.uninstall` and `memql.clusters.repair` leave the Clusters
  context menu for Deployments.
- `memql.clusters.remove` keeps its title ("Remove Cluster From List") and
  gains a confirmation which, for a local instance, reads: *"This removes the
  connection only. The cluster keeps running -- uninstall it from
  Deployments."* That sentence belongs in the dialog, not in documentation.
- `src/webview/clusterPanel.ts` is deleted and replaced by
  `src/webview/connectionPanel.ts`.

### 6.2 The connection page

```
Cluster: staging                        [Open Portal ↗]

  Connection
    endpoint   api.staging.example.com:443   ● reachable
    issuer     identity.staging.example.com
    probed     4s ago · 84ms

  Identity
    signed in as  znas@znas.io
    role          owner
    token         expires in 11m  (auto-renews)

  [Sign out]  [Disconnect]  [Edit]  [Remove from list]
```

Nothing here overlaps the portal, which knows nothing about `clusters.yaml` or
VS Code SecretStorage. This is the surface for diagnosing why a cluster will
not come up.

### 6.3 Auto-connect after install

On a successful install run the runner writes the `local` entry into
`clusters.yaml` (`local: true`, domain from the run's own params, endpoint via
`composeEndpointFromDomain`), marks it `selectedCluster`, and hands off to the
sign-in the install has already minted an enrolment link for. Nothing is
re-typed.

### 6.4 Reconnect with zero questions

The `+` menu already renders from `presence.ts`'s verdict. One action is added,
offered when the verdict is `installed-healthy` or `installed-unreachable` and
no `local: true` entry exists:

**"Connect to the local cluster"** -- composes the registry entry from the
receipt, falling back to `stackPin.DEFAULT_LOCAL_DOMAIN` when the receipt is
gone (the hand-built `make up` case), writes it, connects, and runs sign-in.
No form at any point.

### 6.5 Open Portal

When connected, read the portal's `systemOwned` `v1:platform:site` row and open
its hostname. Otherwise compose `https://api.<domain>/portal/`, which is where
`component/genesis/domain.go:72` puts it today.

Reading the row rather than hard-coding the path is what keeps this correct
when memql#3711 moves the portal to its own origin.

---

## 7. Module layout

```
src/state/deployments.ts         instance + run model            pure
src/state/runLog.ts              per-run record read/write       pure
src/deploy/instanceActions.ts    action catalog + enablement     pure
src/views/deploymentsTree.ts     the tree
src/webview/deploymentPanel.ts   instance page + create flow
src/webview/connectionPanel.ts   replaces clusterPanel.ts
src/webview/installScreens.ts    collect/running/failedStep, shared
```

**Reused unchanged:** `src/install/{graph,executor,receipt,runner,session,
removalPreview,stackPin}`, `src/clusters/presence.ts`,
`src/state/deploymentHistory.ts` (for `indexDeployments` /
`resolveTierVersion`), `src/deploy/{actions,enablement}.ts`.

**Deleted:** `src/state/topology.ts` (281 lines), `src/webview/clusterPanel.ts`
(894 lines), the `memql.cluster.open` command and its menu contributions.

`installScreens.ts` exists because `addClusterPanel.ts` keeps its `connect`
screen for remote registration while Deployments needs `collect` / `running` /
`failedStep`. Lifting the three shared screens is what stops the 2234-line
wizard being duplicated.

---

## 8. Errors, and what is not a gate

`src/deploy/actions.ts` already states the doctrine and it is extended, not
amended: the extension's role table mirrors the engine's tiers as a
**courtesy**, never a control. A real refusal arrives from the engine naming
the role required. Presence and health decide what is *offered*; a capability
script's result envelope decides what *happened*.

States that must render rather than throw:

| Condition | Renders as |
|---|---|
| `clusters.yaml` unreadable | the existing synthetic error row |
| Docker not running | the `detect` step failing, in the panel, with its guidance |
| Receipt present, cluster silent | `installed-unreachable`, offering repair |
| Remote unreachable | connection page shows the probe failure; the instance still lists, version unknown |
| No deploy pack, or role too low | the two states in §5.4 |

"Version unknown" is drawn as itself and never as blank -- the rule
`topology.ts` established for derived versions, carried forward even as that
file goes.

---

## 9. Testing

- **Pure modules** (`state/deployments.ts`, `state/runLog.ts`,
  `deploy/instanceActions.ts`) under bare `node --test`: no workbench, no
  cluster. `cmd/memql-lsp/vscodeimportrule_test.go` keeps them that way
  mechanically.
- **Panel wiring** under the existing `npm run test:host` harness.
- **Run-log crash safety**, explicitly: kill a run mid-flight, reopen, and the
  record names exactly the steps that completed. The receipt's own property,
  restated for runs, because it is the reason for the temp-file-and-rename.
- **Deletion guards**: `package.json` no longer contributes `memql.cluster.open`,
  and the Clusters context menu carries no install / uninstall / repair. A
  deletion nothing guards grows back.

---

## 10. Relationship to the other sub-projects

This spec is **C** of four, decomposed from the 2026-08-14 brainstorm.

| | Sub-project | Status |
|---|---|---|
| **A** | Constructs view + the readable input/output viewer | separate spec |
| **B** | Training in the editor (untrained/drifted flagging, read-only rules, promote) | separate spec, depends on A |
| **C** | **Deployments view + the plugin/portal split** | **this spec** |
| **D** | Virtual staging/production inside one cluster | separate spec |

**What C deliberately does not do.**

- **It does not build a deploy pipeline.** §2.2 found the orchestration gone
  from this repo; C drives what the target cluster has and names the state when
  it has none. Fixing the pipeline is D's, and D would rewrite anything built
  here.
- **It does not add an environment axis.** Promoting a site or SPA from staging
  to production needs one on `v1:platform:site`, which has none today. That is
  D.
- **It does not touch scaling.** `make scale` locally and replica counts
  remotely are both D.
- **It does not add a Constructs view.** A is where "what has this cluster
  loaded" lands, and it needs a construct-grain listing RPC that does not exist
  (the pack browser is file-grain).

**What A and B will consume from C.** The boundary rule in §3 is the one they
must respect -- it is what says a construct browser belongs in the plugin while
a pod grid does not. `installScreens.ts` and the run renderer are also the
precedent for how a long-running, step-shaped operation renders, which B's
promote flow will follow.
