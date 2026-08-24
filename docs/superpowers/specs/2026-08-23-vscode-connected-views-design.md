# VS Code extension: connection-gated views, and deployments as the selected cluster's timeline

- **Date:** 2026-08-23
- **Status:** approved (autonomous run: recommendations selected by Claude at the owner's standing instruction)
- **Sub-project L** of the 2026-08-23 backlog brief (VS Code + install/release batch)
- **Owner ask:** "I'm not sure why we have an item called 'local' in Deployments
  that expands into the actual list; 'local' should not display anything if
  there is no cluster selected and connected; if connected it should display
  the list of deployments without that 'local' item; selecting a deployment
  should display data and details, with any actions available through buttons.
  When no cluster is selected, all the other sections should display a similar,
  consistent message that varies a bit per view -- like Constructs' 'Not
  connected' -- except Clusters; once a cluster is selected the rest of the
  panels populate."

## Current state (verified 2026-08-23)

- `src/views/deploymentsTree.ts` renders **instances at the top, runs beneath**
  -- and rule 2 of its own header (refs #3737, #3733) REQUIRES the `local` row
  to render even on a machine with no local cluster, as "not installed",
  carrying Create deployment. That rule is what this epic reverses; the entry
  point it protects moves (D4 below), it does not vanish.
- Run rows (`runItem`, `deploymentsTree.ts:202-210`) carry **no command** --
  clicking a deployment does nothing. Instance rows open
  `memql.deployments.open` (the instance page, #3739/#3740).
- Disconnected rendering is inconsistent per view:
  - Constructs returns a synthetic `"Not connected"` TreeItem
    (`constructsTree.ts:222`).
  - Data lists whatever `listConcepts()` returns or an error row.
  - Deployments always shows every registered instance regardless of selection.
  - Runs lists workspace run-config file entries (a local file, not cluster
    data).
  - Clusters has the only `viewsWelcome` (shown only when zero clusters exist).
- No `setContext` calls exist anywhere in the extension -- there are no context
  keys describing connection state, so `viewsWelcome`/menu `when` clauses have
  nothing to key on.
- The deployment panel already renders rich instance pages:
  `renderInstanceOverview`, `renderRemoteInstance`, `renderChooseTag`
  (`src/webview/deploymentScreens.ts`), with role-gated actions from
  `src/deploy/instanceActions.ts` (local: create/repair/uninstall/
  rebuild-from-checkout/upgrade; remote: deploy-control actions mirroring
  engine tiers). Deployment history state lives in
  `src/state/deploymentHistory.ts` / `deploymentsCatalog.ts`, unit-tested under
  bare `node --test`.

## Decisions

### D1 -- Two context keys, one source of truth

The connection manager (`src/connection/manager.ts`) becomes the single
publisher of two context keys via `vscode.commands.executeCommand("setContext", ...)`:

- `memql.clusterSelected` -- a cluster is the current selection (registry has a
  current entry).
- `memql.connected` -- the selected cluster's transport is currently up.

Every provider and every `viewsWelcome`/menu `when` clause reads these; no view
derives its own answer. Set on activation, on select/disconnect/sign-out, and
on transport state changes. A pure helper (`src/state/connectionContext.ts`,
vscode-free) maps manager state -> key values so the mapping is testable.

### D2 -- Consistent disconnected welcome, per view, via `viewsWelcome`

Cluster-backed views return `[]` from `getChildren` when
`!memql.clusterSelected`, so VS Code shows their welcome content (welcomes only
render over an empty tree -- this is why Constructs' synthetic row must go).
`package.json` gains `viewsWelcome` entries, one sentence each, same shape:

- Deployments (`when: !memql.clusterSelected`):
  "Not connected. Select a cluster to see its deployments.\n
  [Select Cluster](command:memql.clusters.select)\n
  [Install a local cluster](command:memql.deployments.createDeployment)"
- Constructs: "Not connected. Select a cluster to browse its constructs.\n[Select Cluster](...)"
- Data: "Not connected. Select a cluster to browse its data.\n[Select Cluster](...)"
- Clusters keeps its existing no-clusters welcome unchanged.

The synthetic `"Not connected"` row in `constructsTree.ts` is removed in favor
of the welcome. Selected-but-unreachable is NOT the empty state: views render
their normal shape and the row-level error/`unreachable` affordances carry it
(Deployments' view description says `unreachable`, Constructs keeps its
version-mismatch/failed-read rows). Empty-tree welcomes describe "nothing to
show", and "the cluster is down" is a fact about something.

**The Runs exception, stated:** Runs lists `runs.json` entries from the
WORKSPACE -- user-authored local files, not cluster data. Hiding a user's own
configurations because a cluster is not selected would read as data loss. Runs
therefore keeps its listing always, and `memql.runs.execute` refuses with the
consistent sentence ("Not connected. Select a cluster first.") when
`!memql.connected`. Its welcome (shown only when the file has no entries) is
unchanged.

### D3 -- Deployments becomes the SELECTED cluster's flat timeline

- Top-level rows are the selected cluster's **deployment runs, newest first**
  (install / upgrade / rollout / rebuild), exactly the rows that today nest
  under an instance. No instance grouping row of any kind.
- The instance-level facts move to the **view description**:
  `treeView.description = "<name> · healthy · v0.19.1"` (and `· update v0.20.0
  available` when the release cache says so -- same `releaseCache` single-flight
  the tree already shares). `TreeView.description` is the API made for this.
- Instance-level ACTIONS stay reachable in two places: the view title menu
  (toolbar `...`) gains the existing commands (Create Deployment, Repair,
  Rebuild From Checkout, Uninstall) gated by `when` on the context keys +
  existing per-instance contextValues; and the instance overview page keeps its
  buttons as today. Nothing loses a route; the wrapper row loses its existence.
- `state/deploymentsCatalog.ts` gains a `runsForSelected(catalog, selection)`
  shaping (pure, `node --test`) rather than the provider filtering inline --
  the render/ownership split the file header documents stays intact.
- The catalog still reads clusters.yaml + receipts for OTHER instances (the
  Clusters view continues to show every registered cluster with health); only
  the Deployments view narrows to the selection.

### D4 -- The install entry point moves, it does not vanish

Rule 2 of `deploymentsTree.ts` existed so a machine with nothing installed had
somewhere to start. That entry point is now: the Clusters welcome (already
carries "Install a local cluster" + "Add an existing cluster"), the Deployments
disconnected welcome (D2 carries the install link), and the view title menu's
Create Deployment. The header comment and rules 1-2 are REWRITTEN to describe
the new model, and refs #3737/#3733 are annotated as superseded by this epic
(sweep the prose that argues the old behavior -- do not leave the argument
standing over the reversed decision).

### D5 -- Selecting a deployment opens its detail page, actions as buttons

- Every run row gets `command: memql.deployments.openRun` with the run + its
  instance as arguments.
- New screen `renderRunDetail` in `deploymentScreens.ts` (pure fragment,
  rendered by the existing deployment panel): what/when/outcome -- kind,
  from -> to versions (or commit for checkout builds, reusing
  `state/imageLane.ts` wording), status with reason, started/finished/duration,
  and the step/log detail where the run record carries it
  (`state/deploymentHistory.ts`).
- **Actions are the EXISTING role-gated set, contextualized -- no new verbs.**
  The detail page shows the buttons from `instanceActions` that make sense from
  that run: a failed local run leads with Repair; a checkout-mode row leads
  with Rebuild From Checkout; a remote rollout surfaces the deploy-control
  actions the caller's role offers (rollback stays owner-only via
  `requiresOwner`, exactly as `deploy/actions.ts` states). The doctrine in
  `instanceActions.ts` ("never offer a button whose only outcome is a
  refusal") applies unchanged.
- Clicking the view description / title opens the instance overview page as
  today (`memql.deployments.open`).

## Testing

- `node --test`: `connectionContext` mapping table; `runsForSelected` shaping
  (selection filtering, newest-first, empty when nothing selected);
  `renderRunDetail` renders every run kind + the no-log case; a gate that
  Deployments/Constructs/Data providers return `[]` when the injected
  connection says no selection (so the welcomes can render).
- `package.json` checks: a test asserting each of the three views carries a
  `viewsWelcome` entry keyed on `!memql.clusterSelected` (string-level, like
  the existing contributes tests if present; otherwise add one).
- Manual checklist: fresh machine (no clusters) -> welcomes everywhere;
  select local -> all views populate; disconnect -> welcomes return; two
  clusters registered -> Deployments follows selection switches.

## Out of scope

- New deployment actions (rollback semantics, re-run) beyond the existing set.
- Changing the Clusters view's own rendering (it is the selector, per the ask).
- The portal's Deployments surface (unchanged).
- Live cluster-side deployment rows (`v1:cluster:deployment`) in the tree --
  the tree stays receipt/history-driven as today; the instance page already
  reads the cluster where it needs to.

## Risks

- `viewsWelcome` only renders over EMPTY trees; any provider that keeps a
  synthetic row by accident suppresses its welcome silently. The provider-
  returns-[] tests exist to catch exactly that.
- Removing the `local` wrapper changes contextValue-driven menus; the `when`
  clauses move from row-scoped to view-scoped and each menu entry must be
  re-verified by hand (part of the manual checklist).

## Task breakdown (preview; tasks carry the acceptance criteria)

1. Context keys + `connectionContext` mapping + manager wiring.
2. `viewsWelcome` entries + providers return `[]` when unselected + Constructs
   synthetic row removal + Runs execute-time refusal wording.
3. Deployments flat timeline: catalog shaping, view description, title-menu
   actions, header-prose sweep (#3737/#3733 superseded).
4. Run detail screen + `openRun` command + contextual action buttons.
