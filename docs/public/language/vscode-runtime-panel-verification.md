---
title: VS Code Runtime Panel -- Manual Verification Checklist
audience: public
status: stable
area: language
sinceVersion: 0.14.0
owner: znas
---

# VS Code Runtime Panel -- Manual Verification Checklist

The artifact a human works through before calling a change to the VS Code
runtime panel done.

## Why this exists even though there is an automated host lane

There are two verification lanes for this extension, and they answer different
questions.

| Lane | Command | Answers |
|---|---|---|
| Unit | `make vscode-test` | Does the logic compute the right answer? Fast, Electron-free, covers only modules that do not import `vscode`. |
| Host smoke | `make vscode-test-host` | Does the extension survive a real VS Code? Activation, command registration, the activity-bar contributions, the host runtime's WebSocket story, watching a path outside the workspace, webview creation. Runs against both the declared `engines.vscode` floor and current stable. |
| This checklist | A human, `F5` | Does it *work*, and does it *look right*, against a live cluster? |

The host smoke lane (memql#3302) exists because a whole class of defect passes
every unit test and fails only in a host -- an unguarded global dereference on
a runtime that lacks it, a file watcher that silently never fires. It caught
three instances of that class. What it deliberately does **not** do is dial a
cluster: it never connects, so everything downstream of a connection -- real
rows, a real run, a real deployment -- is unverified until someone runs it.
That is this document's job, and it is the reason the list below is longer than
the smoke lane, not shorter.

## Setup

```bash
make vscode-deps                              # build the file: workspace deps
cd editors/vscode && npm ci && npm run compile
```

Then open `editors/vscode` in VS Code and press **F5**. In the Extension
Development Host, open a folder containing `.memql` files (the repo's `dsl/`
tree is the obvious choice) and set `memql.lsp.serverPath` to a built
`memql-lsp`, or run `make vscode-install` first so a platform binary is
bundled.

A live cluster is required from section 2 onward. `make up` brings one up
locally; add it to `~/.memql/clusters.yaml` with an endpoint and a PAT minted
at the identity binary's `/me/tokens`.

Record the versions you verified against -- "it worked" means little without
them:

- [ ] VS Code version: ____________
- [ ] Extension commit: ____________
- [ ] Cluster: ____________

---

## 1. Panel basics (B1)

- [ ] The memQL icon appears in the activity bar and reads cleanly at 24x24
- [ ] Clusters lists the local cluster from `~/.memql/clusters.yaml`
- [ ] Selecting it connects, and the icon turns to a filled green circle
- [ ] Concepts lists domains, and expanding one lists its concepts
- [ ] Clicking a concept opens a tab, rows render, and **Load more** pages
      correctly
- [ ] Clicking a row shows its full nested detail -- payload, provenance and
      intrinsics, unflattened
- [ ] Inserting a row elsewhere (the Cockpit, or `psql`) updates the list with
      no manual refresh
- [ ] Editing `~/.memql/clusters.yaml` externally refreshes the Clusters tree
- [ ] In an **untrusted** workspace: language features still work, and the
      runtime views do not appear

WARNING: the external-edit item is worth doing deliberately rather than
skimming. It is the item that was broken twice, both times silently, and both
times while every automated test was green.

## 2. Cluster registry editing (B1)

- [ ] **Add Cluster** collects name, domain, endpoint and PAT, and the new
      cluster appears in the tree
- [ ] Adding a cluster whose name collides with an existing one is refused,
      not silently turned into an edit
- [ ] **Edit Cluster** with the name field changed renames the existing entry
      rather than appending a second one
- [ ] Comments and unknown fields already in `clusters.yaml` survive a write
      (the Cockpit shares this file)
- [ ] A cluster with no endpoint or no credential shows the yellow "not
      configured" state, and a failed connection shows the red error icon with
      the message on hover
- [ ] **Disconnect** returns the icon to the hollow circle and empties Concepts

## 3. Running a construct (B2, memql#3309)

Open a `.memql` file with a runnable construct (a query is the easiest).

- [ ] A **Run** CodeLens renders above the construct's signature, and a
      **Run with...** lens beside it
- [ ] The lens tooltip names what will actually run
- [ ] Nothing runs on open and nothing runs on save -- the lens is an
      affordance, and only a click fires it
- [ ] **Run** on a no-argument construct executes and opens a result tab
- [ ] Result rows render through each concept's own `@displayCard`, and a
      concept with none falls back to the row id
- [ ] Clicking a result row opens it in the Concepts surface
- [ ] **Run with...** opens the argument form, with a field per declared arg
      and the declared types enforced
- [ ] A required arg left blank is reported in the form rather than sent
- [ ] Running a **mutation** raises a modal confirmation, and dismissing it
      cancels the run
- [ ] Editing the construct in the buffer and re-running runs the EDITED
      definition, not the deployed one (the result banner says which)
- [ ] Disconnecting and reconnecting, then re-running, still runs the edited
      definition rather than silently falling back to the deployed one

### Diagnostics mapping

- [ ] Introduce a syntax error in the construct, then run: the engine's
      diagnostics land in the **Problems** panel against the right file and the
      right line
- [ ] A diagnostic the engine could not position lands as a file-level problem
      on the active file, not parked on line 1 of some unrelated dependency
- [ ] Fixing the error and re-running clears them
- [ ] Typing in the buffer does NOT clear a run's diagnostics (they are a
      separate collection from the language server's)

### Saved run configurations

- [ ] Saving a named configuration from the arg form writes
      `.memql/runs.json` in the workspace
- [ ] The **Runs** view lists it
- [ ] Its inline play button re-runs it with the saved arguments
- [ ] Its inline delete button removes it, from the view and from the file
- [ ] **Open Run Configurations File** opens `.memql/runs.json`
- [ ] Editing that file by hand and hitting Refresh shows the change
- [ ] Re-running a saved configuration whose construct no longer exists fails
      with a legible message rather than a stack trace

## 4. Running an automation (B3, memql#3310)

Open a file containing an `automation`.

- [ ] A **Run automation...** CodeLens renders above it
- [ ] The form opens on the mode the trigger implies: `schedule` for a
      `@trigger(schedule=...)`, `row` for a concept-triggered one, `json`
      otherwise
- [ ] The form states in one sentence what the run will fire
- [ ] In **row** mode the picker lists real rows of the trigger concept, and
      **Load more** pages
- [ ] Picking a row and running fires the automation against it
- [ ] In **json** mode a malformed payload is reported in the form rather than
      sent
- [ ] An automation whose trigger names no concept does not offer the row
      picker at all

### The step trace

- [ ] Running opens the trace tab beside the form without stealing focus
- [ ] Steps appear as they complete, ordered by sequence, with per-step timing
- [ ] A failing automation shows the failing step, and the timeline stays
      intact
- [ ] A REFUSED run (unknown name, `@disabled`, a `@filter` miss, wrong role)
      reads as a refusal -- "it never started" -- and not as a failed run
- [ ] The raw toggle shows the underlying frames
- [ ] Toggling raw mid-run is not undone by the next step landing
- [ ] Saving the automation run as a configuration, then re-running it from the
      Runs view, refills the form and fires the same payload

## 5. The Cluster tab (B4, memql#3312)

Open it from the Clusters tree's inline action, or from the palette
(**memQL: Open Cluster Topology and Deployments**).

- [ ] The tab opens against the selected cluster and is titled with its name
- [ ] **Topology** shows one tile per registered node: label, short id,
      advertised address, version, deployment, health
- [ ] The replica tally shows one row per node type, and a short tier is
      flagged as under-replica
- [ ] A tier with no declared replica count reads "N running", never "N of 0"
- [ ] An orphaned node is marked as such, with its reason
- [ ] **Deployment history** lists deployments newest first, with status and
      shortened digests
- [ ] Selecting a deployment shows its per-node-type specs
- [ ] The view updates live as a deployment progresses -- no manual refresh
- [ ] **Actions** shows only what your role permits (Cut version / Deploy /
      Promote / Roll back / Rollout promote-abort), and the hidden ones say why
- [ ] A destructive action requires typing its confirmation phrase, and a
      mismatch refuses
- [ ] Disconnecting shows the "not connected" state and the live-updates-offline
      notice rather than stale data
- [ ] Switching to a different cluster repaints the tab from the new cluster
      rather than leaving the old one's data on screen

## 6. Cross-cutting

- [ ] Open every tab type at once, then **Developer: Reload Window**: the
      extension comes back clean with no errors in the Extension Host log
- [ ] Close every tab: no "disposed" errors in the log
- [ ] Switch clusters with several tabs open: every tab repaints against the
      new cluster, and none shows the previous cluster's rows
- [ ] Stop the cluster mid-run: the failure is reported, and the panel does not
      wedge
- [ ] Nothing anywhere renders a PAT -- not a tab, not a tooltip, not the log

## When something on this list fails

If the failure is of the host-only class -- nothing throws in the unit tests,
the feature is just silently dead -- consider whether the automated host lane
could have caught it, and add the case if so. That lane lives in
`editors/vscode/test-host/`; the three defects it already guards against are
documented in its header, and each of them was found the same way: a human
working through a list like this one.
