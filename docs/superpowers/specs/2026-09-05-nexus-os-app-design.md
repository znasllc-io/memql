# Nexus on MemQL OS -- Design

- **Date:** 2026-09-05
- **Status:** approved. The three forks below (D1-D3) were put to the owner as
  selectable options in the 2026-09-05 design round and each was answered; they
  are not open questions. Everything else is a recommendation with its
  rationale and what it rejected.
- **Program:** sub-project **B** of the five-part program agreed in the
  2026-09-05 brainstorm -- A the spine, **B Nexus on MemQL OS (this record)**,
  C the Materializer, D the Files places, E portal removal, F cognition/spaces/
  voice. A landed first because Nexus's shape is the spine's shape (spine
  record D2), so B is built once.
- **Epic:** memql#4785, tasks memql#4974 / memql#4975 / memql#4976.
- **Scope:** `clients/os/src/apps/nexus/` (from `apps/work/`),
  `clients/os/src/nexus/scene/` (re-pointed), `clients/os/test/nexus/`,
  `clients/os/src/styles/index.css`, `clients/os/README.md`,
  `dsl/work/{queries,builtins}.memql` (additive).
- **Reads first:** [the work spine](2026-09-05-work-spine-design.md) ·
  [the portal Nexus](2026-08-22-nexus-living-map-of-a-goal-design.md)
  (deleted surface, live reasoning) · `clients/os/README.md`

---

## Why

A person gives the system a goal. The system works out how to do it once,
records the working as automations it can replay, and from then on runs it
without a model unless reasoning is genuinely needed. That claim is the
product. **It is also completely invisible in a table.**

Forty-seven rows that all look alike cannot tell anyone that three of them
cost money and forty-four were free, that the goal is two phases from done, or
that the run has been parked on a question since Tuesday. The spine's rows
already carry every one of those facts. What is missing is a surface that
shows them without being read.

Nexus is that surface: the goal as the unit of intent, drawn as a place the
work arrives at.

---

## Locked decisions

### D1 -- Nexus subsumes the Work app; it does not sit beside it

`clients/os/src/apps/work/` is DELETED and `clients/os/src/apps/nexus/`
replaces it. One app, id `nexus`, name "Nexus".

Sub-project A shipped a Work app over `v1:work:*` -- Goals, Runs, Approvals,
Logs, Settings -- and B is specced to draw the same populations plus a map,
Automations and Replay. Two apps both listing goals and both holding an
approvals queue is the exact shape this codebase writes down against on ten
manifests: *a second copy of the list is one that can disagree*. Here it would
be worse than a stale label -- an approvals inbox that two windows disagree
about is a run somebody thinks they unparked.

The rename is not a repaint. Everything the Work app earned is kept: the three
root feeds one-per-concept, the per-run steps feed held by the page, the
journal as an honest on-demand read, the step spine, the action bar's
legal-acts rule, the settings store. What changes is that the goal, rather
than the run, is the thing the app is about -- and `actions.ts` already wrote
`requestedVia: "nexus"` with a comment saying this app was "the first half of
it", so the two halves were always one app.

Rejected: keeping both (a duplicated queue), and keeping the name "Work" (the
epic, the file path in memql#4974 and `v1:work:goal.requestedVia`'s own enum
member all say `nexus`; the launcher would be the only place that did not).

### D2 -- The map is the beacon timeline, flattened to SVG

You at the start, the goal at the far end as the thing the work arrives at,
phases marching left to right between them, what the run produced below the
road and who ran it above.

The portal's record calls this the emotional core of the surface and states
the correction that produced it: the obvious arrangement puts the goal at the
origin with work radiating out, and watching it materialize made the mistake
plain -- **a map whose centre is the request has nowhere for progress to go.**
That reasoning is renderer-independent, so it survives the move to 2D intact.

Rejected: the Deployables columnar flow (goal | runs | steps), which is
cheapest -- `map/layout.ts` adapts almost as-is -- but reads as a structure
diagram. It answers "what is connected to what", and the question here is "how
far along is this". Also rejected: compressing the map to a phase ribbon above
the rail, which makes the rail the whole surface and the goal stop being a
destination.

### D3 -- A Runs section stays

`v1:work:run.goalId` is EMPTY for an automation run no goal asked for -- a
scheduled sweep, an event-triggered automation. In a goal-only app those runs
have no home at all.

So Nexus keeps a top-level Runs section over every run the caller owns,
goal-born or not, with the show-finished preference. A goal-born run also
opens from its goal; this is a second door on the same room, not a second
room, and it is the same reading Files takes with Browse and the Bin.

