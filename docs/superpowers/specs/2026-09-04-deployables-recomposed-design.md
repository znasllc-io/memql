# Deployables Recomposed -- Design

- **Date:** 2026-09-04
- **Status:** approved for build. Three forks were put to the owner as
  selectable options and each was answered; they are D1, D2 and D3 below and
  are not open questions. Everything else is a recommendation with its
  rationale and what it rejected.
- **Scope:** `clients/os/src/apps/deployables/**` (the list, the page, the
  rail, the compose flow), `clients/os/DESIGN.md` (two new rules),
  `clients/os/src/kit/` (one new piece), `dsl/platform/` (the delete
  capability, the cancel verb), `component/memql/platform_site_status_guard.go`
  and `platform_site_hostname_policy.go`, `component/packages/**` (the cancel
  flag, the stage boundaries, the per-app skip), and
  `dsl/platform/concepts.memql` (`packageDeployment.status`).
- **Extends:** [2026-09-02-deployables-program-design.md](2026-09-02-deployables-program-design.md)
  (the program), [2026-09-02-deployables-compose-design.md](2026-09-02-deployables-compose-design.md)
  (the rail as the form, which this keeps), and
  [2026-09-01-packages-and-deployables-design.md](2026-09-01-packages-and-deployables-design.md)
  (the D10 lifecycle, which this extends by exactly one rung).
- **Closes:** memql#4930 (pick which declared deployables to deploy).

---

## Why

The Deployables app composes, deploys and manages what this cluster serves,
and it does all three in one scroll column. Opening a live deployable on a real
cluster produces this:

| Measured on `storefront.memql.example.com`, 2026-09-04 | |
|---|---|
| One scroll column | **5,069 px, 5.9 viewports** |
| `os-head` elements stacked in it | **2** (the list's, then the page's) |
| Rails drawn on one page | **13**, at three different meanings |
| Interactive controls in one column | **36** |
| Pause (the act a person came for) | y = **2,412** |
| Archive this deployable | y = **2,499** |
| Archive this SOURCE AND EVERY APP IT PRODUCED | y = **885** |
| Controls reading "Retry" | **6**, carrying **2 different promises** |

Four separate faults are visible in that table.

**The detail shares the list's scroll column.** Every other app in this shell
either puts the detail beside the list in its own scroller (`.os-bin-list`
carries `overflow-y: auto`) or replaces the list. Deployables appends a whole
page beneath the list it was selected from, which is where the second Head and
most of the 5,069 px come from. This is the one app that did not adopt the
pattern the shell already had.

**The dangerous control is the easiest to reach.** `packageArchive` cascades
to every site its package produced, so "Archive this source and every app it
produced" on `web`'s page also archives `storefront`. It renders 1,614 px
ABOVE that page's own archive, because it is drawn inside the Source stop and
the Source stop comes first.

**The same 2,600 px of history is drawn twice.** `usePackageDeployments`
reads the PACKAGE's timeline, so `storefront` and `web` render an identical
"Every attempt". Six attempts, each a full six-stop rail with its own refusal
block, below everything else on both pages.

**One word, two promises, six buttons.** The Head's Retry deploys the source
again; an attempt's Retry deploys the bytes that lost run had already fetched.
`clients/os/README.md` names this as the thing being avoided. The rendered
page has six of them at once.

### And three things are missing outright

**A draft is a dead end with a trap.** On a draft deployable
(`tee.memql.example.com`, read live):

- the Head offers `Open`, `Logs`, `Ask` and **no primary action at all**;
- the Availability section does not render (`Live.tsx` gates it on
  `live || paused`), so there is **no control that can reach `disabled`**;
- "Archive this deployable" **renders enabled** -- and
  `validateSiteStatusTransition` refuses it, because archiving requires a
  prior status of `disabled`.

So the only lifecycle control a draft offers is one the engine rejects, and
the state it demands first is unreachable. A draft cannot be cancelled,
archived or deleted from this shell.

**There is no delete in MemQL OS.** `deleteSite`
(`dsl/platform/mutations.memql:550`) exists and the deprecated portal calls
it. The OS shipped archive only.

