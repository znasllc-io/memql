---
title: Portal <-> VS Code Handoff, and the Locality Edit Policy
audience: internal
status: design approved; ready for implementation planning
area: design
date: 2026-08-21
owner: znas
surface: VS Code extension (editors/vscode) + portal (clients/portal) + memql-lsp + scripts/k3d + component/edge + sdk/ts
---

# Portal <-> VS Code Handoff, and the Locality Edit Policy

Let a person reading a construct in the portal open its source in VS Code.
Let a developer edit a local cluster's checkout and make the edit real. Keep a
remote cluster's seeded constructs read-only while new ones go in by training.

Sub-project **1** of the 2026-08-21 brainstorm. Tracked as epic memql#4242.
Sub-project 2 -- artifacts: labels + the portal Library page -- is its own
spec and is not brainstormed yet.

---

## 1. Problem

Four failures, each observed in the tree rather than assumed.

1. **There is no portal -> editor path.** The portal never names a construct's
   source anywhere: `clients/portal/src/pages/ConceptSchemaPane.tsx` renders
   fields and annotations and stops. The extension registers no `UriHandler`
   and declares no `onUri` activation (`editors/vscode/package.json`). The only
   link between the two surfaces runs the other way --
   `memql.clusters.openPortal`.

2. **The read-only rule's premise is false on a local cluster.**
   `editors/vscode/src/constructs/readonly.ts` seals every `core` file on
   every cluster, on the stated claim that a core edit "changes nothing on any
   cluster". That was true only because the extension offered no rebuild. A
   wizard-installed local cluster runs **released images pulled at a tag**
   (`clusterUp --image-tag`, derived from the checkout's pin), so the clone at
   `~/.memql/src` is inert until something rebuilds from it. `make dev` does
   exactly that, is already a capability script (`scripts/k3d/dev.sh`,
   capability `k3d.dev`: build -> k3d import -> rolling restart), and is
   reachable from no surface in the extension.

3. **The clone is invisible after install.** It exists, the receipt records
   where (`recordedCheckout`, memql#3901), and nothing opens it, names it on
   the instance row, or offers it when a construct's file is "not in this
   workspace". The repo that was installed and cloned by the extension is
   connected to nothing.

4. **A remote construct with no local file is a dead end.** The construct
   detail page (`src/webview/constructPanel.ts`) says the path "is not in this
   workspace" and stops, although the engine can serve the bytes: the pack
   browser (`ListPackDomainsMsg` / `ListPackFilesMsg` / `ReadPackFileMsg`,
   `component/grpc/pack_handlers.go`) enumerates the embedded, plugin-registered
   and `MEMQL_DSL_PATH` trees alike. `sdk/ts` carries no client for it.

---

## 2. Constraints discovered in the tree

Findings, not assumptions. Each one closed off a direction.

### 2.1 The boundary rule is already written, and it is right

The extension README and the 2026-08-14 deployments-surface design state one
rule: *the extension owns what is on your machine and what you can reach; the
portal owns what is inside a cluster.* Nothing here reopens it. This design
adds a row to its table (section 3.1) and builds on it.

### 2.2 The read-only rule is already written, and it is right too

`readonly.ts` states its one rule: *a file is read-only exactly when editing
it cannot change what the cluster runs.* It also states that the verdict comes
from the catalog's `origin` -- the ENGINE's answer -- never from the shape of a
path, and that the marking is a courtesy while `PromoteAuthoredConstruct` is
the control. All three stand. What moves is the consequence table, because
the premise under one of its rows moves (section 3.2).

### 2.3 The language server owns parsing and state

`cmd/memql-lsp/training.go` computes the training state of every construct in
an open document by hashing its source and comparing against the catalog the
extension pushes in over `memql/clusterCatalog`; the extension renders and
never derives a state. Today `seeded` stays `seeded` whatever the hash says,
reasoned from action sets: "the states exist to pick an ACTION SET, and
`seeded` has none". Section 3.4 gives it one, which is what changes the
answer.

### 2.4 Image overrides are written at Application creation, and `k3d.dev` does not touch them

`scripts/k3d/up.sh` emits `spec.source.kustomize.images` into the ArgoCD
Application YAML at creation (`kustomize_source_block`): when
`--image-registry` is set, every node image is overridden to
`<registry>/memql-<node>:<tag>`, and the database operand gets its own tag on
a different version axis (memql#4063). `scripts/k3d/dev.sh` builds
`memql-<node>:local`, imports it, and restarts the Deployments; it neither
syncs ArgoCD nor edits the overrides, because the local overlay it was written
for already names `:local`. On a wizard-installed cluster a bare rebuild
therefore imports images nothing references.

### 2.5 The receipt's "what is running" reads the install's own parameters

`recordedImageTag` (memql#4068) reads `clusterUp --image-tag` off the receipt.
A rebuild that changes what the cluster runs without recording it would make
the instance row lie, and would make the next repair -- which replays
`clusterUp` with that tag -- silently return the cluster to released images.

### 2.6 The packaged extension runs a staged copy of `scripts/` with no Go source

`src/install/root.ts`: a `.vsix` carries `<extension>/staged/scripts/...`,
shape-identical to the repository's `scripts/`, and nothing else. A build
cannot run from the staged tree; it needs a repository root to build from,
exactly as `k3d.up` already takes `--repo-root`.

### 2.7 Only the seeded tier moves by rollout

The training ladder, from the code:

| Tier | Goes live | Survives restart | Reaches other replicas | Needs a rollout |
|---|---|---|---|---|
| try in session (`AuthorSessionBundle`) | instantly, on your stream | no -- dies with the connection | no | no |
| staged (`authoring_staged.go`) | instantly, for the author only, on the node that took the call | yes (boot re-hydration) | **not until that replica's next boot** -- there is deliberately no staged broadcast; "the omission IS the tier" | no |
| promoted (`authoring_promote_durable.go`, `_propagate.go`) | instantly, for everyone | yes (`RehydratePromotedConstructs`) | **yes, within seconds** -- `authoring.promote.<bundleId>` broadcast + routing rule; every node re-hydrates the bundle from the shared DB; demote propagates the same way | no |
| seeded (core / bundle) | -- | -- | -- | **only** -- a new engine image or bundle image |

A promoted concept is also no-restart (it registers into the shared concept
registry and rebuilds relationship/node-type state in place; rows are JSONB),
and a concept cannot be staged (refused by name -- no owner-scoped resolution
path exists for one). So the split this design draws -- the training ladder is
hot, the seeded tier only moves by rollout -- is the engine's own, and
*Rebuild from checkout* is a rollout: a local one.

### 2.8 A construct's catalog identity is its registry key

`component/memql/construct_catalog.go`: `Name` is the construct's registry key,
unique per kind; for a concept that key is its CANONICAL ID
(`catalogConcepts` reports `Concept.Name`, e.g. `v1:cognition:space`). A link
needs `kind` + `name` and nothing else.

### 2.9 The portal does not know its own domain

`runtime-config.json` (`component/edge/runtimeconfig.go`) carries
`identityUrl`, `identityApiBaseUrl`, `oauthClientId`, `authEnabled` -- derived
from `MEMQL_DOMAIN`, but the domain itself is not in the document. The front
door rule (memql#3767) makes every role host a single label under the domain,
so `identity.<domain>` minus its first label IS the domain -- exact, not a
guess -- but an explicit field is the honest form.

---

## 3. The model

### 3.1 The boundary rule, plus one row

| Question | Surface |
|---|---|
| Read or edit a construct's **source** | extension -- the portal **hands off**, never renders code |

The portal renders no source, not even read-only. A person without the
extension gets an honest dead end plus the install pointer (section 4.2).

### 3.2 The read-only rule, and its new table

The rule is unchanged: *a file is read-only exactly when editing it cannot
change what the cluster runs.* With a Rebuild action, a local cluster's
checkout CAN change what that cluster runs, so the table becomes:

| origin | remote cluster | **local cluster** (workspace is its recorded checkout) |
|---|---|---|
| core | read-only, `sealed` -- changes only by an engine release | **editable -- applies on rebuild** |
| bundle | read-only, `remote` -- the cluster loads its bundle from its image | **editable -- applies on rebuild** |
| promoted / staged | editable (training) | editable (training) |
| not in the catalog (a new file) | editable -- this IS the training path | editable |

Marking remains a courtesy; the engine still enforces
(`PromoteAuthoredConstruct` refuses a core shadow). Nothing here touches the
engine's authority.

### 3.3 What "local" means

*Local* means both of: the selected cluster carries `local: true` in
`clusters.yaml`, AND the workspace folder the file lives in is that cluster's
recorded checkout (`recordedCheckout` off the install receipt, path-compared).

A different memql clone open while the local cluster is selected stays
editable -- the safe direction; it is the developer's own file -- but the
hover says so: *"this folder is not the checkout `local` rebuilds from
(`~/.memql/src`)."* Today's path-matching heuristic would call any clone "the
checkout", because every memql checkout has the same relative paths.

`ReadonlyInput` therefore gains a `workspaceIsClusterCheckout` fact beside
`clusterLocal`; `readonlyVerdict` stays a pure function and the adapter
(`readonlyDecorations.ts`) supplies both. The `ReadonlyReason` set gains no
value: on local nothing is read-only, and the *applies on rebuild* sentence is
a lens/hover, not a lock.

### 3.4 One new training state: `edited`

| state | meaning | gutter | lens (local) | lens (remote) |
|---|---|---|---|---|
| `seeded` | from disk, source matches what the cluster loaded | live | -- | -- |
| `edited` *(new)* | from disk, **your source differs** | drifted mark | **Rebuild from checkout** | "differs from what *staging* runs -- seeded constructs change by rollout" |

The gutter keeps its single question (*does what I am looking at match what
runs?*), so `edited` takes the drifted mark on every cluster; only the lens
wording varies by locality. `edited` is decided in `trainingStateFor` AFTER
the staged/promoted branches and BEFORE the seeded degrade: origin seeded +
hash mismatch -> `edited`; origin seeded + hash match -> `seeded`. Every other
state is untouched, and the corpus parity gate (memql#3758) is not involved --
the hash is the same hash.

The seven states are then `untrained / drifted / trained / staged / seeded /
edited / unknown`, and `docs/public/language/training.md` lists all seven.

---

## 4. The handoff

### 4.1 The link

```
vscode://znasllc.memql/open?v=1&cluster=<domain>&kind=<kind>&name=<registry key>
```

- `cluster` is the **domain** -- the one value the extension's add/edit flow
  collects and stores (`ClusterConfig.domain`); endpoint and issuer compose
  from it (`composeEndpointFromDomain`).
- `kind` + `name` identify the construct per section 2.8. A concept's `name`
  is its canonical id, percent-encoded like any query value.
- `v=1` is the contract version. An unknown `v` is refused, not guessed at.
- Scheme: `vscode://` by default; the button's caret offers **VS Code
  Insiders** (`vscode-insiders://`), remembered in `localStorage`.

### 4.2 Portal side

- `runtime-config.json` gains an additive `domain` field. The portal reads it
  and, when an older node omits it, derives the domain from `identityUrl` by
  stripping the `identity.` label (section 2.9).
- One affordance, `OpenInVsCode`, on the concept page header beside the
  Rows/Schema tabs: **Open definition in VS Code**. Secondary text: *Needs the
  MemQL extension for VS Code -- how to install*, linking to the extension
  README (it installs via `make vscode-install`, not from the marketplace).
- Shown to everyone who can see the page. The extension and the engine answer
  authorization; the portal does not pretend to.
- It is a component any future surface that names a construct can place; in
  this design only the concept page places it (decision D2).

### 4.3 Extension side: the handler

`activationEvents` gains `onUri`; `window.registerUriHandler` delegates to a
pure resolver under `src/handoff/` (no `vscode` import; parse, validate,
cluster-match and landing decision are unit-tested) plus a thin adapter in
`extension.ts`.

1. **Parse and validate.** Unknown `v`, a missing or malformed field, a
   `kind` outside the catalog vocabulary -> one toast naming the problem;
   stop.
2. **Match the cluster** against `clusters.yaml`: normalized `domain` equal,
   or `endpoint == composeEndpointFromDomain(domain)`.
   - none -> *"No registered cluster for acme.example.com"* with **Add
     cluster...**, which opens the existing add form PREFILLED with the
     domain. The person completes it; a link adds nothing.
   - one -> select it if it is not selected (non-modal *"Switched to
     staging"*), connect it if it is not connected (the existing sign-in
     flows, which ask what they already ask).
3. **Resolve the construct** from the refreshed catalog (`ListConstructs`).
   Miss -> *"staging has no query spaceParticipants loaded"* -- the cluster
   may have moved since the page was loaded.
4. **Land** (section 4.4).

### 4.4 Landing

ConstructPanel's three outcomes, plus the one that is a dead end today:

| situation | what opens |
|---|---|
| the file is in the open workspace | the file, revealed at the signature (existing `openFile`); read-only verdict per section 3.2 |
| **local** cluster, file not in the workspace | the cluster's recorded checkout. No folder open -> open it in this window and finish after the reload (pending handoff in `globalState`, 2-minute TTL, consumed exactly once). Another folder open -> *Open checkout in new window* / *Add to this workspace* |
| **remote** cluster, file not in the workspace | a **cluster document** (section 4.5), revealed at the signature |
| promoted / staged | the construct detail page -- its source is already rendered there from the catalog, labelled as living in the database |

### 4.5 Cluster documents

`memql-cluster://<cluster>/<originPath>`, served by a
`TextDocumentContentProvider` over `ReadPackFile` through a new `sdk/ts`
pack-browser client (three messages; the proto exists). Read-only by
construction, `memql` language for TextMate highlighting, badge *remote --
read-only*, one header CodeLens: *From staging -- Open construct details*.
Run lives on the detail page, as today; browsing a cluster does not become a
quieter way to write to production (memql#3309 still gates every mutation).

The detail page's dead end (*"... is not in this workspace"*) becomes the
action **View source from cluster** on the same provider, so the handoff and
the page share one path.

The LSP client's `documentSelector` narrows to `{ language: 'memql', scheme:
'file' }`: cluster documents must not receive import-resolution diagnostics
against files that are not on disk, and the server stays the offline,
file-based process it is designed to be (`cmd/memql-lsp/main.go`). Cluster
documents get highlighting and nothing else. That is a reading surface.

### 4.6 What a link may and may not do

May: select a registered cluster, connect it through the existing consent
flows, open a document. May never: add a cluster, sign in silently, run
anything, or write settings beyond the existing read-only marking.
`originPath` always comes from the cluster's catalog, never from the link. VS
Code's own *"Allow 'MemQL' to open this URI?"* is the consent gate; no second
one is added. Every handoff is logged to the `MemQL Connection` output channel
under the information policy (memql#4194): cluster name and construct key,
never a token, never a raw error.

### 4.7 The reverse direction

The construct detail page for a **concept** gains **Browse rows in portal**
-> `/concepts/<id>`, through the existing `portalTarget` (which reads the
portal's `systemOwned` site row when connected and composes the URL
otherwise). Small, and it makes "synchronize" two-way.

---

## 5. Rebuild from checkout, and the checkout as part of the instance

### 5.1 A local instance's image source is a mode

| mode | images | set by | instance row reads |
|---|---|---|---|
| `released` | registry at a tag (`ghcr.io/.../memql-bff:v0.17.0`) | install / upgrade / repair | `local -- healthy -- v0.17.0` |
| `checkout` | built locally from the recorded checkout (`memql-bff:local`) | **Rebuild from checkout** | `local -- healthy -- checkout abc1234 (4 uncommitted)` |

The run that sets the mode records it in the receipt (section 5.4); the
Connection page says it in words. **Crossing is never silent.** A
released-lane run (install, upgrade, repair) on a checkout-mode instance says
in its preflight *"this returns local to released v0.17.0 images"* -- a repair
must never upgrade (memql#3605) and must never silently un-rebuild either. The
Rebuild preflight on a released instance says the reverse.

### 5.2 The action

Deployments -> local, installed -> **Rebuild from checkout**. A one-step graph
(`scripts/install/graph/rebuild.json`) over the existing `k3d.dev` capability,
which gains two parameters:

- `repo-root` -- the build context. Default: the script's own repository, as
  today; the extension passes the recorded checkout. Mirrors `k3d.up
  --repo-root` (section 2.6).
- `image-source=checkout` -- a flag. Build and import as today; then, if the
  Application's `spec.source.kustomize.images` still carries NODE overrides,
  patch them out so the overlay's own `:local` references apply, and wait for
  the sync to reconcile (the same wait `k3d.up` uses); then the existing
  rolling restart. Ordering matters: import BEFORE patching, so the pods the
  sync rolls find their images present. The database operand override is
  preserved and `ensure_db_image` is skipped under this flag -- the database is
  not a node (memql#4063), and a dev loop must not swap the operand.

Parameters: `node` (default all app nodes; the preflight lets the person
narrow it), never `pull-infra`. Same runner, same `MemQL Install` channel,
same JSON envelope, same exit codes. The instance-actions table
(`src/deploy/instanceActions.ts`) gains one row:

| Action | local, nothing installed | local, installed | remote |
|---|---|---|---|
| Rebuild from checkout | -- | `k3d.dev` over the recorded checkout | -- |

### 5.3 Before it runs

The existing checklist idiom:

- Docker daemon reachable.
- The checkout exists at the recorded path and is a memql checkout.
- Its git ref (tag / branch / detached HEAD) and the count of uncommitted
  files, with *"`deploy/` has edits -- manifests do not ride a rebuild"* when
  that directory is dirty.
- Which nodes will rebuild.
- The lane statement from section 5.1, when crossing.
- *"A first build takes minutes."* The number is measured when it is
  implemented, not promised here.

### 5.4 After it runs

A receipt entry for the `rebuild` step: commit, uncommitted count, nodes,
`imageSource: checkout`. `recordedImageSource(receipt)` answers the mode the
way `recordedImageTag` answers the tag: from the LATEST entry that set it,
across both lanes. The catalog refreshes, so `edited` marks clear where the
hashes now match. Toast: *"Rebuilt bff, cognition, agent -- local now runs
your checkout (abc1234, 4 uncommitted files)."* Failure -> classified verdict
plus the channel, as every run.

### 5.5 Where else the action appears

The `edited` lens on a local cluster (section 3.4) and the construct detail
page for an edited seeded construct on local both invoke the same command.

### 5.6 The checkout is part of the instance

- Install done-screen: **Open source checkout**.
- Deployments instance row: **Open checkout**, permanent.
- Connection page: checkout path, git ref, image-source mode.
- The handoff (section 4.4) reads the same recorded path.

That is what "connected" means in practice: the instance row knows its
checkout, and every surface that can get a person there does.

### 5.7 Not in Rebuild

Manifests (ArgoCD syncs `deploy/` from the repository at the pinned revision);
carrier and bundle images for product repositories (`k3d.dev` has the
`carrier-*` parameters; the extension passes none -- an engine-only install
has no bundle); branch automation (a detached HEAD is stated, not fixed);
remote clusters (that is the deploy pipeline).

---

## 6. Decisions

| # | Decision | Why | Rejected |
|---|---|---|---|
| D1 | Local = editable for every origin, with a Rebuild action | "Editable" is only honest with a path to apply the edit; `k3d.dev` already exists | editable with rebuild left to the terminal; keeping core sealed on local |
| D2 | The handoff lives on the concept page only, for now | The portal shows constructs only as concepts; the catalog is the extension's by the boundary rule | a portal `/constructs` inventory; per-module construct lists (the registry does not report them) |
| D3 | Deep link, not a graph-mediated channel | Standard, small, no cross-node work; the browser prompt is a known gesture | `v1:authoring:openRequest` rows + presence (new concept, routing rules across replicas, TTLs, two-window ambiguity) -- deferred |
| D4 | No source rendering in the portal, even read-only | The brief: no editor-like UI in the portal; one authoring surface | a read-only viewer as the no-extension fallback |
| D5 | Cluster documents get highlighting, not LSP | The server is offline and file-based; virtual imports would produce false diagnostics | serving imported files to the server lazily |
| D6 | `edited` takes the drifted mark on every cluster | The gutter answers one question and the answer is "no"; the lens carries locality | drifted mark on local only |
| D7 | Rebuild builds from the RECORDED checkout only | That is the repo the install cloned; "connected" means one path | rebuilding from whatever folder is open |
| D8 | Crossing image-source lanes is stated in the preflight, never done silently | A repair must never upgrade and must never un-rebuild unannounced | preserving checkout mode across repair (it would make "repair" mean two things) |
| D9 | The operand override survives `image-source=checkout` | The database is not a node (memql#4063); a dev loop must not roll the database | dropping every override |
| D10 | No branch automation for the detached checkout | Stating a fact is cheaper than owning a git workflow | creating `local-dev` off the tag |

---

## 7. Test plan

Nothing in this design crosses a node boundary: no new events, no forwarded
state, no session-local fact read on another node. `ListConstructs` and
`ReadPackFile` answer identically from any replica (same image, shared DB).
The cluster-e2e harness gains nothing here, and that is stated rather than
left implied.

- **Extension** (`node --test`, no `vscode` import): the read-only matrix
  (origin x locality x checkout-match); handoff parse / validate /
  cluster-match / landing decision; the instance-actions row; mode display;
  preflight wording under the `report.ts` doctrine; pending-handoff TTL and
  single consumption.
- **LSP**: `edited` cases in `training_test.go` (seeded + mismatch -> edited;
  seeded + match -> seeded; staged / promoted / untrained / unknown unchanged)
  plus `training_acceptance_test.go`.
- **sdk/ts**: pack-browser request / reply envelopes and their routing-ledger
  classification.
- **Portal** (vitest): `OpenInVsCode` link composition -- runtime-config
  domain vs derived; scheme choice and persistence; canonical-id encoding;
  `portal_view_composition_test.go` untouched.
- **Scripts**: `scripts/lib/capability_contract_test.go` picks up `dev.sh`'s
  new parameters through `--print-spec`; a unit test over the override filter
  (node overrides removed, operand kept).
- **Host tests** (xvfb, env-gated like the rest): URI round trip against a
  fake cluster; the checkout-opening flow.
- **Engine**: the additive `domain` field in `runtimeconfig_test.go`.
- **Not verified live on the shared machine**: an actual local-cluster install
  or rebuild (forbidden there; same caveat as memql#4200). The manual
  checklist in `vscode-runtime-panel-verification.md` gains the rebuild and
  handoff cases.

---

## 8. Documentation

- Extension README: the boundary table row; "Where a construct came from"
  gains the local column and `edited`; a new "Open from the portal" section;
  Deployments gains Rebuild and the mode; "Read-only files" rewritten to the
  table in section 3.2.
- `docs/public/language/training.md`: the seventh state.
- `clients/README.md`: the handoff, one paragraph.
- `docs/public/operate/reproduce-the-cloud-locally.md`: Rebuild is `make dev`.
- The 2026-08-14 designs are historical and stay as written; this document
  supersedes their read-only table where the two differ.

---

## 9. Out of scope

Deferred, and NOT filed as issues -- none has an implementable shape yet:
presence detection (graph-mediated); a portal constructs inventory; module ->
constructs; carrier/bundle rebuilds from the extension; Cursor and other
forks; a Rebuild for remote clusters.

---

## 10. Relationship to sub-project 2

Artifacts -- a `labels` field on `v1:library:artifact`, the mutations and
agent tools around it, and the portal Library page -- shares nothing with this
design except the portal's shell. It is brainstormed, specified and tracked
separately.

---

Refs: memql#4242 (epic), memql#3745 / #3747 / #3752 / #3759 / #3762 (constructs
view, training, read-only), memql#3901 / #4068 (receipt), memql#3572 / #4063
(image overrides), memql#3928 (staged), memql#4194 (information policy),
memql#3309 (write confirmation), memql#3767 (front door hosts).