Rejected: grouping goal-less runs under a synthetic "Automations" goal (it
fabricates a row that does not exist and mixes a real population with an
invented one), and listing them only under their template (a person asking
"what ran last night" should not have to guess the template first).

---

## A. The app

| Section | What it is | Live? |
|---|---|---|
| **Goals** | Every goal the caller owns, newest first, running on top. Opening one replaces the list with the goal view. | live -- `v1:work:goal` broadcasts |
| **Runs** | Every run the caller owns, goal-born or not (D3). | live -- `v1:work:run` broadcasts |
| **Automations** | What this instance can replay without a model. | **a read**, see section D |
| **Approvals** | Everything parked waiting on a person. | live -- `v1:work:approval` broadcasts |
| **Logs** | `logger.Subject` over this app's concepts. `roles: { min: "admin" }`. | the log store's own rule |
| **Settings** | Open-on, finished runs, and the ceilings account. | -- |

Goals is first and is therefore the section a window opens on. Settings can
point a window at Approvals instead, which is the choice somebody who lives in
this app all day makes: a run parked on a question does not move until a person
answers it.

**No manifest role**, and the reasoning is Files', Deployables' and Campaigns':
every `v1:work:*` concept declares the composite tier
(`@rowAuthz(owner="ownerUserId", clusterOwner)`), so every signed-in person has
goals of their own and the engine decides how far each list reaches. Gating
here would be presentation pretending to be authorization. Section G says what
the role mirror does and does not amount to.

**Four feeds at the root, one per concept** (goals, runs, approvals,
automations-is-not-one), and the steps feed is the page's. That rule is
inherited unchanged from the Work app, which inherited it from Deployables:
subscribing a window to every step of every run in order to draw one of them is
what it forbids.

---

## B. The goal view

A goal opens in place of the list -- the `<- Goals` form of DESIGN.md rule 11,
because this page is tall and two Heads in one scroller is the tell that
neither happened.

```
+-- Ship the Q4 pricing page --------------- open . 2 runs . $0.41 --+
|  MAP                                                               |
|                                                                    |
|   you    [ plan ]      [ build ]        [ verify ]                 |
|    O-------o--o--o-------*--*--*--*--*-----o----o----------->  (@) |
|             .              .        .          .              GOAL |
|          artifact      construct  <pause>approval          filling |
+---------------+----------------------------------------------------+
|  RAIL         |  decide . waiting on you                           |
|  | v compile  |  tier B . budget . rule spend-ceiling              |
|  | v fetch    |  ran: gpt-5.4-mini . 1.2s . $0.004                 |
|  + * decide   |                                                    |
|  | o write    |  [ Approve ]   [ Reject ]                          |
+---------------+----------------------------------------------------+
```

### The map

- **The road runs left to right and brightens behind the work.** The segment
  between two phases is drawn at the completion of the phase behind it. There
  is no animation on a timer -- the road changes when a step's status changes,
  which is the only thing that should move a picture somebody is reading.
- **The beacon fills.** The goal's node carries a completion arc: steps done
  over steps known. It is the one place in the app where progress is a
  proportion, and it is honest about the denominator -- a run still compiling
  has no step count, so the beacon is drawn EMPTY with the word `compiling`
  rather than at some fraction of a number nobody has yet.
- **A phase collapses at density, and expands on click.** Inherited from the
  portal's layout with its threshold intact. A collapsed phase still reports
  its real count -- that is the number the cluster node renders.
- **Above the road: who ran it.** A step's `binding` names a surface, a
  machine, a provider and a model; those are drawn as marks above the road at
  the step they belong to, because "who did this" is a question asked while
  looking at the step.
- **Below the road: what it had to ask.** An approval hangs under the step
  that raised it, so a parked run shows WHERE it is parked without being read.
- **The x axis is DEPENDENCY DEPTH, not `seq`.** Steps that can run at the
  same time sit in the same column. That is the map's one structural claim and
  it is true of the rows: `dependsOn` is a real edge, so the column is a fact
  about the automation rather than a rendering convenience. In the common case
  where every step follows the previous one, every column holds one step and
  the map is a straight road -- which is the correct picture of that run.
- **Finished stretches fold.** A maximal run of consecutive columns that are
  entirely `done` collapses to one segment carrying its count, and expands on
  click. That is what makes a finished forty-seven-step run readable, and it
  is the density control memql#4974 asks for under the word "phases": there is
  no phase ROW in the spine, so folding is applied to the structure that does
  exist rather than to a grouping nobody writes.