**Archiving does not free the hostname, and nothing tears a domain down.**
`liveSiteIdsForHostname` (`component/memql/platform_site_hostname_policy.go`)
is the cluster-wide uniqueness probe. It excludes rows carrying
`deleted: true` and **never reads `status`**. So an archived
`fylo.memql.example.com` refuses a new deployable at that name permanently, with
no control in the OS able to release it. And no automation anywhere reacts to
a site status change, so an archived site keeps its `v1:platform:customDomain`
rows at `live`: the Ingress and the Certificate stay applied and the hostname
stays claimed against both concepts.

**There is no cancel path for a deployment anywhere in the tree** -- not a
builtin, not a mutation, not a status. A run ends by finishing or by dying.

---

## What was verified before any of this was decided

Reading the code is not the same as reading the product, so both were done.

- **The rendered page was measured in a browser** on a live cluster at
  1,600 x 1,100, both themes, list / draft / published / in-flight. Every
  figure in the table above is a `getBoundingClientRect` offset inside
  `.os-window-content`, not an estimate. jsdom performs no layout and resolves
  no custom properties; none of those numbers is observable from the suite.
- **The draft dead end was read off the DOM**, not inferred: the page's Head
  carried three buttons and no primary, `os-report-heading` contained no
  "Availability", and `Archive this deployable` reported `disabled === false`.
- **The uniqueness probe was read to its last line.** `deleted` is the only
  exclusion; `status` is never consulted.
- **The absence of a cancel path was established by a reachable positive:**
  the same grep that returns nothing for a cancel verb returns
  `StatusAbandoned` and the whole `packageSweepAbandoned` machinery, so the
  instrument could have moved.
- **memql#4930 was found already open** and its notes are adopted here
  verbatim rather than re-derived.

---

## Locked decisions

**D1. Delete is a fourth rung below Archive; archive semantics do not
change.** (owner) Two other shapes were offered and rejected: making archive
itself release the name (it redefines every archived row that exists and makes
restore no longer an undo), and collapsing to draft / published / deleted (it
loses the "parked but the name is still mine" state).

**D2. Selecting a row replaces the list, settled stops collapse, and one
action bar is pinned to the window's bottom edge.** (owner) A left-spine
variant and a minimal "pin the bar and leave the rest" variant were offered
and rejected -- the first costs the rail its top-to-bottom reading, the second
leaves both Heads and most of the scroll length.

**D3. Cancel reaches a run in flight, up to the roll.** (owner) It covers
fetch, analyze, the confirm gate and the build, which is where the time goes.
Once `stage DSL` begins the control disappears and the surface says why. A
"mark it abandoned by hand" variant was rejected as dishonest: it changes the
record without changing the reality, and `abandoned` today means the cluster
OBSERVED a loss rather than that somebody chose one.

**D4. A source is a thing with its own page.** Not a locked owner decision,
but load-bearing for D2 and adopted here: the credential, the auto-deploy
switch, the archive-the-source cascade and the run history are facts about the
source, and rendering them inside each app's Source stop is what produced both
the duplicated history and the mis-placed cascade.

**D5. Discard is a delete, not an archive.** A draft that never served skips
the disable-then-archive ceremony entirely. The pause exists to give people
still using a site a chance to notice; nobody is using a draft, because a
draft resolves for nobody.

**D6. The surface's words change; the enum values do not.** `live` reads
**Published**, `disabled` reads **Unpublished**. No migration, no new enum
member on `v1:platform:site.status`. The distinction the engine draws (503 for
a deliberate pause, 404 for an archive) is preserved in the copy beneath the
state rather than in the state's name.

---

## A. Information architecture

Three sibling views, one at a time, one Head each.

```
List  --select an app-->  Deployable   --history-->  History
  |                            |
  |   --select a source-->  Source
  |
  '-- New deployable ------>  Compose
```

Compose already works this way (`ComposePage` replaces the list and offers a
quiet Back). The deployable page joins it, and the source and the history
become views of their own.

`DeployablesSection` currently holds `selectedSiteId` and renders
`<DeployablePage>` as a sibling of `<LiveList>`. It gains a view union
instead:

```ts
type DeployablesView =
  | { kind: "list" }
  | { kind: "deployable"; siteId: string }
  | { kind: "source"; packageId: string }
  | { kind: "history"; packageId: string }
  | { kind: "compose"; parkedPackageId?: string };
```

