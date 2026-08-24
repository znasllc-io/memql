# The deployment run experience: actions first, a branded progress bar, logs behind a disclosure, and content written for a product

- **Date:** 2026-08-24
- **Status:** approved (autonomous run: recommendations selected by Claude at the owner's standing instruction)
- **Sub-project P** of the 2026-08-23/24 backlog brief (VS Code + install/release batch)
- **Owner ask:** "When I select a deployment, the actionable buttons should be
  at the top rather than at the bottom -- the user wants the actions first,
  then the details and the logs. There's a lot of extra information exposed
  that feels like troubleshooting placeholders rather than logs for a product
  that ships -- even for a developer audience the content needs to be
  different. And when installing/uninstalling/repairing, instead of a bunch of
  logs and a checkmark list, show the MemQL logo with a progress bar
  underneath -- following the app's look and feel -- with minor messages about
  what's happening; the actual logs hidden unless the user chooses to see
  them, in a scrollable view. This applies to deployments too."

## Current state (verified 2026-08-24)

- **Actions render LAST on every screen.** The run screen
  (`editors/vscode/src/webview/installScreens.ts`, `renderRunningScreen`)
  emits heading -> lede -> step checklist -> `<div class="actions">` at the
  bottom; the failure screen and the collect/confirm screens end the same way;
  `deploymentScreens.ts`'s instance overview places its action row after the
  facts. Nothing about this order is load-bearing -- it is accretion.
- **The run display IS the step checklist.** view-kit's `renderInstallSteps`
  over `state/installProgress.ts`'s projection: every step, every state, with
  per-step output accumulating in `StepProgress.log` -- which is "everything
  the script wrote, verbatim" (`state/addCluster.ts:63`), i.e. capability
  stderr. The failure screen leads with `failureGuidance(exitCode, remedy,
  reason || log)` and a `<pre class="remedy">` block.
- **The brand layer already has the pieces:** `brandTokens.ts` ships the
  palette, `brandMarkSvg(size)` (the 9-node mark, inline, currentColor), the
  `brand-head`/`panel-box`/`badge` component classes, and every panel inlines
  it under its CSP nonce. There is no progress-bar component and no
  collapsed-log component yet.
- **Display redaction exists and must survive:** `redactForDisplay` /
  `maskHomePath` (`src/install/secrets.ts`, memql#4194) is the one redactor
  for human surfaces; step logs pass through it on their way to any pane.
- **Webviews re-render HTML wholesale on every event** (the state modules
  document this), so any open/closed or scroll state that lives in the DOM
  dies on the next `stepLog` event -- roughly once a second during a run.
- Sibling epics touching the same surfaces: #4423 (connection-gated views;
  its #4427 adds the run DETAIL page), #4428 (onboarding forms). This epic
  sets the LAYOUT AND CONTENT DOCTRINE those screens render under; it does
  not change what any screen is FOR.

## Decisions

### D1 -- Actions first: one ordering doctrine for every deployment surface

Every screen in the add-cluster panel and the deployment panel renders in this
order, stated once and enforced by tests rather than convention:

1. **Brand header** (`brandStrip`/`brandHeader` -- already present).
2. **Actions** -- the primary/secondary buttons for THIS screen (Start /
   Cancel / Retry / Back; Repair / Uninstall / Rebuild / deploy-control on the
   deployment pages). Directly under the header, always visible without
   scrolling.
3. **Status** -- the progress area (D2) or the screen's headline facts.
4. **Details** -- forms, fact rows, step specifics.
5. **Logs / diagnostics** -- last, collapsed (D3).

The failure state does not move actions back down: Retry / Switch-to-guided
render in the actions row at the top, the failure summary renders in the
status area, and the failed step's output discloses in the log area (D3).
The existing doctrine "never offer a button whose only outcome is a refusal"
(`instanceActions.ts`) is unchanged -- this decides WHERE buttons go, never
WHICH buttons exist.

### D2 -- The branded run: logo, progress bar, and a one-line narration

`renderRunningScreen` (install / repair / uninstall / rebuild -- all four
modes) is rebuilt around a single centered run block:

- **The MemQL mark** (`brandMarkSvg`, ~48px, accent-tinted) above a
- **determinate progress bar**: `settled / total` over the step list the
  executor seeds at `runStarted` (`state/addCluster.ts` upserts THE STEPS
  AHEAD precisely so "how much is left" is knowable -- this is the number the
  bar renders). `skipped` and `preserved` count as settled; the bar's fill
  uses `--memql-accent` on a `--memql-raised` track, honors
  `prefers-reduced-motion`, and carries `role="progressbar"` +
  `aria-valuenow`. While total is still unknown (the pre-`runStarted` moment)
  the bar renders indeterminate with the existing "Starting." lede.
- **One minor message beneath**: the CURRENTLY RUNNING step's description --
  the human sentence the graph already carries per step -- plus a muted
  "step N of M". Never raw output, never a script name. Several steps running
  in one wave show the first unsettled step's description ("and 2 more in
  progress").
- The full step checklist MOVES into the details area below the fold as a
  compact list (it remains the truthful record; it stops being the headline).
- Terminal states swap the block's message, not its shape: success ("Installed.
  <next-step sentence>"), cancelled, failed ("<failed step's description>
  failed -- see the log below"), with the actions row above already carrying
  Retry / Back per D1.

The math lives in a pure `runProgress(steps)` function in
`state/installProgress.ts` (settled, total, percent, currentDescriptions),
unit-tested under `node --test` -- waves, skips, retries (a retry RESETS
failed steps to pending, so the bar must be allowed to move backwards),
and the empty pre-seed moment.

### D3 -- Logs exist, behind a disclosure, scrollable, state-held

- A single **"Show logs"** disclosure sits at the bottom of every run and
  detail screen. Collapsed by default. Open, it renders the per-step output
  (redacted via `redactForDisplay`, as today) in a scrollable pane
  (`max-height` ~40vh, `overflow-y: auto`, the `.data` monospace voice).
- **The open/closed flag and the scroll intent live in the STATE module, not
  the DOM** -- the panel re-renders wholesale on every `stepLog` event, so DOM
  state dies once a second. `AddClusterState` (and the deployment panel's
  state) gain `logsOpen` (toggled by a message like every other control) and
  the pane auto-follows the tail while the user has not scrolled up
  (pinned-to-bottom semantics; a scroll-up sets a `follow=false` flag the next
  render respects, a scroll back to the bottom re-arms it).
- **Failure auto-discloses.** When a step fails, `logsOpen` is set true and
  the pane scrolls to that step's output -- an operator staring at a failure
  needs the log NOW, and hiding it behind a click at exactly that moment
  would be design spite. The remedy block (`<pre class="remedy">` +
  guidance) stays in the status/details area, OUTSIDE the disclosure: the
  command that fixes the problem is an action-adjacent fact, not a log line.
- The disclosure line itself carries the honest size ("Show logs -- 214
  lines"), so an empty log renders "No output yet" disabled rather than a
  toggle that opens nothing.

### D4 -- Content curation: written for a product, verbatim only inside the pane

The audit the owner asked for, as enforceable rules rather than taste:

- **Step descriptions are the narration** and must read as product sentences:
  what is happening in the operator's terms ("Checking that Docker can run
  containers", "Writing the identity bootstrap values"), never which script
  runs, never internal vocabulary (capability, envelope, graph, wave,
  seedBootstrap). The install graph's `description` fields
  (`scripts/install/graph/install.json`, `uninstall.json`, `rebuild.json`)
  are PASSED through today -- this task rewrites them once, in place, to that
  standard (they are already the single source both the wizard and the CLI
  render).
- **Verbatim subprocess output appears in exactly one place: inside the D3
  pane.** No screen renders `StepProgress.log`, exit codes, or envelope
  fields into its status or details areas. The failure summary renders the
  step's DESCRIPTION + the capability's `reason` sentence (capability
  scripts' reasons are already written for humans by contract) + the remedy;
  the raw output is one disclosure away.
- **Facts pages drop the troubleshooting tier into a "Diagnostics"
  disclosure.** The instance overview keeps: name, health, version/commit,
  lane, domain, last run, update availability. Receipt paths, CA locations,
  raw timestamps, node-by-node minutiae move under a collapsed Diagnostics
  section at the bottom (same component as D3). Nothing is deleted --
  demoted, so the support case still has it.
- **The sweep is recorded.** The task's PR lists every string it rewrote or
  demoted (before -> after), so the owner can veto specific rewrites cheaply.
- Redaction is untouched and re-asserted: the D3 pane and the Diagnostics
  section both pass through `redactForDisplay`; the existing secrets tests
  extend to the new components.

### D5 -- The deployments panel follows the same doctrine

`renderInstanceOverview` / `renderRemoteInstance` reorder to D1 (actions
under the header). The run-detail page that #4427 (epic #4423) introduces is
BUILT on this epic's components -- actions row on top, status facts, then the
D3 log disclosure for whatever output the run record carries -- rather than
inventing its own layout. Coordination note: whichever of #4427 / this
epic's task 2 lands second rebases onto the shared components; the doctrine
here wins on layout questions, #4427 wins on what the run detail CONTAINS.

## Testing

- `node --test`: `runProgress` table (waves, skips, retry-moves-backwards,
  pre-seed); `logsOpen`/follow-tail state transitions incl. failure
  auto-disclose; ordering tests asserting rendered HTML has the actions row
  before the status block on every screen (string-position assertions per
  screen, the cheapest honest gate); the disclosure renders redacted output
  only; empty-log disabled state.
- Graph-description gate: a test over the three graph JSONs failing on the
  banned internal vocabulary in `description` fields (the word list lives
  with the test) -- so a future step arrives product-toned or fails CI.
- Manual checklist: a full install watched end to end -- logo + bar + one-line
  narration, no raw output visible until Show logs; a forced failure
  auto-discloses at the failed step; an uninstall and a repair render the
  same shape; screenshots attached to the PR for the owner to veto.

## Out of scope

- Changing WHICH actions exist anywhere (that is #4423/#4427 territory and
  `instanceActions.ts` doctrine).
- The portal's deployment surfaces (this epic is the VS Code extension).
- Log persistence/export (the run log file machinery is untouched --
  this is display only).
- Cockpit TUI rendering.

## Risks

- Auto-disclosing logs on failure re-shows content D4 works to de-emphasize;
  deliberate -- at failure time the log IS the product. The remedy staying
  outside the pane keeps the common case one-glance.
- The graph-description rewrite touches strings the CLI also prints;
  that is the point (one source), but the CLI's plainer surface should be
  spot-checked in the manual pass.
- String-position ordering tests are blunt; they are cheap and honest, and a
  screen that legitimately needs a different order gets an explicit exemption
  comment where the test lists screens.

## Task breakdown (preview; tasks carry the acceptance criteria)

1. Layout doctrine (D1): reorder every add-cluster + deployment screen;
   ordering tests.
2. Branded run block (D2): progress components in the brand layer,
   `runProgress` projection, narration line, terminal states.
3. Log disclosure (D3): state-held toggle + follow-tail, failure
   auto-disclose, size line, redaction assertions; the same component reused
   for D4's Diagnostics.
4. Content curation (D4): graph-description rewrite + vocabulary gate,
   facts demotion into Diagnostics, the recorded before/after sweep.
