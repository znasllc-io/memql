# The core gate, honest readiness, honest install -- Design

- **Date:** 2026-09-07
- **Status:** approved in the 2026-09-07 brainstorm as epic 1 of the local-first
  inference and routing program (`2026-09-07-local-first-routing-program.md`), the
  first to ship. The owner set the behaviour: an owner or developer is held until the
  core has an inference door, everyone else is told an owner has to set it up, an
  unconfigured app stays configurable from inside the app, and an uninstall that keeps
  a cluster says so. D1-D9 are rulings recorded for the owner to overturn.
- **Owner areas:** `clients/os` (the gate surface, the wizard's placement, the Ask
  sheet), `component/memql` (the inference readiness evaluator and its triggers),
  `integrations/email` (the status probe's timing), `editors/vscode` and
  `scripts/install` (the uninstall), `docs/public/operate`.
- **Depends on:** nothing. Epic 2 of the program is convenient, not required.

---

## 1. Problem

The first-run wizard shipped on 2026-09-07 (epic memql#5106) as a desk widget that the
shell places while seeding a person's desk. In production the seed runs in a React state
initializer before the person's role has resolved, the owner-or-developer gate fails
closed against an empty role ladder, and the widget is never placed. An existing desk
never gains a new widget, because the graph-backed store adopts the stored row and
replaces the seeded surfaces wholesale. And the wizard was designed to gate nothing. So
on the owner's own cluster, with inference unconfigured on every node, MemQL OS opened
every app as though the platform were ready, and the Ask sheet answered a question with
the raw engine error "no streaming provider available".

The readiness feed underneath is right about that cluster and has four defects that
matter the moment it drives a gate: the inference verdict is a live fact frozen in a
row that is rewritten only at boot, on a providers reload, on an email configure or by a
manual recompute; only agent binaries hold the fleet and app seams, so on any multi-node
cluster the fold turns the fleet door into "partial" forever; a manual recompute
evaluates inference as the calling user and writes their personal fleet as a node fact;
and the email module reports "not applicable" on every node because the rows are written
before the lazy mail sender has resolved.

The install path made the symptom look like a reinstall. The VS Code installer adopts a
k3d cluster that already exists, the uninstall refuses to delete a cluster it did not
create, that refusal is classed `preserved` and counts as ok, the done screen prints
"Removed. Everything the install put on this machine has been taken back", the receipt
is deleted, and presence then reads "absent" because it never asks k3d. Install is
offered again and adopts the same database. The operator page says the cluster is
"removed on uninstall: yes, always".

## 2. What the tree already has

- **The setup widget** under `clients/os/src/apps/setup/`: manifest (roles owner or
  developer, no `requires`), `SetupGate` (draws nothing while the feed is unloaded,
  retires after `EXIT_HOLD_MS` once every core stop is settled), `stops.ts` (the pure
  mapping from verdicts and the passkey read to stops, with the passkey-first law),
  `InferenceStop` (three doors), `returns.ts` and `SetupReturnDispatcher` (the parked
  return marker). All of it works once the card is on a desk.
- **Placement** is `seedDocument` in `clients/os/src/chrome/state.tsx`, called from the
  state initializer with the role the shell holds at that instant, which in production
  is `""` (`Shell.tsx` passes `access?.clusterRole ?? ""`; `App.tsx` renders the shell
  without an access prop). The file's own header says a desk "seeded before the ladder
  lands carries no wizard".
- **Retire** is `actions.removeWidget`, a persisted delete of the desk item. `addDesk`
  seeds nothing, so "it comes back on a fresh desk" (`docs/public/operate/first-run.md`)
  is not implemented.
- **The session facts** (`clients/os/src/chrome/access.tsx`): `access`, `ladderLoaded`
  (absent means not loaded, fail-closed) and `readiness` (absent means not loaded,
  deliberately fail-open). `SessionScope` in `Shell.tsx` retains one live collection over
  `v1:platform:moduleReadiness` and one over `v1:cluster:node`, folds them with the
  mirrored `foldReadiness`, and `loaded` is true only when both are past seeding.
- **Per-window gating** exists: `WindowFrame` and `PhoneShell` render `SurfaceUnconfigured`
  in place of an app body when a section's `requires` is unmet, Settings and Logs
  exempt; the sentence for everyone but owners and developers is "An owner or developer
  can set it up in Settings." (`clients/os/src/kit/ReadinessStates.tsx`). Nine apps
  render a `SetupGroup` at the top of their Settings section.
- **The Ask widget** gates on the `ai` module; **the Ask sheet** does not, and renders the
  engine's error verbatim (`clients/os/src/ask/AskSurface.tsx`).
- **The readiness model**: modules declared in `component/envregistry/manifest.yaml`
  (`ai`, `storage`, `email` are `core: true`), evaluated per node by
  `component/memql/readiness_eval.go`, written by `readiness_write.go` as one row per
  `(module, nodeId)` at a deterministic id, folded by the leaf package
  `component/memql/readiness` (worst state wins, disagreement is `partial`, no live row
  is `unreported`), broadcast by `component/node/routing.go`, readable by any signed-in
  person (`@rowAuthz(public, requiresIdentity)`).
- **The `ai` evaluator** is `InferenceOpen`, which is `len(e.inferenceDoors(ctx).Doors) > 0`
  (`component/memql/fleet_catalog_read.go`): a local door needs an online machine
  offering a model with structured output and at least `MinimumContextWindow` = 8192; an
  app door needs an online stream holding a known, allowed, signed-in app; federation is
  structural presence of the four identity ids. `SetFleetInference` and
  `SetAppInference` are agent-tagged (`app/cluster_worker.go`,
  `app/integrations_worker_agent.go`), so every other node type answers "no local or app
  door". `evaluateModules` receives the raw caller context, so an owner's manual
  `readinessRecompute` resolves the owner's own machines.
- **The email evaluator** asks `integration.email.status` in-process; a probe error or an
  unregistered integration is `notApplicable` (`readiness_eval.go`). The boot write in
  `app/run.go` runs after `waitForReady`; the lazy sender's own resolution
  (`integrations/email/lazy.go`) logged twenty seconds later on the owner's cluster.
- **The installer** is `editors/vscode/src/install/*` driving `scripts/install/*.sh`
  through the graphs in `scripts/install/graph/`. `remove-artifact.sh` refuses
  `--pre-existing=true` with exit 3 for every kind; `executor.ts` classes that refusal
  `preserved`, and `ExecutionReport.ok` is "true when no step failed. Skips and
  preservations are not failures"; `addClusterPanel.ts` calls `completeUninstall` on
  `ok`; `installScreens.ts` prints the removed sentence unconditionally;
  `clusters/presence.ts` decides `absent | installed-healthy | installed-unreachable`
  from the receipt, the registry row and an endpoint probe. `resolvePreExisting` fails
  toward `true`. The receipt on the owner's machine records `clusterUp preExisting: true`.
- **`make up-refresh`** is the only data wipe, documented in
  `docs/public/operate/reproduce-the-cloud-locally.md`. The capability-script contract
  reserves `--confirm=<phrase>` for a destructive act.

## 3. Decisions

### D1 -- The gate is an OS surface before the desk, computed from the readiness feed, never an engine refusal

Chosen over an engine-side gate (refusing streams for non-owners while the core is
unconfigured) and over keeping the offer-only widget. The engine's per-call refusals are
already the enforcement; refusing the stream would block the very reads the wizard needs
and would take away from a viewer the feed that tells them to ask an owner. The
readiness rows are readable by every signed-in person and writable only by the engine
(configuration-readiness record, section 6), which is exactly what lets a client-side
gate be honest for every role. `CoreGate` mounts in `Shell.tsx` between `SessionScope`
and `ShellRoster`, renders in place of the desk, and is re-evaluated on every boot and
on every feed change. Nothing is dismissed and nothing is remembered in a browser: the
verdict is the state.

### D2 -- The gate keys on the inference module only; the wizard still walks every core stop

Chosen over holding the OS until every core module is configured. Storage is always
configured on a local cluster and mail is log-only there by design (memql#4477), and
neither is needed to use the OS. The gate holds while the `ai` verdict is
`unconfigured`. The other core stops render in the same rail, unlit, non-blocking, and
stay with the setup widget after the gate lifts (D4).

### D3 -- Inference readiness is configuration, evaluated from rows, identically on every node, under the maintenance actor

Chosen over presence (the shipped evaluator), over hosting the module on agent nodes
only, and over a cluster-wide health verdict. A door is **configured** when the rows say
it exists: an unrevoked `v1:worker:registration` carrying a `model:<id>` label whose
attributes meet the floor (structured output, context at least `MinimumContextWindow`),
an unrevoked registration whose `apps` carry a known, allowed, signed-in app, or a
federated provider whose identity ids are present. Whether the door is **open right now**
is a second fact carried on the same row: each door is one lane, `complete` is
configured, and its single slot `live` says whether the machine was seen within the
online window, the app stream is held by some replica, or the vendor entry is available.
The gate and the wizard read `complete`; the park and the Readiness signpost keep
reading `inferenceStatus`, which stays the live, per-caller answer.

Rows are what every node can read, so the evaluator no longer touches the fleet or app
seams and every node type answers the same verdict: the fold's disagreement rule stops
firing for `ai`, and the `hostedBy` mechanism is not needed. The evaluation context is
built inside `WriteModuleReadiness` from the maintenance actor, never from the caller,
so an owner's recompute cannot write their laptop as a cluster fact. The registration
read is cluster-wide (the concept is owner-tiered; the maintenance actor carries
`AccessContext.Unranked` and is what every sweep already uses).

### D4 -- The setup widget's presence on a desk is derived, never seeded

Chosen over fixing the seed's timing (a seed is a one-time act, and the person's role,
the ladder and the feed all land after it) and over a migration that adds the widget to
old desks (a one-time act again, wrong the next time a stop unsettles). An effect in
`OsProvider` runs whenever the ladder, the feed or the active desk changes: if the role
admits the widget and some core stop is unsettled and the active desk has no setup item,
add one under Ask; if every stop is settled and an item exists, retire it through the
existing exit hold. `seedDocument` places Ask alone. The roaming test's seeded roster
becomes `["ask"]`, and a new test asserts the ensure and the retire. "It comes back on a
fresh desk" becomes true without a re-seed: it comes back on any desk the moment a core
stop is unsettled, which is the honest answer to "where did it go".

### D5 -- Readiness recomputes on the events that change it, with one delayed boot re-write

> **AMENDED on 2026-09-14.** The premise that the registration broadcast reaches
> every replica is false: identity is excluded from every broadcast, a node
> nobody dials receives no mesh event, and a peer whose connection is absent at
> that moment is skipped (memql#5259). D2 of
> `2026-09-14-readiness-convergence-design.md` adds a jittered exponential retry
> after a failed or unknown pass and a 10-minute jittered safety-net pass, so
> "not a timer" no longer holds as stated and `TestNothingRewritesWithoutAnEvent`
> became `TestASafetyNetPassRunsWithoutAnEvent`; its S1 sets aside a row whose
> cluster-scoped lanes lag the freshest one, so the verdict does not wait for
> the lagging row. The event subscription, the debounce and the 30 s boot
> re-write stand, and the boot write goes through the same loop.

Chosen over a timer (too slow after a change, wasteful when nothing changed) and over
the four existing triggers alone. A per-node subscriber rewrites the rows, debounced two
seconds, on `graph.node.created|updated|deleted` for `v1:worker:registration` (a
machine paired, revoked, or re-advertising its models and apps), on the existing
providers-reload broadcast (federation set), and on the email configure path. Boot keeps
its write after `waitForReady` and adds one re-write thirty seconds later, which is what
covers a lazily materialized integration on a node whose plug-in resolved late. The
email status probe must answer from configuration presence without materializing a
sender; the implementing session pins the cause of the owner's "not applicable on every
node" with a test that boots a node carrying the email plug-in and asserts the verdict is
never `notApplicable`.

### D6 -- Two role variants, one sentence each

Owner or developer: the rail of core stops from `stops.ts`, the inference stop open with
its three doors (a machine on your fleet first, then Anthropic, then OpenAI, any
combination), the other stops listed unlit, and the passkey-first law kept. Everyone
else: "MemQL is not set up yet. An owner or developer has to set up inference before
anyone can use it." and Sign out. No app, no dock, no launcher behind the gate. The
gate does not name the owner, because a person's name is not the cluster's to publish.

### D7 -- The Ask sheet gates like the Ask widget

The sheet reads the `ai` verdict and, while it is `unconfigured`, renders the module's
sentence and the ask-an-owner line in place of the input. The chat handlers' error
mapping (a typed refusal instead of `codes.Internal`) belongs to epic 2, where the chat
path starts going through the router; this epic only stops a person from reaching it.

### D8 -- An uninstall that keeps a cluster says "kept", keeps its receipt, and presence asks k3d

Chosen over treating `preserved` as a failure (that abandons every removal downstream,
which the executor's comment rightly refuses) and over silently converting a kept cluster
into "removed". `preserved` renders as "Kept: it was here before the install" with the
capability's own reason; the done sentence changes when anything was kept; the receipt
entry for a kept cluster is retained, so presence reads `installed-healthy` and offers
repair, rebuild and uninstall rather than Install; and presence gains a fourth signal,
`k3d cluster list` through the detect capability, so a live cluster named `memql` with
no receipt reads `present-unreceipted` with the sentence "a cluster named memql exists
that this installer did not create" and the acts adopt or delete.

*As built:* the delete needed a path of its own, because both session entry points call
`requireReceipt` and `present-unreceipted` means there is no receipt -- so the card was
offered and the screen behind it read "no receipt at ~/.memql/install-receipt.json".
`SessionOptions.unreceiptedCluster` names the cluster the verdict was MADE from, and
`unreceiptedClusterReceipt` builds a one-entry record for it with `preExisting: true`;
every other uninstall step finds no entry and skips satisfied. Nothing is guessed: the
cluster is the one artifact `k3d cluster list` is direct evidence of, and the general
refusal is unchanged for a caller that names nothing.

Two predicates came out of this, not one. `ExecutionReport.kept` ("is anything still on
this machine?") drives the closing sentence; `keptCluster` ("is the CLUSTER still here?")
decides whether the receipt and the registry row stay. Reading one for both is a bug in
each direction: a preserved checkout with the cluster deleted would strand a receipt
naming a cluster that is gone, which is memql#3544 re-opened from the other side.

### D9 -- The one destructive act is offered, behind a typed phrase

The uninstall form gains "Also delete the cluster and its data", a form checkbox (rule 10
of the interface language permits a checkbox in a form) with a phrase field. When ticked
and the phrase `delete memql data` is typed, `removeCluster` runs with
`--confirm=delete-memql-data`, which `remove-artifact.sh` accepts for `--kind=stack`
only and which overrides the pre-existing refusal for that one kind. (The record said
`--kind=cluster` when this was written; `stack` is the artifact kind the script and the
receipt actually use, and `--cluster` is the flag that names which one.) Nothing else in the
graph accepts a confirm phrase. The install page gains the sentence that a wizard
uninstall plus install is not a data reset unless that box is ticked, and the table row
for the k3d cluster says "only if MemQL created it, or if you tick the data box".

## 4. The change

- `clients/os/src/chrome/CoreGate.tsx`: the surface, both variants, mounted in
  `Shell.tsx`; draws nothing while `readiness.loaded` or `ladderLoaded` is false, so a
  configured cluster never flashes a gate.
- `clients/os/src/apps/setup/stops.ts`: unchanged mapping; `inferenceConfigured` reads
  the lane's `complete`. *As built:* the door NAMES are still the client's own strings,
  not the lanes'. Taking them from the feed would put operator-facing copy in a Go
  constant, where the interface language cannot reach it; the lane `name` stays an
  identifier the client matches on.
- `clients/os/src/chrome/state.tsx`: `seedDocument` places Ask alone; `ensureSetupWidget`
  effect; the retire path reused.
- `clients/os/src/ask/AskSheet.tsx`: the readiness gate.
- `clients/os/src/system/readinessFold.ts` and `component/memql/readiness`: the lane
  shape already carries `complete` and `slots`; the fixture parity tests gain the
  inference lanes.
- `component/memql/readiness_eval.go`: the `ai` arm reads registrations and the provider
  registry, and applies the evaluation actor at the one place a context reaches a
  resolver -- `evaluateModule`, not the caller. *As built:* it began as a line in
  `readiness_write.go`, which meant one caller carried the whole property and reverting
  one assignment left every test green. `readiness_recompute_subscriber.go` (new): the
  debounced event-driven rewrite; `app/run.go`: the delayed re-write, routed through the
  debounce where there is one.
- `integrations/email`: *unchanged.* The record expected the status probe to need work
  here and it did not -- the probe was already a reproduction of the resolution
  algorithm, and the cause of `email` reading notApplicable on every node was the ACTOR,
  not timing. `component/memql/readiness_email_probe_test.go` pins both halves, and
  `TestEmailStatusFloorMatchesTheIntegration` reads the real `statusAuthorized` out of
  its source file rather than trusting the copy beside it.
- `editors/vscode/src/install/executor.ts`, `webview/addClusterPanel.ts`,
  `webview/installScreens.ts`, `clusters/presence.ts`, `clusters/registry.ts`,
  `state/uninstallRun.ts`: kept as a result, the receipt retained, the fourth presence
  signal, the data-wipe form.
- `scripts/install/remove-artifact.sh`, `scripts/install/detect.sh`,
  `scripts/install/graph/uninstall.json`: the confirm phrase for the cluster kind, the
  cluster list in detect's envelope.
- `docs/public/operate/first-run.md`: rewritten to what ships (the gate, the role
  variants, the derived widget). `docs/public/operate/install-prerequisites.md`: the
  table row and the "start over" sentence.

## 5. Failure modes

- The feed loaded with no `ai` row from any live node (`unreported`): the gate draws
  nothing, the shell opens, the Readiness signpost says nobody reported. A broken
  cluster is not the same as an unconfigured one, and the gate must not claim it is.
- The gate lifts on a door that is configured but not live (a laptop asleep): the OS
  opens; a call parks with the router's report naming the machine and why. That is the
  design: configuration opens the OS, presence decides a call.
- A person removes the setup widget by hand while a stop is unsettled: the effect puts it
  back on the next change of the feed or the ladder. The widget is the state.
- The uninstall keeps the cluster and the person wanted it gone: the done screen names
  it as kept, the receipt stays, and the uninstall can be run again with the data box
  ticked.
- The confirm phrase is mistyped: exit 2 from the script, the step fails, nothing is
  removed, the report says so.

## 6. Testing

- `stops.ts` on fixtures: the inference lanes in every combination of `complete` and
  `live`; the gate predicate; the passkey law unchanged.
- `CoreGate`: renders nothing while unknown; the owner and developer rail; the reader
  sentence; lifts on a feed change without a reload; both modes screenshotted.
- The derived widget: seeded roster is `["ask"]`; ensured after the ladder and feed load
  for an owner; absent for a reader; retired when every stop settles; back when one
  unsettles. `test/system/roamingShell.test.tsx` and `test/chrome.test.tsx` re-pointed.
- The Ask sheet gate.
- `readiness_eval.go`: the `ai` arm on fixtures for every door, configured versus live;
  a node of every type produces the same report from the same rows (the multi-node parity
  test); the maintenance-actor context (`readiness_internal_origin_test.go` extended to
  the evaluation, not only the write).
- The subscriber: a registration event rewrites within the debounce; a burst rewrites
  once; the delayed boot re-write fires.
- The email probe: the node-with-plug-in test above.
- The installer: `preserved` renders kept, the report and done sentence, the receipt
  retained, presence with a live unreceipted cluster, the phrase accepted only for the
  cluster kind, a mistyped phrase refused. `scripts/install/remove_artifact_test.go`
  and the executor tests.

## 7. Delivery

Three PRs, one per owner area, each mergeable alone.

- **PR 1, engine.** Task 1: inference readiness from rows, configured and live, under the
  maintenance actor, identical on every node. Task 2: event-driven recompute, the delayed
  boot re-write, the email probe timing.
- **PR 2, OS.** Task 3: the core gate surface and its role variants. Task 4: the derived
  setup widget. Task 5: the Ask sheet gate. Task 6: `first-run.md` rewritten.
- **PR 3, install.** Task 7: kept is a result, the receipt retained, presence asks k3d.
  Task 8: the data wipe behind the phrase, the operator page corrected.

The picking-up session writes the plan from this record with `superpowers:writing-plans`
before the first task and deletes it in the epic's merge (docs/CLAUDE.md).

## 8. Out of scope

- Any engine-side refusal keyed on readiness.
- Gating on storage, mail or the passkey.
- A "skip" or "remind me later": the verdict is the state.
- The chat handlers' error mapping and the Ask surface reaching the fleet (epic 2).
- A wizard-side equivalent of `make up-refresh` beyond the data box (the box is that
  equivalent).

## 9. Facts to re-verify before starting

That `seedDocument` still gates on `actorRole`; that `SetupGate` still retires through
`removeWidget`; the lane and slot shape in `component/memql/readiness`; that
`MinimumContextWindow` is still gate-only; the maintenance actor's constructor in
`component/auth`; that `executor.ts` still reports `preserved` and `ok` ignores it; that
`presence.ts` still never asks k3d; the `--confirm=` convention in
`scripts/lib/capability.sh`. All were read on 2026-09-07 at commit 907e385fb.