The selection stays the app root's (`MapSelection`), so walking the map and
switching to the list still lands on the same deployable -- opening one now
means entering its view rather than expanding a panel under the list.

**The feeds do not move.** One per concept, retained at the app root, exactly
as `clients/os/README.md` requires. The deployment TIMELINE stays retained by
the view that shows it, and the parked-runs feed stays the one recorded
exception.

---

## B. The list

**One list language.** Every line is a row. A source is a real row that opens
its own view; the apps it produced indent beneath it. Today the source line is
a `div` of caption text with two chips wedged between clickable rows, which is
the "list inside a list" the owner reported -- and it is not reachable by
keyboard or announced as anything.

**Sorting is one level.** Sources and hand-made deployables sort together;
a source's apps sort within it. No flat-then-grouped-then-flat interleaving.

**The rail gets a fixed trailing column.** `.os-deploy-railcol` at a fixed
width, last in the row's trailing group, so the five marks land at the same x
on every row and can be scanned down the list. Today the rail follows a
variable-length hostname and lands in a different place on every row.

The Refine facets, the archived flip, the traffic chip and the arrival cue are
unchanged.

---

## C. The deployable page

**The Head carries the name and the quiet three** -- `Open`, `Logs`, `Ask` --
plus the Back. **No primary action lives in the Head any more**; it moves to
the bar in section D. `headStateFor` / `headActionFor` (`page/head.ts`,
`page/rail.ts`) are kept and re-pointed: the state reading is right, only its
rendering site changes.

**A settled stop is one line.** Mark, label, its answer, a disclosure
chevron. `railFor` gains no fourth mode -- the three modes are correct and
`clients/os/README.md` argues for keeping them at three. What it gains is an
`answer` per stop, which `standing` already computes for its notes, and
`Rail.tsx` gains a disclosure around `stopBody`.

**Exactly one stop is open, and it is chosen by what is a question:**

| Situation | Open stop |
|---|---|
| a run in flight | the stop the run is at |
| the newest run refused | the stop it stopped at, with the refusal |
| draft | Live -- "nothing is served yet" |
| published or unpublished | Live -- traffic, versions, settings |
| archived | Live -- "the name is still held" |

An earlier stop reopens on click, and opening one closes the other: a person
reading a deployable is answering one question at a time. A stop the flow
cannot reach yet stays a dim line and is not clickable, which is the rule the
compose reading already follows.

**The history is one line** at the foot of the rail --
`History - 6 attempts, last refused 4 Sep` -- and it opens the SOURCE's
history view. `EveryAttempt` moves there wholesale.

Note the arithmetic: one rail instead of thirteen, one Head instead of two,
and the whole deployable inside one viewport.

---

## D. The action bar

A new kit piece, `kit/ActionBar`, pinned to the bottom edge of the window's
content pane (a grid row, not `position: fixed` -- the desk plate is
CSS-transformed and becomes a fixed element's containing block, which
`clients/os/README.md` already records against the Logs jump pill).

Left: the state in words, with the shell's dot. Right: the acts legal from
that state, at most three, primary last.

| State | Bar reads | Acts |
|---|---|---|
| Draft, never published | `Draft` | `Discard` - **Publish** |
| Draft, a run refused | `Draft` + the refusal in one clause | `Discard` - **Retry the deploy** |
| Published | `Published - serving since <date>` | `Unpublish` - **Deploy the update** |
| Unpublished | `Unpublished - visitors get 503` | `Archive` - **Publish** |
| Archived | `Archived - answers nothing` | `Delete` - **Restore** |
| Run in flight, before the roll | `Building - 2m 14s` | **Cancel** |
| Run in flight, from the roll on | `Rolling` | none, and the bar says why |
| Deleting | `Deleting - releasing <host>` | none |
| `systemOwned`, or a reader | the state alone | **no acts** |

**An act that is not legal is ABSENT, never disabled.** This is the existing
rule (`Live.tsx` already argues it at length for system-owned rows) applied
uniformly -- and it is what fixes the draft trap: a draft never renders an
Archive the server would refuse, because Archive is not legal from `draft`.