**NOTHING DRAWN BELOW THE ROAD IS AN ARTIFACT, AND THAT IS A FINDING RATHER
THAN AN OMISSION.** The portal's map hung artifacts and authored constructs
under the task that made them, and the equivalent join does not exist here:
`v1:library:artifact.producedByPlanId` still names `v1:planner:plan`, and
`v1:authoring:bundle.sourcePlanId` does too -- the spine retires those
concepts in its section F, gated on epic A3, and until that lands nothing
points a produced thing at a run. `v1:skills:skill` is the one population that
DID get re-keyed (`originatingGoalId`, `mintedByRunId`), and it is not drawn
either, because it would cost a fifth feed for a population most goals never
touch. Drawing any of them today would mean inventing a join, which is the one
thing a map read as evidence must not do.
- **Zoom keeps the point under the cursor under the cursor**, and
  `touch-action: none` on the canvas is load-bearing. Both are lifted from
  `deployables/map/viewport.ts` rather than re-derived, which is the point of
  having written them down once.

**One run at a time, and the goal picks it.** A goal's progress toward being
done is ONE run's progress; a replay or a fork is a different attempt at the
same goal, not further progress on it. So the map draws the goal's active run
-- or the one the header's run picker names when there is more than one -- and
a `subrun` step expands into its child run inline. Drawing every run of a goal
end to end would put two attempts at the same work on one road and read as
twice the work.

### The rail

The step spine, unchanged from sub-project A, and it is the device this app has
that no other app has: **a deterministic step is a hollow node on a hairline
and a reasoning step is a filled node on thick ink**, so a long run reads as a
thin grey thread with a few dense knots in it. The cost readout obeys the same
rule -- only a step that thought shows tokens and money -- so the visual weight
and the bill are in the same places.

The map and the rail share ONE selection, held as ids rather than as node
objects, because the layout re-derives on every event. Clicking a step in the
rail frames its node on the map; clicking a node on the map opens its step in
the rail. That is the Deployables `MapSection` rule and it is the same rule for
the same reason.

### One action bar, and an illegal act is absent

Rule 12. On the goal: **Cancel goal** while it is open, **New run** never (a
goal gets its run from `createGoal`; a second one is a fork or a replay of a
run, both of which live on the run). On a run: **Replay** and **Fork** on a
terminal run only -- a replay serves every model call from the journal, so
replaying a run still writing that journal is offering a failure -- and
**Answer** when it is parked on an approval.

There is no Cancel on the run page, and the bar says why: the verb is
`cancelGoal`, which asks EVERY run of the goal to stop, so a button here would
destroy this run's siblings from a page about this one.

---

## C. The scene library, re-pointed

`clients/os/src/nexus/scene/` survived the portal's deletion (spine record D7)
because it is functions over rows with no renderer and no GPU. Sub-project B
does the two things its own header says are left:

1. **`concepts.ts` re-points** from `v1:planner:plan` / `v1:planner:task` to
   `v1:work:goal` / `v1:work:run` / `v1:work:step`. The SHAPE of a goal's world
   -- a root, its steps by phase, the artifacts and constructs hanging off them
   -- is unchanged by the rows underneath it, which is the whole reason the
   library was worth keeping.
2. **`layout.ts` becomes 2D.** The portal's layout emits `{x, y, z}` with lanes
   on `y` and wrapping along `z`. SVG has two axes, so the lane keeps `y` and
   the wrap becomes a `y` offset inside the lane's band. Nothing else about the
   function changes: it stays pure, it stays deterministic, it still sorts its
   own input, and it still compares timestamps as STRINGS (RFC3339 in a fixed
   offset sorts lexicographically, and a string compare cannot silently produce
   `NaN` the way `new Date("")` does -- the failure that turns a missing
   timestamp into a node at the origin).

**Node identity is not row identity, and the difference is the retry.** The
portal keyed a retried task's node on `logicalStepId`. The spine's equivalent
is the step KEY: a retry increments `attempt` and keeps `key`, so the node keys
on `runId:key` and `rowId` names the latest attempt -- the one an operator
opening the node wants to read.

`events()` invents nothing: a moment with no timestamp produces no event,
because a scrubber is read as evidence. That rule is carried over verbatim and
it is what makes Replay defensible.

**No WebGL, and it is enforced.** `test/nexus/map.test.tsx` scans the module
graph for a three.js import AND checks the package manifest, because a static
import is only one of the two ways one gets in. Both halves carry the
reachable-positive that makes an empty offender list evidence about the tree
rather than a statement about the regex. This is the Deployables map's guard,
applied to the app the owner named WebGL-free.

---

## D. Automations

What this instance can replay without a model, where each came from, how well
it has done, and whether it is armed.

