# VS Code Clusters Surface: Lifecycle, Enablement, and the Add-a-Cluster Page

**Date:** 2026-08-09
**Status:** Design approved; ready for implementation planning
**Surface:** VS Code extension (`editors/vscode`)
**Supersedes:** §4.5 of `2026-08-08-local-cluster-install-wizard-design.md` on the choice of renderer (see D4)
**Delivers:** Epic 2 of that design, plus the Clusters-tree gaps it never covered

---

## 1. Problem

The Clusters surface can register a cluster and edit it. It cannot get rid of
one, it cannot install one, and it cannot tell you when its own buttons will not
work. Four failures, observed in use:

1. **A cluster cannot be removed.** `package.json` contributes
   `memql.clusters.{refresh,select,add,edit,signIn,signOut,disconnect,signInWithCode}`.
   There is no command that deletes a row. A cluster added by mistake is
   permanent unless the operator hand-edits `~/.memql/clusters.yaml`.

2. **Actions are enabled on a cluster that cannot run them.** `actionsHtml()` in
   `src/webview/clusterPanel.ts` computes its buttons from `visibleActions(visibility)`
   — the caller's *role*, and nothing else. Connection state never enters the
   decision, so a cluster that is unreachable, or whose credential expired,
   renders a full set of live-looking deploy buttons directly beneath a warning
   saying it is not connected.

3. **Repair reports a command instead of running one.**
   `launchLocalClusterInstaller()` (`src/extension.ts:1257`) shows an
   information message naming `npm run install-cli -- install` and offers to
   copy it. It is an explicit placeholder — its own comment says so — but to an
   operator it reads as the feature failing.

4. **There is no uninstall, and the entry point is a palette.** The `+` button
   opens a quick pick. A local cluster that exists and does not answer can be
   *offered* a repair, but there is no way to remove it from the machine at all,
   and every branch terminates in the stub above.

The substrate for (3) and (4) already exists. Epic #3357 shipped the step graph,
the host executor, the receipt, and a working `cli.js install|uninstall`. What is
missing is the UI that drives it.

---

## 2. Constraints discovered in the tree

Findings, not assumptions. Each one closed off a direction.

### 2.1 The renderer is view-kit, not React

The prior design (§4.5) specifies "VS Code webview (React, Style A)". The
extension has since converged on `@znasllc-io/memql-view-kit`, whose README
states two hard rules: **no DOM** (`renderToHtml` returns a string) and **no
inline event handlers** (interactivity is data attributes plus one delegated
listener, because the webview runs under a CSP that forbids inline script).
`src/webview/clusterPanel.ts` and `conceptPanel.ts` both render this way, and
view-kit already ships `renderChecklist` — the exact shape of a wizard's step
list.

**Consequence:** the wizard is a view-kit panel. Introducing React would add the
extension's only React surface, forgo `renderChecklist`, and contradict the
no-DOM premise the package is built on. The prior design predates view-kit; this
document supersedes it on that point alone.

### 2.2 The install seam was designed and left unimplemented on purpose

`src/extension.ts:1248` names what replaces the stub: *"the install wizard from
the install-substrate epic. When that entry point is callable, the body of this
function becomes the call to it and nothing else in this file changes — the
menu, the verdict, and the invalidate() that follows a completed run are all
already wired."*

**Consequence:** this epic does not design the seam. It builds the callable
entry point the seam was left waiting for, and deletes the stub.

### 2.3 Repair is install, re-run

Every graph step runs its verify predicate first and skips when already
satisfied — the `changed=false` behaviour `up.sh` has today. `extension.ts`
already records the consequence: *"`repair` runs the same graph as `install`
... Only the wording differs."*

**Consequence:** repair is not a second code path, a second graph, or a second
set of tasks. It is one flag on the same run, affecting labels only.

### 2.4 `local` is strictly-typed evidence, and absence means remote

`src/clusters/presence.ts` insists on `c.local === true` rather than a
truthiness test, because every cluster registered before that field existed
carries no flag, and reading those as local *"would report an operator's staging
cluster as the local install."*

**Consequence:** the tree's local-vs-remote row distinction inherits the same
strictness. A missing flag is a remote cluster.

### 2.5 The three unusable states are already distinguished, once