**Nothing that changes the thing's state lives anywhere else on the page.**
Pause, Resume, Archive, Restore and the Head's action all leave their current
homes. The Live stop keeps only the READINGS -- traffic, versions, runtime
settings -- which is what a non-admin owner may see anyway.

**The bar renders no empty band.** With no acts and no state to name -- the
list view -- there is no bar. Nothing happens where nothing is offered.

---

## E. The lifecycle

```
Draft --> Published <-> Unpublished --> Archived --> Deleted
  |                                                     ^
  '------------------ Discard --------------------------'
```

### E1. `siteDelete`, a new capability

`@sdk @executor("integration.packages.deleteSite")`, beside `siteArchive` and
`siteRestore` in `dsl/platform/builtins.memql`, taking `siteId` and
`confirmHostname`. A capability rather than the raw `deleteSite` mutation,
because the cascade below has to be one decision made in one place -- a client
that stamped `deleted: true` itself would release the name and leave the
domains bound.

It runs under the caller's actor, resolves the site through the owner-scoped
read first (so a caller who cannot read the row is refused by name before
anything is written), and then:

1. **refuses unless the site is `archived` or `draft`.** Those are the two
   states nothing is being served from. `live` and `disabled` are refused with
   the sentence naming the next step.
2. **refuses unless `confirmHostname` matches the stored hostname exactly** --
   the same typed confirmation `siteArchive` verifies server-side.
3. **refuses a `systemOwned` row outright.** The existing
   `validateSiteSystemOwnedDelete` guard already refuses this beside
   `executeWrite` whoever asks; the capability refuses it earlier so the
   message names the reason rather than the guard.
4. **walks every `v1:platform:customDomain` row for the site to `removing`**
   through the existing `removeCustomDomain` mutation. The reconciliation
   sweep takes the Ingress and the Certificate away on its own schedule; the
   engine gains no Kubernetes client for it, which is the constraint
   [2026-09-01-custom-domains-design.md](2026-09-01-custom-domains-design.md)
   D2 fixed and this must not break.
5. **disarms `autoDeploy` on the package when this was its last live app**, so
   a source whose apps are all gone stops fetching on a timer.
6. **stamps `deleted: true`** on the site row, last. The hostname is free at
   that instant, because that is the field the uniqueness probe already reads.

Order matters and is not an implementation detail: the site row is stamped
LAST, so a failure part-way leaves a deployable that is still findable and
still says what state it is in, rather than an invisible row holding a name.

### E2. Deleting is a state the surface shows

The domain teardown is asynchronous -- `customDomainReconcile` runs every two
minutes -- so "deleted" is not instantaneous and pretending otherwise would
have the list drop a row while its certificate is still installed.

`Deleting` is **derived, not stored**: a site carrying `deleted: true` whose
custom-domain rows are not all `removed` yet. No new field. The bar reads
`Deleting - releasing shop.acme.com` and the row keeps its place in the
list with a `deleting` chip until the last binding reaches `removed`.

This requires the list's fold to stop dropping `deleted` rows unconditionally
and instead drop them once the teardown is complete. The `sitesAll` read
already excludes them through `isNotDeleted`, so the deleting rows arrive by
subscription only -- which is exactly the case `DeployablesApp`'s existing
comment describes ("a soft delete arrives as an UPDATE"). The fold keeps a
`deleted` row while any binding of its is non-terminal, and drops it after.

### E3. Delete is terminal, and says so

There is no browser for deleted deployables and no undelete. Archive is the
reversible step and it is one rung up; that is why the ladder has both. The
confirm says it in the bar, in full:

> Deleting releases `fylo.memql.example.com` and takes down `shop.acme.com`.
> The record stays in this cluster's history, but the deployable cannot be
> brought back -- restore it from Archived instead if you are not sure.

### E4. `Discard`, for a draft

The same capability with the same confirmation. A draft is admitted at step 1
above, so `Discard` is `siteDelete` under a name that says what it means for
something that never served. It is what closes the dead end.

### E5. The status guard learns one edge

`validateSiteStatusTransition` is unchanged for every existing transition. It
gains nothing at all, in fact -- delete does not move `status`, it stamps
`deleted`. The guard that grows is `validateSiteSystemOwnedDelete`, which
already refuses a systemOwned delete and now also carries the sentence for a
`live` or `disabled` row reaching the delete path without going through the
capability.

---

