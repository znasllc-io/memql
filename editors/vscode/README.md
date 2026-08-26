# MemQL for VS Code

Language support for MemQL (`.memql`) files, powered by the offline
`memql-lsp` language server (which embeds the same MemQL Sense brain the
Cockpit uses). Works fully offline against local files -- no cluster, no auth.

## Features

- Syntax highlighting (TextMate grammar, generated from the DSL spec) refined
  by semantic tokens from the server.
- Live diagnostics (errors and warnings) as you type, including cross-reference
  resolution: unknown import modules/ids and signature concepts that resolve to
  nothing.
- Context-aware completion (constructs, concepts, functions, annotations, ...),
  including segment-aware `use`-line completion (namespaces -> kinds -> ids) and
  kind-filtered in-body invocation completion.
- Hover documentation and signature help.
- Per-construct **training state** against the connected cluster -- a gutter
  icon, a CodeLens and a status-bar item saying which constructs in this file
  the cluster does not know, or knows in an older version, plus the actions
  that change that. See [Training](#training-what-the-cluster-knows-about-the-file-you-are-editing).

## The five views, and which question each answers

An activity-bar panel connects the extension to a running cluster. It requires
a trusted workspace, since it reads credentials and opens a network connection.

| View | Answers |
|---|---|
| **Clusters** | Which clusters can I reach, as whom, in what state -- the home view |
| **Deployments** | What changed the SELECTED cluster, when, and how it ended |
| **Constructs** | What is DEFINED on this cluster |
| **Data** | What rows EXIST |
| **Runs** | What have I run against a cluster |

**Clusters is the home view** (memql#4195): the extension's mission is managing
clusters -- many remote ones, and installing / repairing / uninstalling the
local one -- so the list of them leads. Each row leads with its state (connected
/ needs sign-in / unreachable) and recorded release; a cluster recorded behind
the release this extension ships for says so in its tooltip. With zero clusters
the view offers the two ways to get one -- install a local cluster, or add an
existing one -- and install / repair are also in the view's title menu. The
install, repair and uninstall flows open with a "Before it runs" checklist
(graph loaded, whether your password will be needed, where the provider key
path comes from) before anything starts, show honest per-step progress during,
and put the full run log in the `MemQL Install` output channel.

Constructs and Data are the two halves of one question, and keeping them apart
is what the naming is for: a query is a definition, the rows it returns are
data, and a *concept* is itself a construct. The view that browses rows was
called Concepts until memql#3754 -- which was the wrong name from the start,
since it never showed a concept's definition. It shows rows, so it is Data, and
its commands moved with it: `memql.concepts.*` is now `memql.data.*`.

### The boundary

One rule decides where every surface in this extension goes, including ones
nobody has proposed yet:

> **The extension owns what is on your machine and what you can reach.
> The portal owns what is inside a cluster.**

| Question | Surface |
|---|---|
| What instances do I operate, at what version? | extension -- Deployments |
| Install / upgrade / repair / uninstall / roll out | extension -- Deployments |
| Which clusters can I reach, as whom? | extension -- Clusters |
| Which pods run, which are orphaned, which tier is under-replicated | **portal** |
| Integrations, identity, sites, accounts | **portal** |
| What does this construct do, what rows exist | extension -- Constructs / Data / Runs |
| Read or edit a construct's SOURCE | extension -- from your workspace or from the cluster |

**The row above it is adjacent, not the same claim.** *What does this construct
do* is a description and *what rows exist* is data; the portal could render
either, and for rows it already does. Code is neither. So the portal's concept
page carries **Open definition in VS Code** and no source pane -- one door, and
it opens in the editor -- while the extension serves the bytes from wherever
they are: your workspace when the file is there, the cluster's own pack browser
when it is not. Even a construct whose file is nowhere on your machine is read
here rather than there.

**Topology used to be here and is not any more.** A pod grid, orphan verdicts
and under-replica alarms are cluster state, the portal already draws them, and
two surfaces answering one question diverge on the day the second one ships.
That is the rule, not a preference about which UI is nicer -- so a pod grid
proposed for this extension has its answer before anyone writes it. Every
cluster's portal is one click away: **Open Portal**, on the Clusters row and on
the connection page.

Full rationale: [the Deployments surface design](https://github.com/znasllc-io/memql/blob/main/docs/superpowers/specs/2026-08-14-vscode-deployments-surface-design.md).

## Deployments

The **selected** cluster's deployment runs, newest first. One cluster, flat.

```
DEPLOYMENTS   local · healthy · v0.19.1
|- upgrade   v0.16.1 -> v0.17.0   succeeded   2d ago
\- install                        succeeded   9d ago
```

**Which cluster** is the one this editor has in hand, and it is named in the
view's description -- the line beside the view's own name, carrying its health
and its version, plus `· update vX.Y.Z available` when there is one. Switching
the selection in **Clusters** switches this view with it. With nothing selected
the view is empty and says so, with the two ways out of that state:

```
Not connected. Select a cluster to see its deployments.
Select Cluster
Install a local cluster
```

There is no `local` wrapper row (memql#4426). It used to be there so that a
machine with nothing installed had somewhere to start; that entry point now
lives in three places instead -- the welcome above, the Clusters welcome, and
**Create Deployment** in this view's title menu -- and the instance's own
actions (Repair, Rebuild From Checkout, Uninstall, Open Local Checkout) moved to
the title menu with it. Nothing lost a route.

**An instance** is a MemQL you operate. It is derived rather than declared:
`local` is whatever is on this machine (an install receipt, a `local: true`
registry row, and whether the front door answers), and every other
`clusters.yaml` entry is a remote one.

**Selecting a run opens it.** The detail page states what the run was, what it
moved the cluster between, how it ended and why, when it started and finished
and how long it took, and its steps (or, for a remote deployment, its node
types). Its buttons are the instance's own role-gated actions, ordered for what
you are looking at: a failed run leads with **Repair**, a cluster running
checkout images leads with **Rebuild From Checkout**, and a remote deployment
offers the deploy-control actions your role admits. No verb appears there that
the instance page would not also offer.

**A run** is something that changed an instance's deployed state. Local runs are
recorded in `~/.memql/runs/`, one file per run, rewritten after every step, so a
run killed half-way leaves a record naming exactly the steps that completed.
Remote runs are not recorded locally at all: they are `v1:cluster:deployment`
rows and the cluster is their record.

The two are not the same granularity, and the page says so: a local run's items
are install steps, and a remote run's are **node types** -- the per-tier version,
replicas and image digest a deployment declared.

### What an instance offers

| Action | local, nothing installed | local, installed | remote |
|---|---|---|---|
| Create deployment | the full install graph | move to another release tag | deploy |
| Repair | -- | re-run the graph | -- |
| Rebuild from checkout | -- | k3d.dev over the recorded checkout | -- |
| Uninstall | -- | the uninstall graph, behind its preview | -- |
| Cut version / Promote / Rollout / Roll back | -- | -- | by role |

The local-cluster wizard targets **linux/amd64** only. On any other platform
Create deployment, Repair, Upgrade and Uninstall refuse at the platform gate --
they do not list a tag that cannot run, and they do not touch hosts, mkcert or
the receipt. macOS inner-loop remains `make up`.

**Re-running the install graph is the repair, and it is also the upgrade.**
Every step verifies before it acts and skips whatever is already satisfied, so
one graph serves all three: an install does everything, a repair does whatever
is missing, and a deployment to another tag moves the checkout and reconciles.
Before it starts, the page shows which steps will actually change something --
usually two of fifteen -- because a run reporting fifteen steps looks like a
reinstall to whoever is watching it.

**Rebuild from checkout is a fourth, separate one-step graph.** A wizard install
runs released images pulled at a tag; the checkout it cloned is inert until
something builds from it. Rebuild does: it builds the node images from that
checkout, imports them, points the cluster's Application at them (keeping the
database operand where it is), and restarts. From then on the instance row reads
`checkout <commit> (<n> uncommitted)` instead of a version, and the Connection
page says so. An install, upgrade or repair returns the cluster to released
images -- and says so before it runs.

**Choosing a version never happens by itself.** The tag list comes from the
checkout's origin, newest first, and nothing is pre-selected: a version somebody
picked off a list is a fact they can be held to, and one the extension picked
silently is not. With no network there is a text box, and the reason the list is
missing is printed beside it.

### Remote instances, and the three states a deploy pipeline can be in

The page probes the cluster's deployment status when it opens and renders
exactly one of:

- **the actions**, when the pipeline answered;
- **no deploy pipeline is configured**, in the engine's own words. This is the
  ordinary state of an engine-only cluster -- the orchestration lives in a
  product repository, and local clusters are operated with `make up` rather than
  the deploy console;
- **status is not visible at your role**, because that read is owner/admin
  gated. Deployment history is ordinary rows and is unaffected.

None of the three is an error. A row of buttons that turned out to be refused
would be the error; naming the state is not. What the extension hides is hidden
as a **courtesy** -- the engine decides, and a refusal names the role required.

## Onboarding: getting a cluster into this editor

Two routes, and they are for opposite situations. The **+** on the Clusters view
offers both.

### Install a local cluster

Builds one on this machine: a k3d cluster, an ArgoCD that reconciles it, the
hosts entries and the mkcert certificate that make `memql.localhost` resolve and
serve TLS, and an owner account bootstrapped from the answers on the form.

**The version field recommends Latest, and Latest is preselected.** The list is
read from the repository at page-open time, newest first, and its first entry
reads `Latest -- vX.Y.Z (recommended)`. Take it unless you have a reason not to:
a release's deploy manifests and its node images are cut together at one tag, so
the newest release is the one whose halves are known to match. The extension
also carries a pinned release constant, but its only job now is to be the answer
when the repository cannot be reached -- on a plane, behind a proxy, with no
`git` -- and the field degrades to a text box prefilled with it.

Choosing a specific older tag is supported and unremarkable; the field is a
picker precisely so that "the release with the fix I am waiting on" is an answer
you can give.

**`main` is not a version. It is a lane, and it costs minutes.** The entry reads
`main -- build from source (for MemQL developers)` and it is for one situation:
you have repository access and you want to run the engine as it is on `main`
right now, before any release carries it. Choosing it:

- checks out `main` instead of a tag;
- **builds the node images from that checkout** with Docker and imports them
  into k3d as `memql-<node>:local`. No image is pulled from a registry for the
  engine nodes, because none is published for `main`;
- takes several minutes and needs Docker running.

The cluster then genuinely runs main's engine -- not just main's manifests. That
distinction used to matter a great deal: `main` previously meant main's checkout
paired with the newest *release's* node images, a deliberate skew that delivered
manifest and script fixes but not engine ones. It no longer does, and any prose
you find describing that skew is stale.

A `main` install shows as **checkout mode** on its Deployments row, with the
commit it was built at and how many files were dirty. `Rebuild from checkout`
on that row rebuilds and rolls it forward without reinstalling.

### Add an existing cluster

Registers a cluster that already exists somewhere else. **Nothing is installed
and nothing on the cluster is touched** -- this writes an entry in
`~/.memql/clusters.yaml` saying how to reach it.

**It asks two things: a name and a domain.** Everything else is derived from the
domain, and the hint under it shows the derivation as you type:

| From `example.com` | derives |
|---|---|
| gRPC front door | `api.example.com:443` |
| sign-in / JWKS | `https://identity.example.com` |
| portal | `https://portal.example.com` |

**Advanced** holds two fields you will usually not open. **Endpoint** is
prefilled with the derivation and only needs changing for a front door that is
not at `api.<domain>:443`; an edit there wins. **Token** is for pasting an
identity-issued access token, and the ordinary answer is to leave it empty and
run **MemQL: Sign In**, which mints one through your browser. A personal access
token (`mql_pat_...`) is refused with an explanation: the mesh verifies bearers
against the identity service's JWKS feed, so a PAT fails before any lookup.

**`localhost` and its family are refused here, on purpose.** `localhost`,
anything under `.localhost` (including `memql.localhost`), `127.0.0.0/8` and
`::1` all name this machine, and this form only records an address -- it cannot
create the hosts entries, the certificate or the cluster that would make one
answer. Registering one produces an entry pointing at a front door that does not
exist, and the failure arrives much later as a connection error naming a
hostname you typed yourself. Use **Install a local cluster** instead; that flow
takes `memql.localhost` as its own default, because it is about to make it
resolve.

**Saving probes first, and a failed probe does not stop you.** On a valid form
the extension fetches `https://identity.<domain>/.well-known/jwks.json` and
checks the endpoint is reachable. If it answers, the cluster is registered
silently. If it does not, you get the endpoint and the reason, nothing is
written, and the button becomes **Save anyway** -- because a cluster that is
stopped, behind a VPN you have not connected, or still deploying is one you may
perfectly well want registered now.

## Clusters

Connections, and nothing else: which clusters this editor can reach, and as
whom. Selecting a row opens the connection page -- the endpoint, the issuer,
whether it answered, who you are signed in as, and when the access token expires
(it renews itself; the countdown is not a countdown to being logged out).

That page is the one to open when a cluster will not come up. Nothing on it
overlaps the portal, which knows nothing about `clusters.yaml` or VS Code's
secret storage.

**Signing in needs nothing configured on either side.** Every identity node
carries this editor as a built-in first-party OAuth client (`memql-vscode`), so
**MemQL: Sign In** works against a cluster on the day it is installed. It opens
your browser and catches the callback on a loopback port; when this host cannot
do that -- a machine with no browser, or any remote window (Remote-SSH, a dev
container, WSL, Codespaces), where the callback port would be on the wrong
machine -- it falls back automatically to a short code you approve on another
device. Nothing is registered with the cluster at any point, and the `clientId`
field in `clusters.yaml` is an override you will usually not set.

**A local install must be driven from a local window.** "Install a local
cluster" writes hosts entries, issues an mkcert certificate into *this
machine's* trust store and serves the cluster at `*.memql.localhost`. From a
remote window those all land on the remote host while your browser is on your
own, and `.localhost` resolves to loopback for whichever machine asks -- so the
credential links the wizard hands you open a tab that cannot connect. Install
locally, then register the cluster from wherever you like.

**Sign-in needs the developer role or above on the cluster.** The editor is a
management surface, so writer and reader are refused -- with a message naming
your role, in both flows. Ask a cluster owner or admin to raise it.

Operator-facing detail:
[Connecting an Editor](https://github.com/znasllc-io/memql/blob/main/docs/public/operate/auth/connecting-editors.md).

**Remove takes the row, not the cluster.** It drops the entry from
`~/.memql/clusters.yaml`, deletes the credential this editor stored, and closes
the connection if it was the live one. **Nothing on the machine is touched**:
the cluster keeps running and its data is untouched. For a local cluster the
confirmation says so, and says where to go instead -- uninstalling is a
Deployments action.

**And there is a way back with nothing to re-type.** When a local cluster is on
the machine but not in the list, the **+** offers *Connect to the local cluster*:
it composes the entry from what the install recorded, or from the installer's
own default domain when the receipt is gone, and signs you in. No form at any
point.

**Uninstall** is a Deployments action, on the local instance row. It reverses
the install receipt -- the k3d cluster, the hosts-file entries, the mkcert CA,
the pinned tools -- and there is no undo, because a deleted k3d cluster takes
its database with it. It confirms against an itemised dry run rather than a
yes/no prompt: every artifact the receipt names, what will happen to it, and
which steps will ask for elevation. Anything the install *found* rather than
created is listed as **preserved** and left alone.

Remove and Uninstall are separate commands, in separate views, with separate
confirmations, and that separation is the point: one is a routine edit to a
list and the other is irreversible.

The same install runs from a terminal for scripted and CI use, and is not
deprecated: `npm run install-cli -- install` and `... -- uninstall`. It is not
a second implementation -- `src/install/session.ts` holds the orchestration and
both the page and `src/install/cli.ts` are callers of it, so there is no second
run path to drift out of step.

Operator-facing detail: [VS Code Runtime Panel](https://github.com/znasllc-io/memql/blob/main/docs/public/language/vscode-runtime-panel.md).

## Constructs

Everything the connected cluster has LOADED, grouped by kind and then by
namespace, read from the engine's own registry. A construct added to the DSL
appears with no extension update, and a kind this extension has never heard of
still renders -- under its own name, at the end -- because a client is expected
to outlive several engine releases and a view that silently drops what it does
not recognise disagrees with the cluster it claims to describe.

This is what a `.memql` file cannot tell you: **what is actually loaded there**,
which is not the same as what is in your checkout.

Selecting one opens its detail page -- kind, namespace, origin, bound concept,
arguments with their types and flags, and the way back to its source.

### Where a construct came from

Four origins, and they are four different situations rather than four labels.
The order is the tier order -- sealed, shipped, shared, private -- rather than
alphabetical:

| Origin | What it means |
|---|---|
| **core** | The engine's embedded DSL tree. |
| **bundle** | A product's DSL, mounted at `MEMQL_DSL_PATH`. |
| **promoted** | It lives in the cluster's database and **has no file at all**. |
| **staged** | The same place as promoted, with a different audience: **only you can call it** until it is trained. |

`staged` is a sibling of `promoted` rather than a qualifier on it. The two
differ in WHO can call the construct, which is the question every consumer of
the field is asking -- and it is the reason the read-only rule treats them
identically further down: neither lives in a sealed tree, and for both the file
on disk is the author's own working copy.

Jump-to-source has three answers rather than four -- a staged construct has no
file either, so it shares promoted's -- and one of the three changed. When the
file is in your workspace it opens, revealed at the signature. When there is no
file at all, the source is rendered on the page from what the cluster holds,
labelled as living in the database -- the case where a developer first meets
the seeded-versus-trained distinction.

**The answer in between used to be a dead end and is now an action.** The
catalog reports a path relative to the CLUSTER's tree, and a remote cluster is
usually not the checkout you have open, so the page named the path and stopped
there. But the cluster that loaded the construct also serves the file, over its
pack browser -- so the page offers **View source from cluster**, which opens it
as a read-only `memql-cluster://` document: at the signature, badged `RO`, with
one header lens back to these details.

That document gets `memql` highlighting and **no diagnostics**, which is
deliberate rather than incomplete. The language server is an offline process
over a workspace directory, and the file it is being shown is not in one; the
imports a cluster document names resolve against the cluster's tree, not
against yours, so analysing it would fill the screen with unresolved references
to somebody else's checkout. It is a reading surface.

A cluster document also names its own cluster, and the lens refuses rather than
guesses: a document opened from one cluster does not resolve its construct
against a different one you have since connected to.

### A concept's rows

A **concept** is a schema, so its detail page carries one more action nothing
else does: **Browse rows in portal**, which opens that concept's rows at
`/concepts/<id>` in the cluster's own portal. It is the return leg of the
handoff: the portal hands a definition to the editor, and the editor hands a
concept's rows back. The address is resolved the same way **Open Portal**
resolves it -- from the portal's own site row when there is a connection to read
it over, composed from the cluster's domain when there is not. No other kind has
rows, so no other kind draws the button; the absence is the statement, exactly
as it is for Run below.

The Data view is still where rows are browsed inside the editor. This is a
door, not a replacement for it.

### Running from the catalog

**query, mutation, logic and tool** run from the detail page, through exactly
the run path a CodeLens uses -- the same argument form, the same preflight, the
same Result view, and the same write confirmation. Browsing a catalog is not a
quieter way to write to production: a mutation against a non-local cluster
still asks (memql#3309).

**An automation runs too, through its own form** rather than the argument one.
It is fired by an event, so what it needs is not arguments -- it has none -- but
a trigger: which payload modes to offer, and which concept's rows the picker
browses. `ListConstructs` carries that trigger (memql#3805), so the detail page
opens the same automation form a `.memql` file's CodeLens does, with the same
row picker and the same event synthesis.

Its button reads **Run automation...** rather than **Run**, and the ellipsis is
load-bearing: for the other four kinds a click invokes, while here it opens a
form that ends in firing a real event on a real cluster. An automation the
cluster reports no trigger for is manual-run -- a real, describable form -- so
it is offered as one rather than withheld.

**The other eight kinds -- spec, trait, prompt, seed, concept, shape, provider,
builtin -- are not runnable, and that is a decision rather than a gap.** Each
would need an execution semantic settled before a Run button could mean
anything, and none of them has one:

- a **spec** or **trait** is a predicate that compiles into a SQL `WHERE`
  fragment or evaluates against the auth envelope; running one alone means
  choosing rows to run it against, which is a query;
- a **shape** is a projection with no inputs and no return;
- a **concept** is a schema -- what "running" it would even be is undefined,
  and browsing its rows is the Data view;
- a **prompt** would spend money on a model call, so it needs a cost decision
  before it needs a button;
- a **provider** is a vendor record, a **seed** writes fixture rows, and a
  **builtin** is the Go implementation behind a DSL name, reachable through the
  tool or function that declares it.

So the absence of a Run button is the statement. There are no disabled buttons
here -- a control that cannot work is not drawn.

**Nothing on this page edits.** Editing happens in a `.memql` file, where the
language server owns it. A surface that could change a construct from here
would be a second authoring path for something that already has one.

Full rationale: [the Constructs view design](https://github.com/znasllc-io/memql/blob/main/docs/superpowers/specs/2026-08-14-vscode-constructs-view-design.md).

## Open from the portal

The portal's concept page has **Open definition in VS Code**. It is a link of
one shape:

    vscode://znasllc.memql/open?v=1&cluster=<domain>&kind=<kind>&name=<registry key>

and this extension handles it in four steps: match `cluster` against the
domains in `clusters.yaml`, select and connect that cluster (the same sign-in
you would get from the tree), find the construct in its catalog, and open it
where it is -- the file in your workspace, revealed at its signature; the local
cluster's checkout, if you have none of it open; a read-only document served
from the cluster, when the file is not on this machine; or the construct's
detail page, when it was promoted and has no file.

**A link may select a registered cluster, connect it, and open a document. It
may never add a cluster, sign in silently, run anything, or write settings.**
An unregistered domain gets an offer to add it through the ordinary prompts;
a malformed link is refused and the refusal names the field. VS Code's own
"allow this extension to open the URI" prompt is the consent gate; there is no
second one.

## Training: what the cluster knows about the file you are editing

The Constructs view answers *what is loaded on this cluster*. Training answers
the same question **about the buffer in front of you**, construct by
construct, and offers the actions that change the answer.

The distinction it exists to make visible: a construct is either **seeded** --
loaded from disk when the cluster booted, changeable only by a rollout -- or
**trained**, a row in the cluster's database that went live in seconds and
survives restart. Both are real. Until this surface existed, only the first was
visible. The full model is in
[Training Constructs Into a Running Cluster](https://github.com/znasllc-io/memql/blob/main/docs/public/language/training.md).

### Three surfaces, one state

The language server owns the state. It parses the document, hashes each
construct's source, compares against the cluster's catalog, and reports one of
`untrained` / `drifted` / `trained` / `staged` / `seeded` / `edited` / `unknown`.
The extension renders; it never computes a state, because a client re-deriving
drift would be a second opinion about the one question the surface exists to
answer.

**`edited` is the seventh, and it is not `drifted`.** Drift is defined against a
promotion -- the cluster knows this construct, and you have moved past the
version that was promoted. A construct the cluster loaded **from disk** and
whose source has since moved was never promoted at all, so there is nothing to
promote over: what applies it is a rollout, which on a local cluster is
**Rebuild from checkout** and on a remote one happens in a pipeline this editor
has no hand in.

**A gutter icon** beside each construct's signature. Three marks, not seven,
because the gutter answers one question -- *does what I am looking at match
what runs?* -- and that question has three answers: `untrained`, `drifted`
(which `edited` shares, the answer being the same *no*), and live (`trained`,
`staged` and `seeded` alike). They are distinguishable without relying on
colour. Who can CALL a live construct is a question about actions, and the lens
is where it is answered.

`unknown` gets **no mark at all**. Not a grey icon, not a question mark: at
sixteen pixels, a mark meaning "we do not know" is indistinguishable from one
meaning "something is wrong here", and a disconnection must not look like a
problem with your file.

**A CodeLens** above each signature, beside the Run lens, naming the state and
offering what can be done about it:

```
untrained   Dry-run   Try in session   Stage   Promote
drifted     Dry-run   Try in session   Stage   Promote (updates the trained version)
staged      Re-stage   Train (make it live for everyone)   Demote
trained     Demote
seeded      (no action -- changing it needs a rollout)
edited      Rebuild from checkout   (a local cluster only; a remote one needs a rollout)
```

The order on the first two rows is the **escalation**: a session that ends, a
private one that does not, and one everybody gets.

**`edited` is the one state whose lens depends on the cluster rather than on
the construct.** The other six describe what the cluster knows; this one
describes what would apply the difference, and the answer differs by locality.
So its sentence names the cluster -- "seeded constructs change by rollout" is
abstract until it says which cluster is going to need one -- and its only
button, on a local cluster, is the Deployments command `Rebuild from checkout`
rather than a seventh training action. A remote cluster gets the words and no
button: a disabled control would suggest the editor could do it if only
something were different.

The state label is not a command. It is a fact about the construct, and making
it clickable would leave a developer wondering what clicking it does. A
`seeded` construct gets no disabled buttons either -- a control that cannot
work is not drawn.

**A status-bar item** for the active document: `3 untrained · 1 drifted · 2
edited`. It reports only what needs attention, so a file whose constructs are
all trained shows nothing -- an item that is always present saying "12 trained"
is one you stop reading, and it would take the warning with it. `edited` is in
the reported set because it is the same complaint about a different tier: *I
changed this and the cluster is still running what it booted with.* Its hover
is where *saving does not promote anything* is said in words.

**Nothing runs automatically.** No promote-on-save, no train-on-save. The
extension already holds that line for runs, and a promotion is a strictly
larger commitment than a run.

### The five actions

| Action | What it does | Scope |
|---|---|---|
| **Dry-run** | compiles and binds in the engine's sandbox against a read-only clone of the live registry | nothing is mutated; safe against production |
| **Try in session** | makes the construct callable by name | this connection only, dropped at disconnect |
| **Stage** | persists it and registers it into your own durable tier | you only; survives restart and reconnect; owner-or-developer |
| **Promote** | persists it, registers it cluster-wide, propagates to every node | durable; survives restart; owner-only |
| **Demote** | withdraws it, from whichever tier it is in | owner-only for a trained construct |

**Train is Promote.** The lens over a staged construct labels it *Train (make
it live for everyone)* because that is the consequence, but there is no separate
command: the engine sees the construct is staged for you and flips the same
persisted row rather than writing a second one.

**A concept cannot be staged**, and the refusal names it. A concept registers
into the one shared concept registry, so there is no owner-scoped form of it --
train the concept, then stage the constructs bound to it.

Dry-run diagnostics land **at the construct**, and clear on the next run.

**Promote carries the closure, not just the construct.** Promoting a query
whose spec is untrained must not half-land, so what is being committed is shown
before it is committed.

**Try in session is visibly temporary**, in the confirmation and in the lens.
Mistaking it for a promote is the most expensive confusion this surface can
cause, because the difference between the two is the whole design. **Stage** is
the durable answer to that temporariness: same owner-scoping, same invisibility
to everyone else, and it does not die with the connection.

For a **concept**, two outcomes render structurally rather than as an error
string: a demote reports whether the concept was *retired* (rows exist) or
*removed* (it was empty), with the row count; and a re-promote whose schema
changed reports the classified diff, additive versus breaking, with the field
named and the override alongside it.

### Read-only files

Some `.memql` files are marked read-only, under one rule whose words have not
changed: **a file is read-only exactly when editing it cannot change what the
cluster runs.** What changed is the answer the rule gives on a local cluster,
because **Rebuild from checkout** made an edit to one real.

| File | Connected cluster | Editable |
|---|---|---|
| any origin, core included | **local** | yes |
| core engine `dsl/` | remote | no -- badge `C` |
| product bundle `dsl/` | remote | no -- badge `R` |
| promoted or staged | any | yes -- it lives in the database, not in a tree |
| a new file | any | yes -- this is the training path |

**A local cluster locks nothing, for any origin.** It is rebuilt from a checkout
on this machine, so an edit to any file it loaded -- core included -- is an edit
the next rebuild compiles, and the condition for read-only is not met. Nothing
about it is a permission: the safe direction is the developer's own file staying
writable.

**Against a remote cluster the two locked rows have different ways out.** A
remote cluster loads its bundle from its own image, so editing a local checkout
of that bundle changes nothing there -- select the local cluster and work in the
checkout it rebuilds from. Core constructs are additionally sealed against
promotion by the engine's core-first invariant, so on a cluster nothing here is
rebuilt into, an edit to one changes nothing it runs.

A **new file is never blocked**, on any cluster. Adding one is how training
starts, and a path the catalog has never heard of is the degenerate case of
that -- so the rule holds by construction rather than by a guard.

**Which clone the cluster rebuilds from is a hint, not a lock.** The install
receipt records one directory. With a local cluster selected and a *different*
clone of the same repository open, every file stays editable -- it is your file
-- and the ones that cluster loaded carry an `L` badge whose hover says this is
not the folder it rebuilds from. Locking them instead would be the editor
deciding which of your checkouts is the real one, which is not its decision to
make.

**A cluster document is read-only by construction**, which is a different
mechanism rather than a stricter setting. The bytes behind `memql-cluster://`
are served from the cluster's own pack browser and there is no file on this
machine to write back to, so the answer depends on no catalog and waits for
none. It carries the badge `RO` and a hover naming the cluster it came from;
`files.readonlyInclude` is not involved, because there is nothing to forbid.

The classification comes from the cluster's own `origin` for each construct,
not from the shape of the path: guessing by matching a directory called `dsl/`
would be wrong the first time a product bundle also lives in one, which is the
convention.

The marking is a **courtesy, not the control**. The extension manages
`files.readonlyInclude` in workspace settings and you can override it. What
actually refuses is the engine, which will not let a promoted construct shadow
a core name. The editor explains; the engine enforces.

With no connected cluster, every file stays editable -- there is no "what the
cluster runs" for an edit to fail to change.

Full rationale: [Training Constructs Into a Running Cluster](https://github.com/znasllc-io/memql/blob/main/docs/public/language/training.md).

## Data

Every registered concept on the connected cluster, grouped by domain. Click one
to browse its rows: the list on the left, the selected row's full nested shape
on the right -- payload, provenance and intrinsics kept distinct, nothing
flattened away. The list updates live as rows are created, updated and deleted,
and **Load more** pages through the whole concept.

There is no concept-specific rendering anywhere in the panel, deliberately: it
is what lets a newly declared concept work the day it is declared. Rows are
labelled through whatever `@displayCard` slots the concept declares and fall
through to a stated contract when it declares none.

Row values, run results and automation step output all render through **one**
viewer, which collapses, badges types, filters, and stays bounded on a payload
too large to draw. It lives in `sdk/ts-viewkit` so the portal renders values
the same way.

## Install / update the extension locally

One command builds the extension (building a fresh `memql-lsp`) and
(re)installs it into VS Code:

```bash
make vscode-install          # from the repo root
```

Then reload the editor to pick up the new server: run **Developer: Reload
Window** (Cmd/Ctrl+Shift+P), or restart VS Code. Because the language
intelligence lives in the bundled `memql-lsp` binary, this is the loop to run
every time you change the server or the extension -- `--force` overwrites the
installed build, so re-running it just updates in place.

Options (via `scripts/vscode/install.sh`, or `EDITOR_CMD=` on the make target):

```bash
make vscode-install EDITOR_CMD=cursor        # install into Cursor / code-insiders / codium
bash scripts/vscode/install.sh --no-build    # reinstall the last-built .vsix (skip the rebuild)
bash scripts/vscode/install.sh --help
```

If the editor CLI is missing, run "Shell Command: Install 'code' command in
PATH" from the VS Code command palette.

## Appearance

The MemQL panels wear the portal's palette -- the same hexes memql.io and the
MemQL Portal render. Which of the two they wear is YOUR choice, not the
editor's:

`memql.appearance` -- `system` (default) | `light` | `dark`

- **`system` means follow the EDITOR's colour theme.** Inside VS Code the
  editor is the ambient theme, and it is what tracks your operating system if
  you have asked it to. This matches what "system" means in the portal relative
  to its own host, the browser.
- **`light` / `dark` pin the MemQL palette** regardless of what the editor is
  set to. A light editor with dark MemQL panels is a supported combination.
- **A high-contrast editor theme always wins.** The panels then defer entirely
  to VS Code's own colours and this setting is ignored. A themed green is not
  worth an accessibility regression.

Changing it repaints any panel you already have open -- no reload.

### Why the sidebar does not follow it

Tree rows, the activity bar, view titles, tabs and the status bar are drawn by
the **workbench**, not by the extension. VS Code offers extensions `ThemeIcon`
and `ThemeColor` for those surfaces and nothing else -- there is no API by
which an extension can colour a tree row. This is a platform constraint, not a
gap in this extension, and it is why the MemQL tree items speak VS Code's
`charts.*` vocabulary (green = healthy, red = error, yellow = needs attention,
purple = in progress) rather than a MemQL hex.

So the only way the chrome AROUND the panels can wear the brand is for VS
Code's own colour theme to be a MemQL theme. The extension ships two:

- **MemQL Dark**
- **MemQL Light**

Pick one from `Preferences: Color Theme` (`Ctrl+K Ctrl+T` / `Cmd+K Cmd+T`).
They are opt-in and the extension never changes your theme on its own; the
first time it activates on a non-MemQL theme it offers once, records your
answer either way, and does not ask again.

The two themes brand the workbench -- editor, sidebar, activity bar, status
bar, tabs, lists and selection, buttons, badges and inputs -- plus a minimal
four-rule syntax split (comments, strings, numbers, keywords). They are
deliberately **not** a full syntax theme, and they deliberately leave the
sixteen `terminal.ansi*` colours at VS Code's defaults: those are a contract
between your shell prompt, your `ls` colours and every TUI you run, and a brand
green in that palette reads as "this file is executable".

### If the palette changes

The hexes live in exactly one place, `src/webview/palette.ts`, and both the
panel CSS and the two theme files are derived from it. When the portal palette
changes (upstream is `brand/` at the repository root -- see memql#4177):

1. Edit `src/webview/palette.ts`.
2. Run `node scripts/generate-themes.mjs`.
3. Commit `palette.ts` **and** both files under `themes/`.

`npm test` fails if the committed theme files disagree with the palette, so a
step-1-without-step-2 cannot ship. `node scripts/generate-themes.mjs --check`
is the same comparison from a shell. The test suite also holds every
foreground/background text pair in both themes to WCAG AA (4.5:1), so a new
palette that is unreadable in the workbench fails there rather than in
somebody's editor.

## Requirements

The `memql-lsp` binary, for the LANGUAGE FEATURES only. The extension resolves
it in this order:

1. The `memql.lsp.serverPath` **user** setting, if set (see Settings below --
   a workspace-scoped value is refused).
2. A bundled platform binary at `bin/<platform>-<arch>/memql-lsp` (added at
   packaging time -- `make vscode-install` / `make vscode-package` bundle it).
3. `memql-lsp` on your `PATH`.

Build it from the memql repo with `go build -o bin/memql-lsp ./cmd/memql-lsp`.

When none of the three resolves, the extension says so and keeps going: only
highlighting, diagnostics, completion, hover and signature help are lost. The
runtime surface -- the Clusters, Concepts and Runs views, and connecting to a
cluster -- needs nothing from the language server and is unaffected. (The Run
CodeLens does need it, because the constructs it offers are read from the
server.)

## Security: the information policy

The extension is a public tool that holds credentials and talks to clusters,
so what its UI may SHOW is a policy, not a habit (audited in memql#4194). New
surfaces inherit these rules; the mechanically checkable ones are enforced by
the test suite (`test/clusterForm.test.ts`, `test/clusterStatus.test.ts`,
`test/displayRedaction.test.ts`, `test/diagnostics.test.ts`).

1. **Panels, toasts and tooltips carry a short, classified verdict; the raw
   material lives in an output channel.** Three channels: `MemQL Install`
   (capability stderr and run refusals), `MemQL Connection` (dial, sign-in and
   language-server failures -- every dial failure is recorded exactly once, at
   the connection-state seam), `MemQL Training` (schema diffs and outcomes).
   A toast that has more to say offers "Show details", which reveals the
   channel. A hover can be neither scrolled nor copied, so it is never the
   only home of a diagnostic.
2. **One redactor.** Everything on its way to a file OR a human surface goes
   through `src/install/secrets.ts`: the receipt/run-log withholding gates,
   and `redactForDisplay` (home directory masked to `~`, `sk-`/`mql_*_`
   credentials scrubbed) for channel and panel text.
3. **Credential inputs are never prefilled.** The token boxes show empty
   whatever is stored; the prompt says that something is stored; an empty
   answer keeps it. Removing a credential is sign-out's job, which also clears
   the SecretStorage half. (`src/clusters/form.ts` is the seam.)
4. **Reveal-once credentials are shown by explicit click, once, and nowhere
   else.** The recovery key renders only after "Reveal the recovery key" on
   the install done screen; it never reaches the run log, the receipt, or any
   channel (`src/install/recoveryKey.ts` is the narrow, tested seam -- do not
   widen it). The device code is the deliberate exception: the user must read
   it, so it stays on the progress line and its notification.
5. **Addresses are detail, not decoration.** The cluster list and QuickPick
   show state + version; the endpoint lives in the row tooltip and on the
   Connection page. Internal node ids stay in tooltips. The signed-in email
   appears on the Connection page only.
6. **Values stay out of hovers.** The Runs tree shows argument NAMES and
   SHAPES; the values are in the configurations file, one command away.
7. **Files that hold credentials are 0600.** `clusters.yaml` (plaintext
   access token, shared with the Cockpit), the install receipt and the run
   logs are written owner-only, and their writes pass the withholding gates.
8. **The sudo password is asked by VS Code's own input, held in memory for
   the one run, never exported to an env var or a file**
   (`src/install/sudoAgent.ts` documents the trade).

## Development

This surface is "the VS Code extension" (VS Code's own marketplace word),
never "the plugin". The commit scope for extension work is `vscode`:
`feat(vscode)` / `fix(vscode)`.

```bash
make vscode-deps                 # from the repo root -- see below
cd editors/vscode && npm ci && npm run compile
```

`make vscode-deps` is not optional on a clean checkout. The extension consumes
`sdk/ts` and `sdk/ts-viewkit` as `file:` dependencies, and their `main` /
`types` point into `dist/` -- which does not exist until those packages are
built. Skipping it leaves the symlinks resolving to nothing and `tsc -p ./`
fails.

Press `F5` to launch an Extension Development Host, set `memql.lsp.serverPath`
to your built binary, and open a folder of `.memql` files.

### The context keys (contributor notes)

Three context keys drive every `when` clause in `package.json` that is not
simply `view == ...`. Each has exactly ONE writer, and adding a second writer
to any of them is a race whose winner depends on listener registration order.

| Key | Means | Written by |
|---|---|---|
| `memql.clusterSelected` | This editor has a cluster in hand: dialling one, holding one, or tried and refused | `ConnectionManager` |
| `memql.connected` | That cluster's transport is up right now | `ConnectionManager` |
| `memql.deploymentsInstance` | What the selected cluster IS: `memqlLocalInstance`, `memqlLocalInstanceAbsent`, `memqlRemoteInstance`, or unset | `DeploymentsTreeProvider` |

**The first two are the connection, and only the manager publishes them**
(memql#4424). It is the one object that knows when either changes, so it maps
its own state through `src/state/connectionContext.ts` -- a pure, table-tested
module -- and writes the pair through an injected sink. `ConnectionManager`
itself must stay free of `vscode` imports, so the `setContext` call lives in
`src/extension.ts`; the DECISION does not. Every provider is injected with a
reader of the same mapping rather than asking the manager itself, so a view
cannot disagree with the workbench about whether a welcome should be showing.

`clusterSelected` is **not** "clusters.yaml names a `selectedCluster`". A
registry entry with no dial behind it is a name in a file, and the views keyed
on this render cluster data: a fresh window with a remembered selection and no
connection has nothing to draw. A cluster that was chosen and did NOT answer is
`clusterSelected: true` and `connected: false` -- selected-but-unreachable is a
fact about something, and it belongs in a view's rows and description, not in an
empty state.

**The third describes the selection, not the connection** (memql#4426), and the
Deployments view is its one writer for the same reason the manager owns the
others: it is the only thing that computes the catalog. It exists because the
instance actions moved to the view TITLE menu when the instance row went, and
`view/title` clauses are evaluated with no `viewItem` in scope -- so the row's
contextValue vocabulary had to become a key to survive. It is written on every
pass, including as `""`, so a stale value cannot offer Uninstall over a remote
cluster.

**Welcomes only render over an EMPTY tree.** That is the whole mechanism behind
"Not connected": the Deployments, Constructs and Data providers return `[]` when
`!memql.clusterSelected`, and any row at all -- including a well-meant
"Not connected" placeholder -- silently deletes the welcome. Runs is the stated
exception: it lists the workspace's own `runs.json`, so it keeps listing and
`memql.runs.execute` refuses instead.

### Testing

```bash
make vscode-test        # unit lane -- bare node --test, seconds, no Electron
make vscode-test-host   # host smoke lane -- downloads and drives a real VS Code
```

The two lanes answer different questions. `vscode-test` covers the modules that
do not import `vscode`; it is fast and dependency-light and must stay that way.
It also covers `package.json` itself, because the tree's context menus and view
welcomes are decided by `when` clauses the workbench evaluates and no host API
can read back the entries it drew -- a clause edited to match no row would
otherwise remove an action from the product with nothing noticing
(`test/clusterMenus.test.ts`), and a welcome keyed on a misspelt context key
renders permanently, because VS Code treats an unknown key as unset and
`!unset` is true (`test/viewsWelcome.test.ts`).
`vscode-test-host` (`editors/vscode/test-host/`) launches a real Extension
Development Host to assert what a unit test structurally cannot reach -- that
activation survives the host's runtime, that every command the manifest
contributes is actually registered, that the activity-bar contributions were
accepted, that a file watcher fires for a path outside the workspace, and that
each webview opens. It needs a display, and falls back to `xvfb-run` when
`DISPLAY` is unset. CI runs it against both the declared `engines.vscode` floor
and current stable, because that floor is where this bug class actually fires.

Neither lane dials a cluster. Everything downstream of a connection is verified
by hand against the [manual verification
checklist](https://github.com/znasllc-io/memql/blob/main/docs/public/language/vscode-runtime-panel-verification.md).

## Settings

- `memql.appearance` -- `system` (default) | `light` | `dark`. Which palette
  the MemQL panels use; `system` follows the editor's colour theme. A
  high-contrast editor theme overrides it. See [Appearance](#appearance).
- `memql.lsp.serverPath` -- absolute path to the `memql-lsp` binary. **User
  settings only.** A value in workspace settings (`.vscode/settings.json`) is
  refused, and the extension shows a warning saying it was: an opened folder is
  not trusted to name an executable this extension then runs, so honouring one
  would hand any repository arbitrary code execution. Set it in User Settings.
- `memql.lsp.trace.server` -- `off` | `messages` | `verbose`.