The rows are `v1:authoring:construct` with `kind == "automation"` and
`catalogued == true`, read through `cataloguedConstructsForOwner`. They carry
exactly the four facts this section is for: `goalSignature` (what goal shape it
answers), `catalogedFromBundleId` + `catalogedAt` (where it came from),
`reliability` + `reinforceCount` + `lastReinforced` (the trust ladder), and
`status` (draft / staged / active / retired).

**It is a READ, not a feed, and the surface says so.** `v1:authoring:construct`
carries no broadcast routing rule in `component/node/routing.go`. A
`useLiveCollection` over it would render "Loading from the cluster" and then a
list that silently never moved -- worse than a plain read, because the caption
would be claiming wiring that is not there. So the section prints when it
looked and offers to look again, which is the call the Training app made for
the knowledge side and Accounts made for its ledger. **The absence of a routing
rule was checked, not assumed** -- that is the README's standing rule and this
is what checking it produced.

Re-read after an act, because an act is a change this window caused and
therefore knows about.

**Activate and retire are the authoring catalog's own verbs**
(`setConstructStatus`), offered on the action bar under rule 12: retire is
absent on a retired construct, activate is absent on an active one. Nothing
new is invented for this -- the gate the engine already runs is the gate.

**Reliability is rendered as a ladder position and never as a percentage.**
`0.0-1.0` printed as "62%" invites a reader to treat it as a probability of
success, which it is not: it climbs on a matched-fingerprint success and decays
on mismatch and on disuse. Absent reads as 0, and 0 is drawn as *not yet
proven* rather than as *0%* -- a template nobody has run has earned nothing,
which is a different sentence from one that has failed.

---

## E. Approvals

Everything parked waiting on the person, each with its safety tier, its reason
and its rule id, decided through `decideApproval`.

**The parked-runs feed at the app root is the one recorded exception to the
timeline rule.** Steps are per-page because subscribing to every step of every
run to draw one is indefensible; approvals are per-ROOT because an approval is
not detail about a run you are looking at -- it is a demand on your attention
raised by a run you are not. The queue has to be right when the app opens, from
any section, or a run sits parked while somebody works in another window.

`workApprovalsForOwner` carries `decision==""` in the DSL, so the feed is the
inbox by construction: deciding one is the act that empties the queue, and
watching the row go is the confirmation. That is why there is no success
message and no "show decided" toggle -- a toggle over the rows this app holds
could only reveal approvals decided while the window was open, a list that
filled as you worked and was empty again on reload.

A refusal here is the one worth reading in full and it is shown verbatim: the
builtin refuses a decision whose artifact hash changed since it was raised,
because **an approval is a decision about one specific command, patch, message
or draft and never carries to a modified one.** A paraphrase would drop the
only fact that tells somebody to look again.

---

## F. Replay as a mode

Replay is a MODE of the goal view, not a fifth section. Same map, same rail,
same selection -- a scrubber appears and the world is drawn `at` a moment
instead of now.

- **The timeline is the rows' own timestamps.** `events(world)` in the pure
  library produces the moments; `scene(world, at)` produces the world as it
  stood. Neither invents anything: a row with no timestamp contributes no
  event, so the scrubber's ticks are evidence rather than interpolation.
- **A moment is a URL.** `?at=<rfc3339>` on the goal view. Determinism is what
  makes this work at all -- `layout(sameWorld)` must give the same answer
  twice, or a deep link frames a different picture than the one that was
  shared.
- **The phase boundaries are marked on the scrubber from the layout**, not
  recomputed, so the two cannot disagree about where a phase started.
- **It is not the `replayRun` builtin.** This is scrubbing a recorded run's
  rows; that verb opens a NEW run served from the journal. Both are offered and
  the words are different on purpose -- "Replay" on the action bar makes a run,
  "Rewind" enters the mode. A surface that used one word for both would have
  people spending money by dragging a slider.

---

## G. Roles, and what the mirror amounts to

memql#4976 asks for `@requiresRank` on the constructs the app calls, mirrored
by the manifest's `roles`, with the parity test. Doing that work honestly
produces an ABSENT floor with an account of itself, which is the answer and not
a skipped task.

- **The `v1:work:*` constructs carry no rank floor and should not.** They are
  composite-owner-tier: a goal is yours, and the engine's row admission decides
  which rows a read reaches on both the query and the subscription path. A rank
  floor over "your own goals" would say a writer may not read their own work.
- **The builtins gate in their handlers**, because a builtin's annotation set
  carries no `@requiresRank`. That is where `createGoal`, `cancelGoal`,
  `decideApproval`, `replayRun` and `forkRun` are decided, and none of it is
  decided in a browser.