`src/clusters/status.ts` exists precisely to separate "your credential expired"
from "your cluster went away" (memql#3385), and returns `credential` / `failed`
/ `idle` / `unconfigured` as distinct verdicts with distinct icons.

**Consequence:** the panel's enablement rule consumes that verdict. Computing a
second classification inside the panel would produce two answers to one question
that drift apart.

### 2.6 The `+` menu rule is already written and tested

`addClusterMenu(verdict)` maps `absent` / `installed-healthy` /
`installed-unreachable` onto the offered actions, with the load-bearing property
documented in place: *"INSTALL APPEARS FOR `absent` AND FOR NOTHING ELSE."*

**Consequence:** the page reuses that function and renders its result as cards.
Restating the rule in the webview would create a second place for it to be wrong.

---

## 3. Decisions

### D1 — Two destructive actions, never one

Removing a `clusters.yaml` entry and uninstalling a cluster from the machine are
different operations with different blast radii. They get different commands,
labels, icons, menus, and confirmation shapes:

| | **Remove from list** | **Uninstall local cluster…** |
|---|---|---|
| Command | `memql.clusters.remove` | `memql.clusters.uninstall` |
| Where | inline trash icon, **every** row | context menu, `local === true` rows only |
| Touches | `clusters.yaml` entry, `selectedCluster`, stored credential, live connection | k3d cluster, `/etc/hosts`, mkcert CA, `~/.memql/src`, Docker volumes |
| Confirmation | modal naming the cluster, stating in one line that nothing is uninstalled | the itemized dry-run preview itself |
| Reversible | yes — re-add it | no |

A single action that asks which one you meant was rejected: it puts the
irreversible operation one click away from a routine one.

### D2 — Remove purges the credential

Deleting the YAML row while leaving the token in `SecretStorage` leaves a
credential for a cluster the operator believes they deleted. Removal covers the
entry, `selectedCluster` if it pointed there, the stored credential, and the
live connection if it was the active one.

### D3 — The `+` opens one page, and the page owns every branch

`+` opens a single "Add a cluster" webview beside the editor. It carries install,
repair, uninstall, **and** remote registration. The remote path becomes a real
form with inline validation, replacing the sequential input boxes, which cannot
be navigated backwards and lose everything on Escape.

The page opens on the machine's actual state, from `ClusterPresence`, with
`addClusterMenu(verdict)` rendered as cards:

| Verdict | Cards |
|---|---|
| `absent` | Install locally (Automatic) · Install locally (Guided) · Connect to a remote cluster |
| `installed-unreachable` | Repair · Uninstall · Connect to a remote cluster |
| `installed-healthy` | Connect to a remote cluster · Uninstall |

`addClusterMenu` gains the uninstall entries; the install-appears-only-for-absent
property is preserved and stays under its existing test.

### D4 — view-kit, per §2.1

The panel renders view-kit output. Step progress is `renderChecklist`. This
supersedes §4.5 of the prior design on the renderer, and on nothing else — the
"no front end decides anything" property it establishes is retained in full.

### D5 — Enablement asks two questions, in order

`actionsHtml()` asks connection state first, role second:

| State | Buttons | Primary control |
|---|---|---|
| `connected` | enabled, subject to role as today | — |
| `idle` / `failed` | disabled, tooltip gives the reason | **Connect** |
| `credential` | disabled, tooltip gives the reason | **Sign in** |
| `failed` and `local === true` | disabled | **Repair local cluster** → opens the page |
| role insufficient | absent, as today | — |

Role-hiding and state-disabling stay distinct. A control the caller may never
use disappears; a control that is momentarily unusable greys out and names the
one thing to click. Neither is a gate — the engine remains the authority, as
`src/deploy/actions.ts` states at length.

### D6 — Uninstall reverses a receipt, never a guess

Nothing absent from the receipt is touched. D8's two-tier model from the prior
design holds: an artifact that predated the install — a Docker the operator
already had, a checkout they keep — is reported as **preserved**, which
`src/install/executor.ts` already models as its own status rather than a flavour
of failure. The dry-run preview *is* the confirmation, itemized before anything
runs.

### D7 — One run path, two front ends

The orchestration currently inside `src/install/cli.ts` moves to
`src/install/session.ts`, exposing `runInstall`, `runUninstall`, and
`previewUninstall`, each emitting per-step events. `cli.ts` becomes a thin argv
adapter over it. The CLI and the webview then execute identical code; a
divergence between "it worked from the terminal" and "it worked from the editor"
becomes impossible to introduce.

### D8 — Presence is invalidated by every mutation

`presence.ts` documents `invalidate()` as mandatory after an install or
uninstall. The page is that caller, and the rule extends to repair and remove:
any completed operation that changes the answer drops the memo before the tree
refreshes.

---

## 4. Architecture

```
   Clusters tree ──select──▶ Cluster panel ──actions──▶ DeployControl (engine)
        │                         │
        │ +                       └── enablement ◀── clusters/status.ts verdict
        ▼
   Add-a-cluster page (webview)
        │  renders                        collects
        ├── view-kit (renderChecklist, cards, forms)
        └── state/addCluster.ts  ◀── events ── install/session.ts
                                                    │
                              graph · executor · receipt   (shipped, #3357)
```

| Unit | Responsibility | New? |
|---|---|---|
| `src/install/session.ts` | `runInstall` / `runUninstall` / `previewUninstall`; emits step events | new |
| `src/install/cli.ts` | argv adapter over `session.ts` | rewritten |
| `src/state/addCluster.ts` | wizard state machine: screen, inputs, validation, folded step progress | new |
| `src/webview/addClusterPanel.ts` | webview adapter: view-kit HTML out, `postMessage` in | new |
| `src/clusters/registry.ts` | remove an entry, fix `selectedCluster`, purge the credential | new |
| `src/clusters/presence.ts` | `addClusterMenu` gains uninstall entries | edited |
| `src/views/clustersTree.ts` | emit `memqlLocalCluster` vs `memqlCluster` | edited |
| `src/webview/clusterPanel.ts` | enablement takes connection state | edited |
| `src/extension.ts` | register the new commands; **delete** `launchLocalClusterInstaller` | edited |

**The boundary that makes this testable** is the one the repo already enforces:
`cmd/memql-lsp/vscodeimportrule_test.go` forbids `vscode` imports under
`src/state/`, `src/clusters/`, `src/deploy/`, and `src/install/`. Every decision
in this design lands on that side of the line; the webview and command
registrations are adapter wiring only.

---

## 5. Failure handling

Per-step failure is presented with the capability contract's exit-code taxonomy,
unchanged from the prior design:

| Exit | Meaning | Presentation |
|---|---|---|
| 2 | bad param | internal error; report with context |
| 3 | refused | explain the refusal |
| 4 | prerequisite missing | render as a guided instruction |
| 5 | operation failed | show stderr, offer retry |

Two actions are always offered: **Retry** and **Switch this step to Guided**.
Stderr appears verbatim in a disclosure. Each step carries a timeout. **Cancel
leaves a valid receipt**, so a cancelled install remains fully uninstallable.

---

## 6. Testing

State modules run under bare `node --test` with no jsdom, which is what keeps
them testable at all. Beyond the ordinary coverage, four assertions carry weight
because each pins a way this feature lies while appearing to work:

- **A verify predicate that always returns true must fail a test.** Verify is
  the advance mechanism; one that cannot fail makes the whole wizard green
  without doing anything.
- **Removing a cluster must be proven to purge its credential**, not merely to
  drop the YAML row (D2).
- **Disabled-action rendering is asserted per verdict.** "It renders enabled" is
  the bug being fixed, so the fix needs a test that fails against today's code.
- **`addClusterMenu` still offers install for `absent` and nothing else**, after
  gaining the uninstall entries.

The Extension Development Host lane covers what unit tests structurally cannot:
that the workbench actually opens the panel, and that the tree's context menus
carry the entries their `when` clauses claim.

---

## 7. Delivery

Four tracks. Tracks 1 and 2 are independent of each other and of track 3, and
land first, so the daily-use failures are fixed without waiting on the wizard.

**Every affordance that opens the page ships with the page.** Two of them look
like they belong elsewhere — the tree's uninstall menu entry (a track-1-shaped
tree change) and the panel's *Repair local cluster* control (a track-2-shaped
panel change) — but neither can exist before its target does. Assigning them to
track 3 is what keeps the dependency graph acyclic. Tracks 1 and 2 deliver what
those affordances will hang off: the `contextValue` split, and the enablement
rule with its `Connect` and `Sign in` controls.