## F. Cancel

### F1. The row

`v1:platform:packageDeployment` gains:

- **`cancelRequested`** (`bool`) -- set by the cancel verb, read by the
  running node.
- **`cancelled`** as a new member of `status`, terminal, alongside
  `abandoned`. It is **not a flavour of `failed`**, for the reason `abandoned`
  is not: nothing failed and nothing was published, and the timeline's word
  for it is "cancelled" rather than "failed". This is the same argument
  memql#4900 made for `abandoned` and it is the precedent to follow.

### F2. The verb

`packageCancelDeployment(packageId, deploymentId)`, a capability beside
`packageDeploy`. It resolves the deployment under the caller's actor, refuses
a terminal one by name, refuses one at or past `staging_dsl`, and otherwise
stamps `cancelRequested: true`. It does not itself end the run: the node
running it does, which is what keeps the record honest.

### F3. The stage boundaries

`runDeploy` (`component/packages/pipeline.go`) is already a linear sequence --
fetch, analyze, the D9 contents gate, the D12 confirm gate, build, stage+roll,
publish. A `checkCancelled(ctx, deploymentId)` runs at each boundary BEFORE
the next stage begins, and returns a typed refusal that `closeRun` records as
`cancelled`.

The heartbeat loop (`component/packages/sweep.go`) already writes to this row
every `HeartbeatInterval`; the same tick reads `cancelRequested` back, so a
long single stage -- a build -- learns about a cancel within one heartbeat
rather than only at its own end. The build's own context is cancelled from
there.

**The last checkpoint is immediately before `stageAndRoll`.** From the roll
on there is no cancel, because a roll restarts the cluster onto staged MemQL
and stopping half way through is the one outcome worse than either finishing
or not starting. The bar says exactly that rather than showing a control that
would refuse.

### F4. What a cancelled run leaves

Nothing published, every site still serving what it was serving, the snapshot
artifact kept (so `Retry from these bytes` works on a cancelled run exactly as
it does on a lost one), and the report kept if the analysis had produced one.

---

## G. Choosing which deployables to deploy (memql#4930)

The placement stop asks for a hostname for every declared app and
`packageDeploy` deploys all of them. It becomes a per-app choice.

- `Placement` gains **`skip: bool`**. The manifest is untouched -- this is a
  placement-time decision, which is memql#4930's own D2 and the
  program design's rule that the manifest describes the SOFTWARE.
- A skipped app records the pipeline's existing per-app skipped outcome with
  its own reason ("skipped -- you did not deploy this one"), so the run reads
  as a deliberate partial deploy rather than a stage that vanished.
- A **never-deployed** app that is skipped must not trip
  `deployable_binding_missing`, which today refuses a first deploy whose
  placement names no hostname.
- **Rollback and update detection treat a skipped app as "not this run"**, not
  as removed: its site keeps serving and its `bundleRef` is not touched.
- The compose surface renders each declared app as a row with a
  deploy/skip choice; skipping collapses its address field to one sentence.
  **Deploying zero apps is refused at the control** -- the primary act reads
  `Deploy 0 apps` and is absent, because a run that would do nothing is not a
  run.

---

## H. The source as a thing of its own

`SourceView` renders what is a fact about the source: what it tracks, the
commit deployed, the credential it fetches under, the auto-deploy switch, the
MemQL it adds, the apps it produces, and `Archive this source and every app it
produced`. Its own bar reads `Tracked - N apps` with `Archive this source` and
`Deploy`.

The app's Source stop keeps one line and a link. This removes the cascade
control from every app page it does not belong on, and removes the second copy
of the history.

---

## I. The interface language

Two rules join `clients/os/DESIGN.md`. Both are written as the existing ten
are: the rule, the kit piece that encodes it, and what going wrong looks like.

> **11. A list and its detail never share a scroll column.** Beside the list
> with its own scroller, or replacing it with a quiet `<- <list name>` in the
> Head -- both are right, and which one depends on how tall the detail is.
> Two `Head`s in one scroller is the tell that neither happened.
>
> **12. Acts follow the state, in one place.** A surface with a lifecycle
> carries one action bar on the window's bottom edge (`kit` `ActionBar`): the
> state in words, then the acts legal from that state, at most three. An act
> that is not legal is ABSENT, never disabled. Nothing that changes the
> thing's state lives anywhere else on the page.