- **The Logs section carries `roles: { min: "admin" }`** and that IS a mirror:
  every read on the log store is admin-and-above in the Go handler (spine
  record L3). Rank >= 200 under the one ladder is {admin, developer, owner},
  which is the set the engine admits.
- `TestAppManifestMirrorsTheEngineFloor` (`component/auth`) covers the app by
  id, so the rename is a rename there too.

The active decision is therefore: **no app-level role**, one section floor that
mirrors an engine floor, and this paragraph so that the absent control does not
read as something nobody got round to.

---

## H. New goal, and Ask-to-goal

`createGoal` opens the goal AND its first run in `compiling` and dispatches
compile, so ONE call is the whole act -- there is no client-side follow-up
write to get half-done.

**New goal** is the app's own composer, on the Goals section.

**Ask-to-goal** is the Ask surface handing a prompt off. Ask is the OS's prompt
surface and a goal is what replaces the chat prompt as the unit of intent, so
the handoff is a first-class act there rather than a copy-paste: Ask offers
"Make this a goal", calls `createGoal` with the prompt as the statement, and
opens Nexus on the new goal through the shell's open-intent with `{ goalId }`.

Both write `requestedVia: "nexus"`, which is the enum member for this shell's
surfaces. Guessing `"api"` would file every goal a person typed as one a
program submitted.

**Nothing is inserted locally on either path.** `v1:work:goal` broadcasts, so
the row arrives on the feed the list already draws, with the arrival cue,
exactly like a goal a responsibility raised. A local insert would put a row on
screen the cluster had not confirmed, and the two would differ in whatever the
optimistic copy guessed wrong.

---

## I. The arrival cue

The README's rule, applied to this app's populations, because it is the rule
that goes wrong by default.

- **Fingerprint what a person would call a change.** For a goal: statement,
  status, closeReason. For a run: status, waitingOn, finishedAt. For an
  approval: decision. For a step: status, symptom, attempt.
- **`heartbeatAt` IS NOT IN ANY FINGERPRINT**, and it is the field that would
  do the damage: it moves on a timer for every running run forever, so naming
  it turns the whole list into a strobe -- the standing badge the cue exists
  not to be. `spent` is out for the same reason: it climbs continuously while a
  run works, and the run page displays it continuously already.
- **A resync is not an arrival**, and the show-finished toggle re-baselines by
  keying the `LiveList` on it -- revealing rows the browser already had is not
  the cluster sending them.

---

## J. Testing

Pure-library tests carry the weight, which is the point of the library being
pure:

- `test/nexus/scene.test.ts` -- `layout()` determinism over the 300-node
  fixture, the minimum-separation guarantee inside a lane, phase collapse, and
  `layout(sameWorld)` twice.
- `test/nexus/events.test.ts` -- a row with no timestamp produces no event
  (both directions: the reachable positive is a row WITH one that produces
  exactly one).
- `test/nexus/map.test.tsx` -- no three.js in the module graph and none in the
  manifest, both halves with a reachable positive.
- `test/nexus/goalView.test.tsx` -- the shared selection: clicking the rail
  frames the map node and the reverse.
- `test/nexus/automations.test.tsx` -- the section says when it looked; an act
  re-reads.
- `test/nexus/replay.test.tsx` -- a moment is a URL and the URL restores the
  moment.
- `test/nexus/app.test.tsx`, `approvals.test.tsx`, `rows.test.ts`,
  `runPage.test.tsx`, `settings.test.ts` -- carried over from the Work app.
- A **rendered pass on a live cluster**, both modes, empty and populated. The
  README's own section records that the suite cannot see what the browser
  found; jsdom has no layout, so a map is exactly the surface a green suite
  says nothing about.

---

## K. What it deliberately does not do

- **No cluster-wide operator view.** Every read is the caller's own. A view
  over every person's goals is named as a follow-up in the spine record and
  needs a tier decision, not a page.
- **No goal editing.** A goal's statement is what a person asked for; changing
  it after runs exist would re-write the question the answers were to. Close it
  and open another.
- **No ceilings editor.** A run's ceilings are the goal's, set when the goal is
  accepted; the settings section says so where somebody would look for the
  field.
- **No canvas cards.** Canvas is pack-only; the approvals queue is where a
  human gate lives now.
- **No 3D, ever, and it is enforced rather than intended.**

---

## L. Delivery

ONE PR closing memql#4974, memql#4975 and memql#4976, and closing the epic
memql#4785 -- the owner's direction on 2026-09-05, overriding the two-PR split
those issues were written under. The plan file is deleted in the same merge.