| Track | Contents | Depends on |
|---|---|---|
| **1 — Tree hardening** | remove command + credential purge; local/remote `contextValue`; confirmation wording | — |
| **2 — Panel enablement** | state-aware actions; `Connect` and `Sign in` primary controls | — |
| **3 — The page** | `session.ts` extraction; wizard state; panel shell; landing cards; install/repair run with progress and failure taxonomy; remote form; uninstall preview and run; the tree's uninstall entry; the panel's Repair control | 1 (`contextValue`); 2 (the enablement rule the Repair control slots into) |
| **4 — Close-out** | delete the stub and its "not wired up yet" message; docs; EDH smoke coverage | 1, 2, 3 |

---

## 8. Out of scope

Named so the boundary is explicit. Each remains where the prior design put it.

- **macOS and Windows.** Linux only, matching Epic #3357. Windows is refused
  with a clear message.
- **The Cockpit TUI front end.** Phase 4 of the prior design; a separate surface
  over the same graph.
- **In-engine execution of the graph.** Phase 5; unchanged by this work.
- **Local-model support and published install bundles.** Both already out of
  scope in the prior design (§9), and nothing here alters that.
- **Remote-cluster credential flows.** Sign-in, device code, and token lifecycle
  shipped under epic #3401 and are consumed, not modified.

---

## 9. References

**Code**

- `editors/vscode/src/clusters/presence.ts` — verdict, evidence, `addClusterMenu`
- `editors/vscode/src/clusters/status.ts` — the four row verdicts
- `editors/vscode/src/views/clustersTree.ts` — rows and context values
- `editors/vscode/src/webview/clusterPanel.ts` — `actionsHtml()`
- `editors/vscode/src/deploy/actions.ts` — the engine-is-the-authority note
- `editors/vscode/src/install/{cli,executor,graph,runner,receipt}.ts` — the substrate
- `sdk/ts-viewkit/` — the renderer, and `renderChecklist`
- `cmd/memql-lsp/vscodeimportrule_test.go` — the boundary

**Documents**

- `docs/superpowers/specs/2026-08-08-local-cluster-install-wizard-design.md`
- `docs/superpowers/specs/2026-08-09-signin-without-email-design.md`
- `docs/internal/design/capability-script-contract.md`

**Issues**

- #3357 — install/uninstall substrate (closed; this builds on it)
- #3412 — the `+` presence branch (closed; this replaces its stub)
- #3401 — signing in without email (the credential flows consumed here)
