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

## The five views, and which question each answers

An activity-bar panel connects the extension to a running cluster. It requires
a trusted workspace, since it reads credentials and opens a network connection.

| View | Answers |
|---|---|
| **Deployments** | What do I operate, at what version, and what changed it |
| **Clusters** | Which clusters can I reach, as whom |
| **Constructs** | What is DEFINED on this cluster |
| **Data** | What rows EXIST |
| **Runs** | What have I run against a cluster |

Constructs and Data are the two halves of one question, and keeping them apart
is what the naming is for: a query is a definition, the rows it returns are
data, and a *concept* is itself a construct. The view that browses rows was
called Concepts until memql#3754 -- which was the wrong name from the start,
since it never showed a concept's definition. It shows rows, so it is Data, and
its commands moved with it: `memql.concepts.*` is now `memql.data.*`.

### The boundary

One rule decides where every surface in this extension goes, including ones
nobody has proposed yet:

> **The plugin owns what is on your machine and what you can reach.
> The portal owns what is inside a cluster.**

| Question | Surface |
|---|---|
| What instances do I operate, at what version? | plugin -- Deployments |
| Install / upgrade / repair / uninstall / roll out | plugin -- Deployments |
| Which clusters can I reach, as whom? | plugin -- Clusters |
| Which pods run, which are orphaned, which tier is under-replicated | **portal** |
| Integrations, identity, sites, accounts | **portal** |
| What does this construct do, what rows exist | plugin -- Constructs / Data / Runs |

**Topology used to be here and is not any more.** A pod grid, orphan verdicts
and under-replica alarms are cluster state, the portal already draws them, and
two surfaces answering one question diverge on the day the second one ships.
That is the rule, not a preference about which UI is nicer -- so a pod grid
proposed for this extension has its answer before anyone writes it. Every
cluster's portal is one click away: **Open Portal**, on the Clusters row and on
the connection page.

Full rationale: [the Deployments surface design](https://github.com/znasllc-io/memql/blob/main/docs/superpowers/specs/2026-08-14-vscode-deployments-surface-design.md).

## Deployments

Instances at the top, runs beneath, newest first.

```
DEPLOYMENTS
|- local     healthy - v0.17.0
|  |- upgrade   v0.16.1 -> v0.17.0   succeeded   2d ago
|  \- install                        succeeded   9d ago
\- staging   healthy - v0.9.2
   |- rollout v0.9.2   succeeded     1d ago
   \- rollout v0.9.1   rolled_back   3d ago
```

**An instance** is a memQL you operate. It is derived rather than declared:
`local` is whatever is on this machine (an install receipt, a `local: true`
registry row, and whether the front door answers), and every other
`clusters.yaml` entry is a remote one. A machine with no local cluster still
shows the `local` row, as `not installed`, carrying **Create deployment** as its
only action -- that row is where an operator with nothing installed starts.

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
| Uninstall | -- | the uninstall graph, behind its preview | -- |
| Cut version / Promote / Rollout / Roll back | -- | -- | by role |

**Re-running the install graph is the repair, and it is also the upgrade.**
Every step verifies before it acts and skips whatever is already satisfied, so
one graph serves all three: an install does everything, a repair does whatever
is missing, and a deployment to another tag moves the checkout and reconciles.
Before it starts, the page shows which steps will actually change something --
usually two of fifteen -- because a run reporting fifteen steps looks like a
reinstall to whoever is watching it.

**Choosing a version never happens by itself.** The tag list comes from the
checkout's origin, newest first, and nothing is pre-selected: a version somebody
picked off a list is a fact they can be held to, and one the plugin picked
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

## Clusters

Connections, and nothing else: which clusters this editor can reach, and as
whom. Selecting a row opens the connection page -- the endpoint, the issuer,
whether it answered, who you are signed in as, and when the access token expires
(it renews itself; the countdown is not a countdown to being logged out).

That page is the one to open when a cluster will not come up. Nothing on it
overlaps the portal, which knows nothing about `clusters.yaml` or VS Code's
secret storage.

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

Three origins, and they are three different situations rather than three
labels:

| Origin | What it means |
|---|---|
| **core** | The engine's embedded DSL tree. |
| **bundle** | A product's DSL, mounted at `MEMQL_DSL_PATH`. |
| **promoted** | It lives in the cluster's database and **has no file at all**. |

Jump-to-source has the same three answers. When the file is in your workspace
it opens, revealed at the signature. When it is not, the page says so and names
the path -- the catalog reports a path relative to the CLUSTER's tree, and a
remote cluster is usually not the checkout you have open. When there is no file,
the source is rendered on the page from what the cluster holds, labelled as
living in the database. That last case is where a developer first meets the
seeded-versus-trained distinction.

### Running from the catalog

**query, mutation, logic and tool** run from the detail page, through exactly
the run path a CodeLens uses -- the same argument form, the same preflight, the
same Result view, and the same write confirmation. Browsing a catalog is not a
quieter way to write to production: a mutation against a non-local cluster
still asks (memql#3309).

**An automation does not**, and the page says why rather than leaving an
unexplained gap. An automation is fired by an event, so its form is decided by
its TRIGGER -- which payload modes to offer, which concept the row picker
browses -- and `ListConstructs` does not carry one. A form missing the field
that decides it would fire a real event on a real cluster with a payload nobody
chose. Open it from its `.memql` file to run it. Tracked as memql#3805, which
adds the trigger to the wire.

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

## Development

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

### Testing

```bash
make vscode-test        # unit lane -- bare node --test, seconds, no Electron
make vscode-test-host   # host smoke lane -- downloads and drives a real VS Code
```

The two lanes answer different questions. `vscode-test` covers the modules that
do not import `vscode`; it is fast and dependency-light and must stay that way.
It also covers `package.json` itself, because the tree's context menus are
decided by `when` clauses the workbench evaluates and no host API can read back
the entries it drew -- a clause edited to match no row would otherwise remove an
action from the product with nothing noticing (`test/clusterMenus.test.ts`).
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

- `memql.lsp.serverPath` -- absolute path to the `memql-lsp` binary. **User
  settings only.** A value in workspace settings (`.vscode/settings.json`) is
  refused, and the extension shows a warning saying it was: an opened folder is
  not trusted to name an executable this extension then runs, so honouring one
  would hand any repository arbitrary code execution. Set it in User Settings.
- `memql.lsp.trace.server` -- `off` | `messages` | `verbose`.