**Rule 11 is mostly a ratification.** Bin, Campaigns, Users, Accounts and
Training already comply in one form or the other; Deployables is the
violation. So the rule is written, Deployables is fixed, and the rest are
audited rather than rewritten -- an audit issue is filed, not a refactor.

**Rule 12 is new**, and Campaigns is the surface most likely to want it next
(a campaign has draft / scheduled / sending / sent and scatters its acts the
same way).

---

## J. Testing

The suite cannot see a layout, so the assertions are placed where they mean
something.

- **`railFor` keeps its existing tests unmodified.** The reading is correct
  and this design does not change it. The new pure function is
  `openStopFor(input)` -- the table in section C -- and it is tested
  fixture-wise with no DOM, including that a refused run opens the stop it
  stopped at rather than Live.
- **`actsFor(state)` is pure and is the spine of section D's table**, tested
  member by member. Two negative cases carry the design: a draft yields no
  Archive, and a `systemOwned` row yields no acts at all.
- **The dead end gets a regression test that fails against today's code:** a
  draft site renders no control whose action the status guard would refuse.
- **The delete cascade is a db-gated test** in `component/packages`: after
  `siteDelete`, `liveSiteIdsForHostname` returns empty for that hostname (the
  name is reusable), every `customDomain` for the site is at `removing`, and
  a second `createSite` at the same hostname is admitted. That last assertion
  is the one the owner asked for and the only one that proves the feature.
- **The cancel test asserts the boundary, not the race.** `cancelRequested`
  set before the build boundary produces a `cancelled` run that published
  nothing; set after `stageAndRoll` begins, the verb REFUSES. Sequencing two
  goroutines to try to catch it mid-roll would assert timing rather than the
  guard.
- **The skip test asserts a partial deploy is legible:** a run with one app
  skipped records the skipped outcome, leaves that site's `bundleRef`
  untouched, and does not trip `deployable_binding_missing` for an app that
  has never been deployed.
- **A rendered pass in a real browser is acceptance**, per DESIGN.md's closing
  note: list / draft / published / in-flight / archived / delete confirm /
  deleting, both themes, at 1,600 and at a half-desk width. jsdom performs no
  layout and resolves no custom properties, so the scroll-column claim in this
  document is not assertable from the suite and must be re-measured.

---

## K. Cleanup, and the standing practice

This epic lands a docs cleanup with the work rather than after it, and adds
the practice to the workflow so the next epic does the same.

`docs/CLAUDE.md` already carries the rule:

> When a feature ships, update the affected `public/` reference and either
> delete the `internal/planning/` doc or flip an `internal/design/` doc to
> `status: historical` in the same commit. No stale "deprecated" stubs.

What it does not cover is `docs/superpowers/`, which is exempt from both the
front-matter gate and the relative-links gate and had accumulated 748 KB of
**spent implementation plans** for work that shipped in August. An executed
implementation plan is spent; a design record is not. The rule this epic adds
to `docs/CLAUDE.md` states the difference:

> `docs/superpowers/plans/` holds implementation plans, which are **deleted
> when the epic merges** -- an executed plan is spent, and it is the largest
> stale artifact this repo produces. `docs/superpowers/specs/` holds design
> RECORDS, which are kept and cited; a spec is deleted only when its work
> shipped, nothing cites it, and it names a tree that has since been
> restructured, so it can no longer be read as an accurate account of
> anything.

The measured cleanup is in the epic's own task. The disposition rule is the
part that outlives it.

---

## L. Out of scope

- **A browser for deleted deployables.** Archive is the reversible step. If
  one is wanted later it is additive and needs no change here.
- **Cancelling a roll.** Deliberate, and section F3 says why on the page.
- **Rewriting Bin / Campaigns / Users / Accounts / Training to rule 11.**
  They already comply; the audit is an issue, not this epic.
- **Anything about the Map.** It is untouched and stays first.
- **`bundleRef` editing.** Still deliberately absent, for the reason
  `Live.tsx` gives: a field that accepts any URI is a way to point a live site
  at nothing.
- **iOS / Android / macOS targets.** Still a written-down target shape with no
  enum member, per the packages design D5.
