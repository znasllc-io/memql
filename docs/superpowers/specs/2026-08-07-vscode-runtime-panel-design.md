---
title: VS Code Runtime Panel -- Execute Constructs, Browse Concepts, Drive Clusters
audience: internal
status: draft
area: design
date: 2026-08-07
owner: znas
---

# VS Code Runtime Panel

Turn the memQL VS Code extension from a language-only client into a
runtime helper: connect to a cluster, run any executable construct
straight from its signature with arguments, browse every concept's
schema and rows, and drive deployments -- the cockpit's capabilities,
re-hosted in the editor, plus the one thing the cockpit does not have
(execute a construct and see its result).

This is the first of five specs decomposed from a larger brainstorm.
The others are named in [Relationship to the portal](#relationship-to-the-portal)
and are explicitly out of scope here.

---

## Problem

Today `editors/vscode` is a thin LSP client. It ships `memql-lsp` as a
per-platform binary and contributes syntax highlighting, diagnostics,
completion, hover and signature help for `.memql` files. It has no UI
of its own, no activity-bar presence, and no connection to a running
engine.

That leaves two gaps:

1. **There is no way to execute what you just wrote.** Testing a query,
   mutation or logic construct means saving, redeploying, and driving
   it from somewhere else. Automations are worse: there is no invoke
   path at all, at any surface, so the only way to exercise one is to
   cause its trigger event for real.
2. **There is no way to see the data.** A developer editing a concept
   or a mutation cannot look at the rows it produces without leaving
   the editor.

The memQL Cockpit solves the second problem well (its Concepts tab is a
generic row browser driven by `@displayCard` hints) and solves cluster
management and deployments well (its DevOps tab). It does not solve the
first. The goal here is to bring all three into the editor and add
execution.

---

## Scope

### In scope

All work lands in the `memql` repository. No new repository is needed.

**Engine.** Two additions, both of which the portal also requires:

- **Automation run** -- a message on `MemqlService.Stream` that
  synthesizes a trigger event for a named automation, dispatches it,
  and streams back a step trace.
- **Deploy-control on the stream** -- bridge the `DeployControlService`
  RPCs onto `MemqlService.Stream` so any WebSocket client can reach
  them, gated identically to the unary service.

**TypeScript SDK.** New surfaces on the zero-dependency core
(`sdk/ts`): authoring (validate + session-define), deploy-control,
concept browsing with keyset paging, and automation run.

**`view-kit`.** A new framework-agnostic rendering package at
`sdk/ts-viewkit/`, published as `@znasllc-io/memql-view-kit`: rows plus
`@displayCard` hints in, DOM out. Knows nothing about VS Code or the
portal. Themed through CSS custom properties. It is a sibling of
`sdk/ts` rather than a module inside it, because `sdk/ts` is a
client-agnostic runtime core with no DOM dependency and must stay that
way.

**VS Code extension.** An activity-bar container with Clusters,
Concepts and Runs tree views; CodeLens run affordances on construct
signatures; arg forms, results views, cluster topology and deployment
surfaces as editor-area webview tabs.

**Assets.** A 24x24 single-fill SVG activity-bar icon.

### Out of scope

- The portal repository, its concepts page, and its view system.
- Retiring the server-rendered admin portal at `/admin/*`.
- OIDC credential sharing with the cockpit's keyring credstore. v1
  authenticates with the `pat:` field `clusters.yaml` already supports;
  an OIDC-only cluster reports that it must be authenticated in the
  cockpit first.
- Running `spec`, `trait`, `prompt` or `seed` constructs. Each needs an
  execution semantic decided (which row does a spec evaluate against;
  who pays for a prompt's provider call) that is a design of its own.
- The cockpit's architecture navigator and topology builder. See
  [Deliberate omissions](#deliberate-omissions).

---

## What already exists

Establishing this precisely matters, because it determines how much of
the work is engine-side.

Already on `MemqlService.Stream`, reachable by any WebSocket client:

| Surface | Messages |
|---|---|
| Named invocation | `ExecuteQueryMsg` / `QueryResultChunk` / `QueryErrorMsg` |
| Tool invocation | `ListToolsMsg`, `CallToolMsg` / `CallToolResult` |
| Authoring | `AuthoringValidateBundleMsg`, `AuthoringSessionDefineBundleMsg`, `DurablePromoteBundleMsg`, `DurableDemoteBundleMsg` |
| Concepts | `ConceptsListMsg` -> `ConceptInfo` (carrying `display_card`), `ConceptsSubscribeMsg` |
| DSL source | `ListPackDomainsMsg`, `ListPackFilesMsg`, `ReadPackFileMsg` |
| Grammar | `DslSpecMsg` |
| Access | `MyAccessMsg` |

`AuthoringSessionDefineBundleMsg` is the substrate for running an
editor buffer: it validates a `.memql` bundle, then registers it into
the caller's owner-scoped authored registry, **stream-scoped and
non-durable**. Defined constructs become callable by name within the
session, never shadow core, and are dropped when the stream ends.

Cluster topology and deployment history are ordinary concept rows
(`v1:cluster:node`, `v1:cluster:deployment`,
`v1:cluster:deploymentNodeSpec`, `v1:observability:codeMetric`) and are
therefore already readable over the stream.

Cluster profiles live in `~/.memql/clusters.yaml` (`name`,
`display_name`, `domain`, `endpoint`, `issuer`, `client_id`, optional
`pat`, plus a persisted `selected_cluster`). Credentials otherwise live
in a keyring-backed credstore with a file fallback.

**Not reachable over the WebSocket bridge:** `DeployControlService` is
a separate unary gRPC service mounted on the same listener. The Go SDK
dials it natively. A browser cannot, and neither can a WebSocket
client. This is why the bridge is engine work rather than SDK work.

**Does not exist anywhere:** an invoke path for automations.

---

## Approach

### TypeScript-first, with a Go escape hatch

The extension is an ordinary TypeScript VS Code extension consuming an
extended `sdk/ts`. The SDK has zero dependencies and speaks the
`/memql/ws` bridge via the global `WebSocket`, which exists in VS
Code's Node runtime -- so no native modules and no sidecar process.

The alternative was a Go sidecar: ship a second binary alongside
`memql-lsp` exposing `sdk/go/client`, `sdk/go/authoring`, `sdk/go/pack`
and the keyring credstore over local IPC, with TypeScript as thin UI.
That reuses substantially more existing code and would serve other
editors later. It was rejected because it buys the portal nothing: the
portal is a browser SPA that cannot run a sidecar, so a Go-only client
path means building the entire client surface twice. Every line of SDK
and `view-kit` work done here is work the portal needs regardless.

The accepted cost is that `sdk/ts` and `sdk/go` will carry parallel
implementations of the same wire surfaces and can drift. The mitigation
is that the wire contract, not either SDK, is the specification.

**The escape hatch:** the one problem genuinely shaped for Go is the
keyring credstore, since VS Code removed keytar and there is no clean
TypeScript path to the cockpit's stored OIDC tokens. If PAT-only
authentication proves insufficient, a minimal Go helper covering the
credstore alone is added -- and only that.

### Two owned boundaries

**The LSP owns all parsing.** The extension never parses `.memql`. It
asks the language server which constructs are runnable, what arguments
each takes, and where its signature is. One parser, so the arg form
cannot disagree with the compiler.

**`view-kit` owns all rendering.** It takes rows plus display-card
hints and emits DOM, knowing nothing about its host. This is what lets
the portal inherit the renderer rather than rebuild it.

---

## Architecture

```
VS Code extension (TypeScript)
|-- clusters/    ~/.memql/clusters.yaml read-modify-write + file watch
|                credential resolution (PAT), cluster classification
|-- connection/  one live SDK connection per selected cluster
|-- constructs/  LSP custom requests -> runnable constructs + arg
|                schemas + signature ranges; drives CodeLens
|-- run/         run-config store; orchestrates session-define -> invoke
|-- views/       tree providers: Clusters, Concepts, Runs
|-- webview/     arg form + results + cluster + concept tabs
                        |
                   view-kit  (framework-agnostic DOM + CSS custom
                   properties; shared with the portal)
                        |
              @znasllc-io/memql-sdk-core   (zero deps, /memql/ws)
              + authoring / deploy / concepts / automation-run
                        |
                   memQL engine
```

### Surface layout

The sidebar navigates; the editor area holds content. A sidebar tree is
too narrow for a topology grid or a row table.

```
Activity bar: memQL
|-- Clusters      (tree)     list - status - add / edit - select
|-- Concepts      (tree)     domain -> concept
|-- Runs          (tree)     saved run configurations

Editor-area tabs (webview, view-kit rendered)
|-- Cluster: <name>          topology grid + deployment history + actions
|-- Concept: <id>            row list with search + keyset paging -> detail
|-- Run: <construct>         arg form
|-- Result: <construct>      rows / step trace / raw JSON
```

---

## The run loop

```
.memql buffer
   |  LSP custom request: memql/runnableConstructs
   |  -> [{kind, name, signatureRange,
   |       args[{name, type, required, enum, description}]}]
   v
CodeLens on each runnable signature:  Run | Run with...
   |
   v
Arg form (webview)  --save-->  named run config in the workspace
   |
   v
Preflight
   |-- cluster selected + connected?
   +-- write-kind (mutation / automation) on a non-local cluster -> confirm
   |
   v
Bundle assembly
   the active file, plus any transitively use-imported workspace file
   that currently has unsaved edits. Everything else resolves against
   the live registry -- session-defined constructs never shadow core.
   |
   v
AuthoringValidateBundle   (Gate-1 sandbox, no engine mutation)
   diagnostics carry bundle-file line/column; the extension maps them
   back to buffer coordinates and publishes them to the Problems panel
   |  ok
   v
AuthoringSessionDefineBundle   (stream-scoped, owner-scoped, non-durable)
   |
   v
Invoke by kind
   query / mutation / logic --> ExecuteQueryMsg (named call + args)
   tool                     --> CallToolMsg against the DEPLOYED tool
   automation               --> RunAutomation (new) with a synthetic event
   |
   v
Results view: rows through view-kit + @displayCard, raw-JSON toggle,
step trace for automations, click a row to open it in Concepts.
```

### Runnable kinds

| Kind | Invocation | Runs your buffer? |
|---|---|---|
| `query` | `ExecuteQueryMsg` (named call) | yes, via session-define |
| `mutation` | `ExecuteQueryMsg` (named call) | yes, via session-define |
| `logic` | `ExecuteQueryMsg` (named call) | yes, via session-define |
| `tool` | `CallToolMsg` | **no** -- deployed definition |
| `automation` | `RunAutomation` (new) | **no** -- deployed definition |

Session-define covers the plain construct family (query, mutation,
logic, spec, trait). A `tool` is a declaration bound to a Go-backed
handler and an `automation` is event-triggered; neither is
session-definable. Their result views carry an explicit banner stating
that the deployed definition ran, not the buffer. Same button,
different semantics -- the UI must say so rather than let the developer
assume otherwise.

### Argument entry

The arg form is generated from the construct's own `args` block, which
the LSP supplies from the buffer: typed fields, required markers, enum
dropdowns from `@enum(...)`, descriptions from `@description(...)`.
Filling it and naming it persists a reusable run configuration in the
workspace, so re-running is one action and an agent can author or edit
a configuration as text.

**Automations need a different form.** An automation binds its
triggering event as `args` and reads `args.payload.<field>` freely;
there is no declared payload schema to generate fields from. The
automation form therefore offers two ways to construct the event:

- **Pick an existing row** of the concept named by
  `@trigger(concept=...)`, browsed with the same Concepts picker. This
  is the option that makes automations genuinely testable, and it
  reuses a browser that already exists.
- **Paste JSON** for a payload that does not correspond to a stored
  row.

An automation triggered by `@trigger(schedule=...)` has no concept and
is fire-now with an empty event.

### Session lifetime

Session-defined constructs are dropped when the stream ends. If the
WebSocket drops and reconnects, every injected construct is gone, and a
subsequent run would silently execute the *deployed* version instead of
the buffer -- an invisible failure.

The extension therefore retains the last-injected bundle per cluster
and re-injects it on reconnect before honoring a re-run. This behavior
is covered by an explicit test.

---

## The DevOps surface

The cluster tab carries parity with the cockpit's right pane.

**Topology**, from `v1:cluster:node` rows: per-node health, running
version, short deployment id, `[orphan]` tagging for stopped nodes or
nodes carrying a non-current deployment id, and a tally that flags any
node type below its expected replica count.

**Deployment history**, from `v1:cluster:deployment` newest-first with
status token, version, env and provider; the current deployment marked;
selecting one previews its composition from the
`v1:cluster:deploymentNodeSpec` set, with orphans flagged.

**Actions**, through the bridged deploy-control: cut version (previewing
`SuggestNextVersion`), deploy, roll back, promote staging to prod, and
rollout promote / abort.

The role matrix is the cockpit's, unchanged:

| Action | Required role |
|---|---|
| View | any |
| Cut version, deploy | developer, admin, owner |
| Roll back | **owner only** |

Enforcement is server-side on both the streamed and unary paths. The UI
hides or disables what the caller cannot do, but the gate is the
engine's. Promote-to-prod, rollback and rollout abort require
type-to-confirm. Every action writes a `v1:identity:auditEvent` and the
UI surfaces the returned audit id on success.

### Deliberate omissions

Two cockpit features are excluded. Each is a feature in its own right
rather than parity glue, and each would roughly double this surface:

- **The architecture navigator** (the cockpit's `X` toggle) -- a
  drill-down over the embedded architecture model with a
  `v1:observability:codeMetric` overlay. A distinct observability
  product, and a better fit for the portal than an editor sidebar.
- **The topology builder** (`N` / `V`) -- interactive composition of
  node types by replica count, applied as `CutVersion` plus per-tier
  `deploymentNodeSpec` writes, including a clickable pixel canvas.
  Substantial, and it is authoring infrastructure rather than observing
  or driving a deploy.

---

## The Concepts surface

The Concepts tree lists domains and their concepts from
`ConceptsListMsg`. Opening one gives a row list with in-list search and
keyset paging via the SDK's opaque cursor, which resolves against any
replica. Selecting a row renders the detail pane.

**Detail rendering carries no concept-specific code, ever.** The header
uses whatever `@displayCard` slots the concept declares (primary,
secondary, tertiary, status); the body is a recursive walk of payload,
provenance and intrinsics. A concept declaring no display card degrades
to id plus intrinsics. This is what makes a newly declared concept work
the day it is declared, and it is the contract the portal's view system
extends rather than replaces.

Rows stay live via a CDC subscription for the open concept, so a
mutation run from the editor appears without a manual refresh.

A row in a Result tab links into this surface, opening its concept
detail.

---

## Clusters and credentials

The extension shares `~/.memql/clusters.yaml` with the cockpit. A
cluster added in either appears in both.

- Reads are watched; the tree refreshes when the file changes.
- Writes are read-modify-write. If the file changed since it was last
  read, it is re-read and merged rather than clobbered -- the cockpit
  may have written it.
- `selected_cluster` is honored and updated, so both surfaces resume on
  the same working cluster.
- Authentication uses the `pat:` field. A cluster whose `NeedsAuth`
  condition holds because it is OIDC-only reports that it must be
  authenticated in the cockpit first.

**One new field** is added to the `ClusterConfig` schema in
`clusters.yaml`: `local` (boolean, default false). It gates the write
confirmation below. Because the file is shared, the cockpit's
`ClusterConfig` struct gains the same field so a round-trip through
either tool preserves it.

---

## Safety and trust

### Workspace trust

The extension currently declares `untrustedWorkspaces: supported`,
justified by "the server only parses `.memql` files (no code
execution)." That justification stops holding: the extension will read
credentials, connect to live clusters, execute constructs, and trigger
deploys.

- Language features -- highlighting, diagnostics, completion, hover,
  signature help -- remain available in untrusted workspaces,
  unchanged.
- Connecting, running, and every deploy action require a **trusted**
  workspace. The manifest moves to `supported: "limited"`.
- Run configurations live in the workspace, so a repository can ship
  one. **Nothing ever auto-runs.** CodeLens renders an affordance; it
  never executes on open, and there is no run-on-save.
- The PAT is never written to a workspace file, a log, or a webview.

The existing machine-scope restriction on `memql.lsp.serverPath` stands
-- a workspace must not be able to redirect the extension to an
arbitrary executable.

### Write confirmation

Reads run freely. A mutation or automation run against a cluster not
classified local prompts once, naming the cluster and the construct.
This is the same instinct behind the cockpit's type-to-confirm on
rollback, sized to a smaller risk. The engine's per-row authorization
remains the actual authority; this is friction against the wrong
window, not a second permission system.

---

## Error handling

| Failure | Behavior |
|---|---|
| Bundle fails Gate-1 validation | Per-construct diagnostics in the Problems panel, mapped from bundle-file coordinates back to buffer coordinates. A zero position means "no position" and becomes a file-level diagnostic -- never line 0. |
| Runtime error from the engine | Result tab shows the error state with the engine's `ERR-` id, copyable. |
| Stream drops | Views show disconnected / reconnecting. On reconnect the last bundle is re-injected before any re-run is honored. |
| Insufficient role | The action names the role it requires. Never a silent no-op. |
| Cluster not configured | The `NeedsAuth` case gets an actionable message, including the OIDC-only case pointing at the cockpit. |
| `clusters.yaml` changed underneath us | Re-read and merge rather than clobber. |
| Deploy action fails | `ERROR: <message>` surfaced verbatim, with the audit id when one was written. |

---

## Testing

The engine work carries the risk.

**Automation run must be tested across nodes.** Per the multi-node
default, a green single-node test is a false signal for event-driven
behavior: cross-node event delivery requires an explicit routing rule
or it dies silently in cluster mode. Coverage goes in the cluster-e2e
harness, with the run requested on one node and the automation's
subscriber compiled into another. The test must fail against
single-node-assuming code and pass with the routing rule in place.

**The deploy-control bridge gets a parity test.** The streamed path and
the unary path must enforce the identical role gate -- a non-admin
receives `PermissionDenied` from both. Without this the bridge is a
privilege-escalation hole.

**SDK tests** run against a fake WebSocket server for authoring,
deploy-control, concept browsing and automation run.

**Extension tests** target the failures that would otherwise be
invisible:

- bundle assembly, including inclusion of dirty transitive dependencies
- diagnostic coordinate mapping, including the zero-means-no-position case
- session re-injection after reconnect
- `clusters.yaml` round-trip under concurrent modification

**`view-kit` tests** cover display cards present, absent and partial.

**One integration test** ties it together: run a mutation from a buffer
against the local cluster and assert the resulting row appears in the
Concepts view.

---

## Increments

Each ships independently. B1 and B2 require no engine changes at all.

| | Increment | Engine work |
|---|---|---|
| **B1** | Connection, Clusters tree, Concepts surface, `view-kit`, activity-bar icon | none |
| **B2** | Run loop for query / mutation / logic / tool; arg forms; run configs; results | none |
| **B3** | Automation run and step trace | new message, cross-node routing rule |
| **B4** | DevOps topology, deployment history, deploy actions | deploy-control bridge |

**B4 is sequenced last, after the portal spec has confirmed the bridge
contract.** The bridge's principal beneficiary is the portal, which
cannot reach a unary gRPC service at all; designing it against the
editor alone risks reshaping it when the portal arrives. The cockpit
continues to serve deployments in the meantime.

`view-kit` is built in B1 rather than retrofitted. If the results
renderer lands first as VS Code-specific DOM it will not come back out
cleanly, and the portal loses the reuse that justified choosing
TypeScript in the first place.

---

## Assets

VS Code has two icon slots with different requirements.

- **Extension tile** (`"icon"` in `package.json`) -- full color,
  already pointing at the 256x256 green `icons/memql.png`. Unchanged.
- **Activity-bar container** -- VS Code masks this asset and repaints
  it with the theme foreground color, so the supplied color is
  discarded. The constraint is legibility at 24x24 under a mask. The
  current PNG is a dense twelve-node graph with thin edges and degrades
  badly at that size.

A new 24x24 single-fill SVG is derived from the simplified four-node
glyph already present in `assets/logo.svg` and `component/mcp/icon.svg`.
Brand-continuous, mask-safe, legible.

---

## Relationship to the portal

This spec is one of five decomposed from the originating brainstorm:

| | Sub-project | Status |
|---|---|---|
| **A** | Engine execution and introspection contract | **this spec** (folded into B) |
| **B** | VS Code runtime panel | **this spec** |
| **C** | Portal SPA in-repo: scaffold and concepts browser | separate spec |
| **D** | Portal view system: UI-element library, predefined views for platform concepts, AI-composed custom views | separate spec |
| **E** | Retire the server-rendered `/admin/*` portal into the SPA | separate spec, depends on C |

### Amendment, 2026-08-07: the portal lives in this repository

The original decomposition assumed the portal would get its own repository. That
is **retracted**. The portal is built in the memql repo, at `clients/portal/`,
on React + TypeScript + Tailwind + Vite.

**Why this does not violate product neutrality.** The engine is
product-agnostic and `TestEngineIsProductNeutral` enforces it, but what that
test bans is a downstream *product's* name appearing in tracked files. The
portal is not a product — it is the platform's own operations console, the
graphical sibling of `memql-cockpit`, and CLAUDE.md already sanctions that slot
as the `engine-bff` "Cockpit / ops edge, no bundle" component. The rule it must
keep is narrow and clear: **the portal must never name a downstream product.**

**The `clients/` convention.** memQL is a platform other people self-host to
serve their own clients — landing pages, SPAs, mobile apps, games. So
client-facing app surfaces are plural and first-class, a sibling category to
`integrations/`:

```
clients/
  portal/        the platform's own operations console (this repo's only inhabitant)
```

A product repo built from the `memql-project` template mirrors the same
convention, holding as many surfaces as its clients need
(`clients/acme-landing/`, `clients/acme-spa/`, ...). The engine repo carries
exactly one, which makes the portal the worked example the template copies.
The template currently uses a singular `client/` at the product root and needs
updating to match.

**Consequences for this spec's deliverables.** `view-kit` no longer needs
cross-repo package publishing — the portal consumes it in-repo — but it stays a
separate package with no DOM dependency, because that boundary is what keeps the
renderer usable from both a VS Code webview and a React tree. Nothing in B1
changes.

Three things built here are consumed directly by C and D:

- **`view-kit`**, whose display-card-driven renderer is the portal's
  first UI element and the base its view library extends.
- **The extended `sdk/ts`**, which the portal uses unchanged.
- **The deploy-control bridge**, without which the portal has no
  DevOps surface at all.

One known divergence: a browser cannot read `~/.memql/clusters.yaml`,
so the portal's cluster registry must resolve differently. That is a C
problem and is not solved here.

### Open questions for spec C

Recorded so they are not lost; this spec does not answer them.

- **Cluster registry in a browser.** `~/.memql/clusters.yaml` is unreachable.
  Options: a server-side registry concept, browser-local storage, or deriving
  the cluster from the origin the portal is served from. The last is likely
  simplest — the portal is served *by* the cluster it manages.
- **Where the portal is served from.** The `engine-bff` component is the natural
  host, which makes the origin-derived registry above nearly free.
- **Auth.** The portal cannot read a PAT from a config file. It goes through the
  identity service's magic-link / OAuth flow like any other browser client.
- **Updating the `memql-project` template** to the plural `clients/` convention,
  plus whatever else has drifted since it was last touched.
