# `clients/os` — MemQL OS

The platform's desktop shell, served at `os.<domain>` (second named
front-door site, memql#4705). A real desktop over the cluster
(memql#4710): desks, windows, dock, files, widgets, Ask.
Design: `docs/superpowers/specs/2026-08-26-memql-os-desktop-shell-design.md`.

- **Desks** hold at most two auto-placed windows (solo centered, two-up
  split, swap/throw by drag); a third app spills onto a new desk. Windows
  minimize to the dock, full-screen, close; apps navigate sections inside
  their one window.
- **Desktop items**: files are Library artifact shortcuts with the
  provenance dot (green = reachable, amber = not). Every icon behaves like
  a desktop icon (epic memql#4842): single click SELECTS, double-click or
  Enter OPENS -- a file hands off to VS Code, a folder opens the Files app
  scoped to it under the Desktop place. A desk folder is a SHORTCUT to a
  `v1:library:folder`: desk create/rename are Library mutations (rename
  from the item's context menu), and remove-from-desk removes the shortcut
  and never archives. The under-icon folder popover the foundation shipped
  is gone -- the Files window is the one folder surface. Widgets are
  desk-resident cards; Ask ships first.
- **Ask** is chrome, not a module: the dock orb, the desk widget and every
  title bar open the same streaming surface. It takes dictation (#4747):
  hold the mic to talk, tap it to keep listening.
- **Roles**: one predicate (`system/roles.ts`) gates apps and app sections
  from `MyAccess.clusterRole`. Presentation only — row authz stays the
  engine's. A requirement is a LADDER MINIMUM (`{ min }`) or an explicit SET
  (`{ any }`), and the set form exists for exactly one reason: Settings ->
  Integrations is owner-or-developer and the ladder puts `admin` between the
  two, so `{ min: "developer" }` cannot leave it out (epic memql#4819).
  The minimum stays the default and the common case — reach for the set only
  when a requirement is genuinely non-monotonic, and say why where you
  declare it.
- **Theming**: `--os-*` token packs on the root (`data-os-theme`); mode
  (light/dark/system) is orthogonal. The wallpaper (the memory field) paints
  from tokens and from the pack's own field parameters. A pack is DATA, and
  Themes in the Launcher is the drawer that sells them (#4745). Windows,
  widgets and sheets re-inherit the tokens but do NOT carry the attribute --
  the foundation spec says they do and it is wrong; a per-window mix is three
  one-line edits away and deliberately not built.
- **Persistence**: `system/store.ts` (`DesktopStore`) — versioned
  localStorage; desks, items, pins, theme. Never windows.
- **The interface language**: [DESIGN.md](DESIGN.md) — the ten owner-set
  rules every app surface follows (epic memql#4848): Head-first sections,
  filters behind one Refine affordance, quiet sort, the control line,
  one container grammar. When a rule and a surface disagree, the surface
  is wrong.
- Phone keeps its own chrome (tab bar, one app at a time, Ask sheet);
  layout is keyed off pointer/hover, never width alone.

Pure state machines live in `src/system/` (tested without React); chrome
in `src/chrome/`; the app/widget contracts in `src/system/registry.ts`;
the shared kit in `src/kit/`. Every app is real: Settings, Fleet, Users,
Deployables, Training, Files (#4721), Accounts (#4800) and Campaigns
(#4827) -- the last stub went with Files, and `StubApp` with it.

## Right-click belongs to the shell

**The browser's context menu is OFF by default and opted back INTO**, at the
shell root (`chrome/browserMenu.ts`, attached to both `os-root` elements).
Back / Reload / View Page Source over a desktop window is the loudest tell
that this is a tab.

The default is inverted at the ROOT rather than audited per element on
purpose: suppressing per element means every new control is one somebody
forgot, while suppressing at the root means a new control is silent unless it
asks to speak. A surface with its own menu (the desk background, a desk item,
a dock pin) still calls `preventDefault` and shows it; the root handler
running afterwards is a no-op.

**Three exceptions, because there the browser's menu IS the feature** and the
shell has no replacement for it: an editable field (cut/copy/paste — without
it, someone with no keyboard shortcuts cannot paste a worker token), a live
text selection (copy), and a link with an href (copy address). The rule is
"nothing happens where nothing is offered", not "nothing ever happens". When
the shell grows its own clipboard menu, that list shrinks.

## The rail stops short of the corner

A scrolling box is a rectangle inside a container that clips to a radius, so
the last stretch of the rail runs under the curve and is cut off. Of the three
obvious fixes only one works: a scrollbar is painted in its own gutter at the
box's edge and **cannot be moved inward**, and "move it up" is the same
operation as shortening it. So the TRACK is inset -- the thumb never travels
into the curved band, the content still scrolls end to end, and no layout
moves.

The inset is `var(--os-radius)` rather than a fitted number: a point further
than r from the corner in either axis is outside the curve by construction, so
it stays correct if a window is ever rounded more. A hand-fitted value would
silently start clipping again that day.

**Setting `scrollbar-width` or `scrollbar-color` switches Chrome to the
standard scrollbar, which ignores every `::-webkit-scrollbar` rule.** This is
the trap, and it is silent: the first cut applied both standard properties to
`*`, which made the whole `::-webkit-` section inert -- the inset thumb, the
transparent corner, the suppressed end buttons and the track inset -- and
Chrome drew its own bar with stepper arrows, the bottom one sitting in the
rounded corner. It looked deliberate, which is exactly why it survived a
design pass: a tinted, rounded rail is what you would expect those rules to
produce. The standard properties are therefore scoped to engines that have no
pseudo-elements, with `@supports not selector(::-webkit-scrollbar)`.

It was found by tinting `--os-rail` hot pink in a scratch harness and looking
at it. **If a scrollbar rule seems not to apply, tint it before theorising** --
the failure mode here is a rail that renders plausibly while none of your CSS
is running.

Firefox exposes no track geometry under `scrollbar-width: thin`, so its rail
is still clipped there. Known, and not worth a scripted custom scrollbar --
that trades a cosmetic clip in one engine for a scroll surface that behaves
like the platform's in none of them.

## Live surfaces: the arrival cue (the rule, not a suggestion)

Every live list in the OS renders through `kit/LiveList`, and the reason is
this cue. When a subscription changes something a person is looking at, the
change announces itself **once, quietly, and then it is gone**. It is never a
count, never a badge, never anything that waits to be dismissed -- an unread
marker turns news into a chore, and this is the opposite of that.

The mechanism is already built; what a new surface has to get right is what it
feeds it.

- **A new row rises and rings.** An updated row rings only. Both decay on the
  clock (`live/arrival.ts`, `TICK_TTL_MS`), not on the next data change, so a
  quiet list settles by itself.
- **The ring is a `box-shadow`, never a background.** Almost every row paints
  its own opaque plate, and a background animation underneath one is invisible
  -- it costs the same and announces nothing. This was live for a while and
  fired on nothing.
- **Reduced motion still gets a cue**, held for the life of the tick and then
  removed. "No animation" left those readers with no signal at all, which is a
  different failure from the one the setting asks about.
- **A HEARTBEAT IS NOT NEWS. This is the one that goes wrong.** The
  `fingerprint` prop decides what counts as a change, so anything named in it
  announces itself. Liveness fields -- `lastSeenAt`, `lastSeen`, `lastUsedAt`
  -- move on a timer for every row forever, and naming one turns the whole
  list into a strobe on a 15-second cycle: the standing badge this cue exists
  not to be. Fingerprint what a **person** would call a change (a rename, a
  revocation, a status flip, a label edit) and leave liveness to the thing
  that already displays it continuously, the dot.
- **A resync is not an arrival.** The reducer treats any snapshot following a
  non-live state as a baseline, so a reconnect does not re-animate the world.
  Re-baseline deliberately when a FILTER changes, by keying the `LiveList` on
  it -- revealing rows the browser already had is not the cluster sending
  them.

`test/fleet/machines.test.tsx` pins both directions: the cue fires on a
rename, and stays silent on a heartbeat.

## Fleet, the first real app (memql#4729)

`src/apps/fleet/` is the promotion of the foundation's read-only exemplar
into the whole app: **Machines** (rename, operator labels, revoke,
per-machine detail, add a machine), **Routing** (the policy editor and each
call's routing record), **Workbenches** (per-plan workspaces by replica),
and its own **Settings**. Four things about it generalize to every app epic
after it:

- **A live surface must be RETAINED.** A `LiveCollection` opens its
  subscription and runs its seed from `retain()` and from nowhere else;
  `subscribe()` only registers a listener. A surface that subscribes without
  retaining renders "Loading from the cluster" forever, with nothing thrown
  and nothing logged. `live/useLiveCollection.ts` is the one place that
  contract is honoured — use it rather than constructing a collection.
- **Check the routing rules before deciding a concept is dark.**
  `v1:worker:registration`, `v1:worker:routingPolicy` and
  `v1:workbench:workspace` carry explicit broadcast rules
  (`component/node/routing.go`); `v1:cluster:node` carries one through the
  `v1:cluster:*` wildcards in the same file. Only `v1:worker:invocation` is
  excluded, on volume grounds, so only the per-machine call history is an
  on-demand query that says when it was read. The first cut of Fleet got
  `v1:cluster:node` wrong -- reasoning from the absence of a rule with the
  concept's NAME in it, rather than reading the patterns -- and printed the
  mistake on the page as operator-facing copy.
- **Errors render in surface, never as toasts** (`fleet/ui.tsx`): a
  refusal is usually the server's own sentence and belongs beside the
  control that produced it.
- **Per-app settings** are their own versioned, sanitized store
  (`fleet/settings.ts`) rather than a corner of the desktop document, so an
  app learning a checkbox cannot cost anyone their desks.

## Users, the second app (memql#4733)

`src/apps/users/` is the People list, the invitations, and the three admin
actions the identity service exposes. Four things about it are the rules a
THIRD app gets wrong by default.

- **Read the routing rules for BOTH your concepts, and expect them to
  differ.** `v1:identity:user` carries a `created` broadcast and deliberately
  NO `updated` one -- the row churns on `lastSeenAt`, so broadcasting updates
  would strobe the mesh forever. `v1:identity:invitation` carries both,
  because an invitation is a human action. That asymmetry is the whole
  exemplar (an acceptance moves a row off one list and onto the other live)
  AND the whole cost: an admin action produces no event, so the detail panel
  re-reads **on open** and every write hands its accepted value back to the
  panel. Do not add the missing rule.
- **A query with NO shape projects every field of its concept.**
  `pendingUserInvitations` declared none, so it handed `tokenHash`,
  `previousTokenHash` and `bindingHash` to every admin browser that opened a
  people list. Before rendering a concept you have not rendered before, read
  what its shape actually projects -- and if it is credential-adjacent, add
  the narrow shape rather than ignoring the field. `authSessionAdminSummary`
  and `invitationAdminSummary` are what that looks like.
- **A server read and a browser read are not the same read.**
  the session read was filtered on its argument and nothing else, which is
  safe when the one caller passes the caller's own JWT `sub` and unsafe the
  moment a browser passes an id somebody clicked. It became two queries:
  `sessionsForSubjectAdmin` for the operator surface, gated and hash-free, and
  `authSessionsForSelfIncludingRevoked` for the revoke handlers, which now
  take no argument at all (memql#4768). Neither is a narrowing of the old one
  -- gating it would have refused the self-service revoke path for every
  non-admin in the cluster.
- **Promote on the second use, which is now.** Fleet's row, live view, clock
  and time formatters moved into `kit/` and `live/` rather than being imported
  across apps or copied. `.os-machine` and `.os-fleet` remain as CSS aliases,
  because the shared behaviour is what had to move. The measure that it was
  the right size: Users ships two classes of its own.

## Deployables, the third app (memql#4725, recomposed by epic memql#4885)

`src/apps/deployables/` is what this cluster serves for people, and the one
flow that composes it, deploys it and manages it afterwards. Three sections --
**Map**, **Deployables**, **Settings** -- where there were five: Actions, Sites
and Packages retired, because "create a deployable, give it an address, bind a
domain, tie it to a client" was four screens for one intention. The map is
untouched and stays first.

Ten things about it are new rules rather than repetitions of the apps before
it. The first nine are the compose epic's, and they are the ones the next
multi-step surface in this shell will get wrong by default; the tenth came out
of the Packages half this passage replaces and survived it unchanged.

- **THE RAIL IS THE FORM.** Composing a deployable, watching it deploy and
  reading its standing status are ONE vertical device, top to bottom: Source,
  What it is, Where it lives, Build, Live. Not a stepper beside a status
  panel, not a wizard modal, not numbered circles with Next and Back. The
  order is a law the pipeline enforces (`stage -> roll -> publish`, reversed
  on rollback), which is what earns a sequenced device at all -- normally the
  most over-used structure in software design. The Head's ONE action follows
  the state; the next unanswered stop is the open one; an earlier stop reopens
  on click. A stepper would have been a second device beside the rail, and a
  refusal at step four would page somebody back to step one to re-read it.

- **`railFor` READS THREE WAYS, AND EACH MODE READS DIFFERENT ROWS**
  (`page/rail.ts`, pure, no DOM; `page/Rail.tsx` draws the result).
  `deploy` is the six-stage reading over one `DeploymentRow`, reproduced
  exactly -- its tests moved into the mode unmodified, because the D6 rail was
  already right and a rewrite of a correct reading is a rewrite of its bugs
  back in. `standing` reads the target's five stops off the rows: the package,
  the NEWEST run whatever its status, and the site. `compose` reads the same
  five stops as INPUTS -- answered, being answered, not reachable yet, or
  parked by a refusal.

  Two consequences worth knowing before adding a fourth mode, because neither
  needed one. An IN-FLIGHT run is reported inside `standing`: the stop the run
  is at is `current` and every later stop is `ahead`, which is how "the same
  stops report progress during a deploy" is true with three modes. And a
  REFUSED run does not dim the stops after it -- a site that was live before a
  redeploy failed at Build is still serving, and a rail reading it as "not
  reached" would be lying about the one fact somebody came to check. The
  skipped-stage rule survives unchanged and is still the point: a prebuilt app
  draws Build skipped, "its built output is in the source", because a person
  counting missing steps cannot tell a fast deploy from a broken one.

- **A KIND'S SHAPE LIVES IN THE TARGET REGISTRY, NOT IN THE PAGE**
  (`targets.ts`). A target states its address stop, its build surface, its
  live states and its row, and every stop renders from it -- so the page has
  no branch on "which kind is this", and the Build epic changes what the Build
  stop SAYS rather than where it is. `web` is the only registered target;
  ios / android / macos are the design's table and not code, so nothing
  renders a control for them. **`OFFERED_KINDS` is a single-line literal on
  purpose**: `component/memql/site_kind_os_parity_test.go` reads it out of the
  file and holds it equal to `v1:platform:site.kind`, the way
  `TestFleetOnlineWindowMatchesPortal` holds the online window equal across
  client and engine. A kind the OS offers and the enum does not is a form
  somebody can fill in and nothing can store.

- **THE HEAD HAS ONE ACTION AND IT FOLLOWS THE STATE** (`page/head.ts` names
  the state, `headActionFor` in `page/rail.ts` answers it): Analyze, Deploy,
  Make it live, Deploy the update, Retry, Redeploy -- plus the quiet Ask and
  Open beside it. **Null is a statement, not a gap.** A system-owned row, an
  archived deployable, an archived source and a reader each get NO lifecycle
  control rather than a disabled one, for the reason this app already gave
  once: six greyed-out buttons are six controls somebody has to read past to
  learn they are not for them. A run at a non-terminal stage also gets none --
  a button beside a moving rail is a button competing with it.

- **THE LIST IS ONE `LiveList` OVER THE SITE FEED, JOINED CLIENT-SIDE TO THE
  PACKAGE FEED.** One row per thing that serves or will, so a package with two
  apps is TWO rows sharing a source, grouped under it -- not one row somebody
  has to open to discover it is two things. Each row carries the standing rail
  in its `compact` form: the same five marks, no labels, each carrying its
  label as its accessible name. A row is read as the shape it opens into.

- **THE PARKED-RUNS FEED IS A FOURTH FEED AT THE APP ROOT, AND IT IS THE ONE
  RECORDED EXCEPTION** to the rule this app's own Packages half wrote down: a
  deployment TIMELINE is retained by the page and never by the root. That rule
  guards against subscribing a window to every deploy in the cluster to render
  one row, and it still does. `packageDeploymentsAwaitingConfirm` is parked
  runs ALONE -- a handful of rows, and rows a person needs to see BEFORE they
  open anything, because the whole point of a gate that lives on the row is
  that somebody who closed the window finds their deploy where they left it.
  The list marks that row "a deploy is waiting for you". **Any other timeline
  feed at the root is the thing the rule forbids**, and the exception's code
  comment cites this passage rather than restating it.

- **A CREDENTIAL IS A CARD, AND THERE IS NO TYPE THAT COULD HOLD THE VALUE**
  (`sources/rows.ts`, `sources/useSourceCredentials.ts`). `CredentialRow`
  projects label, host, fingerprint and status; it has no field for a token,
  so a row that carried one would be dropped here rather than rendered. There
  is no chip, fact or tooltip in this app that could show a value.
  **`lastUsedAt` is the heartbeat rule's second sharpest case** after
  `lastCheckedAt`: every fetch of every source under a credential writes it,
  the ten-minute poll feed included, so naming it in the arrival-cue
  fingerprint would ring the card on a timer for as long as anything tracks a
  repository. The fingerprint is label, status, host and fingerprint;
  `lastUsedAt` and `revokedAt` are displayed and never rung.

- **A STOP OWNS ITS OWN REFUSAL** (`page/stops/*`, one component per stop).
  Every refusal renders AT the stop it belongs to -- the OS's headline above
  and the server's sentence beneath, verbatim -- and the rail marks that stop
  stopped and every later one unreached. A refusal with no known code renders
  under a NEUTRAL heading with the server's sentence alone, never under a
  guessed one: `packages/refusals.ts` keys copy by code, and
  `test/deployables/refusals.test.ts` reads `component/packages/refusal.go`
  AND every inline code raised across that package, so a code the engine can
  emit and this build has no name for fails the build rather than reaching a
  browser unnamed. No toasts, no dialogs, no `window.confirm` -- a refusal
  inside a modal that then closes is a refusal nobody can re-read.

- **THE SOURCES GROUP LIVES IN THIS APP'S SETTINGS, NOT IN THE SHELL'S**
  (`settings/SourcesGroup.tsx`). A source credential is this app's own record
  -- the person's token for the repositories THEY deploy -- rather than a
  cluster credential an operator rotates from a shell, which is the same line
  Campaigns draws between its sending identities and Settings -> Integrations.
  Revoking says what it will cost before it is done: sources fetching under it
  will refuse at their next fetch until you switch them. Rotation is adding a
  credential and repointing the source on its Source stop; there is no
  in-place replace, because a replace would change what a source fetches under
  without a row saying so.

- **A CUE AND A STANDING MARK ARE DIFFERENT STATEMENTS, and the update needs
  both.** The arrival ring says "this just changed" and decays on the clock;
  the update chip says "there is a newer version than the one you are running"
  and stays until somebody deploys. A cue alone would make the news visible
  only to whoever happened to be looking. A chip alone would make it arrive in
  silence. `updateAvailable` is named in the fingerprint DESPITE being written
  by a ten-minute poll, because the engine's feed only writes it when the
  upstream actually moved -- so a flip is news by construction rather than a
  heartbeat.

The rules the app was built on in memql#4725 all still hold, and three are
worth restating because the recomposition tested each of them:

- **ONE FEED PER CONCEPT, RETAINED AT THE APP ROOT.** The list, the map and
  the page are readings of ONE retained `LiveCollection` of sites, and a
  second `useSites()` inside the map would open a second subscription and run
  a second seed, free to disagree with the first about what the cluster holds
  -- the one failure an app that is a picture and a table of the same thing
  must not have. **The rule is per CONCEPT, not per app**: sites, packages and
  credentials are three concepts and three feeds, and three concepts cannot
  disagree because they describe different things. The parked-runs feed above
  is the fourth, and the reason it is written down is that it is the only one
  that needed an argument.
- **`v1:platform:site` BROADCASTS BOTH created AND updated**, unlike
  `v1:identity:user`, which broadcasts creates and deliberately not updates
  because the row churns on `lastSeenAt`. That asymmetry is what makes the
  headline true with no engine work: a CI publish through
  `POST /sites/{id}/bundles` flips `bundleRef` on a node nobody in the browser
  is talking to, and the row changes under the person watching it. The same
  went for this epic's rows -- `v1:platform:sourceCredential` needed broadcast
  routing rules in `component/node/routing.go` before the Sources group and a
  Source stop's credential chip could be live. Read the ROUTING RULES before
  deciding what a concept's live feed does.
- **NOTHING IS INSERTED LOCALLY.** A created site, a new credential and a new
  source each arrive on their own broadcast with the arrival cue, exactly like
  one somebody else created. The Source stop marks a `bundleRef` flip because
  the VALUE changed, not because a tick fired -- an `updated` tick fires for a
  rename too, and a marker driven by it would announce a publish that did not
  happen.

Two things the app deliberately does not do, both inherited and both still
true. There is no raw `bundleRef` editor anywhere (a field that accepts any
URI is a way to point a live site at nothing). And a `systemOwned` row renders
NO lifecycle controls at all: the seeded portal and OS sites are exempt from
the lifecycle entirely, the server refuses those writes regardless
(`component/memql/platform_site_status_guard.go` is the gate), and the
presentation is the courtesy.

### The recomposition (epic memql#4937), and the four rules it left behind

Design record:
[2026-09-04-deployables-recomposed-design.md](../../docs/superpowers/specs/2026-09-04-deployables-recomposed-design.md).
The rendered pass that accepted it, and the four defects it caught with the
whole suite green:
[2026-09-04-deployables-recomposed-visual-qa.md](../../docs/internal/ops/2026-09-04-deployables-recomposed-visual-qa.md).

The app above was measured in a browser on a live cluster and found to carry
**5,069px over 5.9 viewports** on one deployable page: two stacked `os-head`
elements, thirteen rails at three different meanings, 36 controls -- with
Pause at y=2412, Archive at 2499, and "archive this source **and every app it
produced**" at y=885, three sections higher than either. Six controls read
"Retry", carrying two different promises. Four rules came out of fixing it.

- **A LIST AND ITS DETAIL NEVER SHARE A SCROLL COLUMN** (DESIGN.md rule 11).
  This app held `selectedSiteId` and rendered `<DeployablePage>` as a SIBLING
  of `<LiveList>`, so selecting a row appended a whole page beneath the list.
  Every other app already did better -- Bin puts the detail beside the list in
  its own scroller, and Campaigns, Users, Accounts and Training do variants --
  so this was the one app that had not adopted the pattern the shell already
  had. It is four sibling views now (list, deployable, source, history) plus
  compose, one at a time, one Head each. **The Map is not an exception**: it
  used to render the same 5,000px page under the picture and now shows a card
  with the way in.

- **ACTS FOLLOW THE STATE, IN ONE PLACE** (rule 12, `kit/ActionBar`), and the
  decision is a PURE function (`page/acts.ts`) so the table is what gets
  asserted. **An illegal act is ABSENT, never disabled** -- that is not a
  preference, it is the bug: a draft rendered an ENABLED "Archive this
  deployable" that `validateSiteStatusTransition` refuses, and had no control
  anywhere that could reach the `disabled` state that guard demands. The only
  lifecycle control a draft offered was one the engine rejected.

- **A SOURCE IS A THING, WITH ITS OWN PAGE.** The credential, the auto-deploy
  switch, the run history and the archive-the-source cascade used to render
  inside the Source stop of EVERY app the source produced. Two consequences,
  both measured: `usePackageDeployments` reads the PACKAGE's timeline, so a
  two-app source drew the identical 2,600px history TWICE; and the cascade --
  which archives every app -- sat 1,614px above the page's own archive, on a
  page about one of its siblings. A control that destroys a sibling, easier to
  reach than the one that destroys what you are looking at, is not a layout
  problem.

- **THE STOP THAT IS OPEN IS THE ONE THAT IS A QUESTION** (`openStopFor`,
  pure). A settled stop is one line -- mark, label, its answer, a chevron. A
  run in flight opens the stop it is at; a REFUSED run opens the stop it
  stopped at, which is the case that carries the reading: opening Live to say
  "the build failed" sends somebody to repair the wrong thing.

- **A RUN BELONGS TO THE APPS IT NAMES** (memql#4953, `runCoversApp` /
  `runForApp` / `siblingRunInFlight`). `usePackageDeployments` reads the
  SOURCE's timeline, and the page used to take `rows[0]` off it -- the newest
  run of the whole package, whatever deployable it was about. Everything then
  derived from the wrong run: a serving `storefront` read "Building" while
  `web` deployed, drew its own later stops as `ahead`, rendered the sibling's
  report and refusal as its own, and offered a Cancel that killed `web`'s
  deploy from a page about `storefront`. A run records `scopedTo` now -- EMPTY
  meaning the whole source, which is what every row written before it was --
  and the page, the list's compact rail and the parked-run mark all ask.

  **The half that is easy to lose while fixing it**: the wrong reading was the
  only thing stopping two concurrent runs of one source. There is no gate for
  that in the engine, and a roll rewrites one pointer and restarts the cluster
  onto it. So the page keeps its own state and its own words, and the acts that
  would START a run are absent -- not disabled -- until the source is free,
  with the line that already explains the state explaining that too. Removing a
  wrong reading can remove a right behaviour that was leaning on it.

Two smaller ones worth knowing. **`transform` does nothing to a non-replaced
INLINE element**, so a disclosure chevron needs `display: inline-block` or it
silently never turns -- jsdom cannot see it, and it reads as a control that
does not respond. And the bar is a **grid row, never `position: fixed`**: the
desk plate is CSS-transformed and becomes the containing block for any fixed
descendant, which is the same trap the Logs jump pill records.

### One vocabulary, activation, and a source archive that frees its names

Design record:
[2026-09-05-deployables-states-activation-and-source-archive-design.md](../../docs/superpowers/specs/2026-09-05-deployables-states-activation-and-source-archive-design.md),
written from the owner's walkthrough of the app on a live cluster.

**EVERY STATE WORD COMES FROM `words.ts`**, and there are seven: Inactive,
Not deployed, Built, Live, Offline, Archived, Deleting. The bar, the list's
chip, the source page's app list, the rail's Live stop and the compose flow's
end all read it. The enum did not move (`live` reads Live, `disabled` reads
Offline, a `draft` with real files reads Built and one with the placeholder
reads Not deployed). `Published`, `Unpublished`, `Publish`, `Unpublish`,
`Make it live`, `Paused` and `off` are gone from every surface: after Deploy
the bar read "Deployed", then "Published 1 app", then "It is not serving yet",
and Deploy sounded final while Published sounded live. The verbs are **Go
live** / **Take offline**, **Activate** / **Deactivate**, **Archive** /
**Restore**, **Delete** (and *Discard* for a draft) -- one name, one promise.

**A SOURCE'S APP IS NEVER ARCHIVED; IT IS DEACTIVATED.** A standalone
deployable climbs the D10 ladder (`Offline -> Archived`, the name kept, then
`Delete`, the name released), because its bundle is its only copy. A source's
app is reproducible from the source, so it has ONE destructive act from every
state, live included: `packageDeactivateDeployable` releases its custom
domains, deletes its site (the write the uniqueness probe reads, so the address
is free at that instant), disarms auto-deploy when it was the source's last
app, and puts the name on the source's off-list. The confirmation is the APP'S
NAME, not its hostname -- the address is generated now and is not what a
person calls the thing. `acts.ts` decides which ladder a row is on from
`packageId`, not from `pkg`: an app of an ARCHIVED source has no package in the
active feed and is exactly the row whose name still needs freeing.

**ARCHIVING A SOURCE DEACTIVATES EVERY APP IT PRODUCED, THEN ARCHIVES THE
SOURCE.** The cascade used to archive each site, and an archived site holds
its hostname, so a re-added source could not take its addresses back. It no
longer refuses while an app is live: the confirmation on the source page names
the live addresses and says they go offline the moment it is confirmed.
`packageRestore` still does not cascade; the apps come back inactive.
`package_has_active_deployables` is retired.

**SKIP IS DEACTIVATE, AND A CLICK NEVER ACTS.** An app skipped at the gate
lands on the off-list as before; what changed is the click. It used to turn the
app back ON with no page and no confirmation, and the next click started a
deploy. Every declared row -- inactive or not, in the list or on the source's
page -- now opens the compose flow scoped to that app: an inactive one with a
notice at the top and **Activate** on the bar, a merely-declared one with
Analyze. Nothing is written until the bar's act is pressed. The scope reaches
the FORM now (`appsToPlace(..., only)`), not only the wire: opened for `web`,
Where it lives used to ask about `storefront` too, with a Deploy/Skip pill on
each and "2 of 2 apps" on the bar.

**THE ADDRESS IS GENERATED, AND CHECKED WHILE IT IS TYPED.** The seed is the
Generate button's own draw (`seedAddress`), because every source declares
`storefront` and `web` and the second person on a cluster found them taken at
the end of the flow. Two engine-native reads, `siteHostnameCheck` and
`customDomainCheck`, run the write guards' own shape and uniqueness rules
without reserving anything; `useAddressChecks` asks after a 350ms pause, the
line under the field reads "checking", then "free" or the policy's own
sentence, Generate draws again until a free name comes back, and Deploy is out
of reach until every address going out has checked out
(`placementsComplete(..., verdicts)`). The guard still decides on the write.

**ONE SOURCE, ONCE.** A write guard beside the hostname policy
(`platform_package_source_policy.go`) refuses a `v1:platform:package` create
naming a repository and ref another ACTIVE package already tracks, normalised
(scheme, `.git`, trailing slash, case, the SSH form); archived packages do not
count. The Source stop asks the same question off the packages feed
(`duplicateSource`) and, knowing the probe's default branch, reads "" and
`main` as one ref there, which the engine cannot.

**THE FLOW ENDS ON BUILT, WITH GO LIVE BESIDE DONE.** A first deploy leaves
the site `draft` with real files, deliberately; the bar says Built in the one
vocabulary and offers Go live right there, so the two-step is one screen.

**A CONFIRMATION IS THE THING'S NAME.** A standalone types its address label
(`storefront`, not `storefront.memql.<domain>`), and the server's
`confirmationMatches` accepts the label or the whole hostname. A source's app
types its manifest name; a source types its own.

### A run can be stopped, up to the roll

`cancelled` is a fifth terminal status on `v1:platform:packageDeployment`, and
it is **not a flavour of `failed`** for the reason `abandoned` is not: nothing
broke and nothing was published. The distinction between the two is WHO
DECIDED -- abandoned is a loss the cluster OBSERVED, cancelled is a choice
somebody MADE, and collapsing them reports a person's own click back to them
as a fault.

The verb FLAGS the row and ends nothing; the node running the attempt closes
it at its next stage boundary, so the timeline can never claim a run stopped
while its build is still going somewhere. The flag rides the HEARTBEAT rather
than a poll of its own, which is what lets a long single stage -- the build --
learn about a cancel within one beat. **The last checkpoint is immediately
before the roll**: from `staging_dsl` on, a roll restarts the cluster onto
staged MemQL, and stopping half way through is worse than either finishing or
not starting, so the ASK is refused there rather than setting a flag nothing
will read. A PARKED run is the one exception the capability closes itself --
nothing is running, so nothing would ever read the flag, and leaving it would
let the abandoned sweep blame the cluster for a person's decision.

### A filter re-baselines through `useLiveView`'s key

Revealing rows the browser already had is not the cluster sending them, so the
Archived flip must not fire the arrival cue for every newly-visible row.
`viewKey` is where a re-baseline is expressed (`live/liveView.ts` rebuilds on
it), and it is one line rather than an unmount.

### The map is plain SVG, and that is enforced

The portal's Nexus is the platform's ONE 3D surface and pays for it with a lazy
chunk and a guard of its own. This map answers a flat question -- which host,
which site, which bundle -- so a WebGL renderer would buy it nothing while
making every OS window carry the largest dependency the portal has.
`test/deployables/map.test.tsx` scans the module graph for a three.js import
AND checks the package manifest, because a static import is only one of the two
ways one gets in. Both halves carry the reachable positive that makes an empty
offender list evidence about the tree rather than a statement about the regex.

`map/layout.ts` is a pure function from rows to positioned nodes and edges, and
`map/viewport.ts` is the pan/zoom arithmetic -- both fixture-tested with no DOM
and no GPU, which is the Nexus precedent for purity without its renderer. Two
of their rules are worth knowing before editing them:

- **The layout sorts its own input.** The collection folds events in the order
  the cluster sent them, so a map that depended on input order would reshuffle
  on an update -- exactly when somebody is watching it.
- **A shared bundle is ONE node with two edges into it**, deduped within its
  domain group and centred on the bands it serves. That is the fact the picture
  carries and a table cannot. It is deduped per GROUP rather than globally
  because an edge running the width of the canvas between two domains costs
  more legibility than the fact is worth -- a bundle serving sites under two
  domains is drawn once per domain, which is also a true reading of it.
- **Zooming keeps the point under the cursor under the cursor.** It is two
  lines of algebra and it is written down once, in `zoomAt`, rather than
  re-derived inside a pointer handler. `touch-action: none` on the canvas is
  load-bearing and invisible when it goes: without it a finger scrolls the
  window and the gesture simply does not exist on a phone.

### The write half

The app itself carries no role. `v1:platform:site` declares the composite tier
(`@rowAuthz(owner="ownerUserId", clusterOwner)`), so every signed-in person has
deployables of their own to read and the engine decides how far the list
reaches. What is gated is presentation over writes the Go hostname policy,
`sitePublishFromArtifact` and the pipeline remain the authority on: rank 200
and above for the write controls, and the OWNER rung for a client's own domain
and a CI-pushed source.

- **The slug rules are mirrored for a keystroke-rate answer, and say so.**
  Cluster-wide uniqueness and the cluster-owner exemption are deliberately NOT
  mirrored -- a browser cannot answer either -- so those refusals arrive from
  the server and render verbatim, because the server's sentence names the
  colliding site and a friendlier paraphrase would drop the one fact that
  helps.
- **A placement is one write, not three.** `packageDeploy` takes
  `{hostname, accountId, ownDomain}` per app and the pipeline applies the two
  optional halves itself, under the caller's actor, as the same calls this app
  makes. So there is no client-side follow-up write to get half-done, and a
  refused domain lands on the deploy's own outcome for the Where-it-lives stop
  to render rather than as a second failure somewhere else on the page.

### Custom domains: the Where-it-lives stop (memql#4805)

`page/stops/Domains.tsx`, mounted as the deployable page's Where-it-lives stop
for a cluster owner (epic memql#4885), is a client's own domain bound to a
site -- the add flow, the two DNS records to create, the live typed status, and
the remove. It was a panel of its own before the recomposition, and moving it
onto the stop is the whole point of the stop: the address, the client and the
domain are one question asked in one place. Three things about it are worth
knowing before touching it.

- **The surface is TWO RECORDS AND WHAT WE SEE AT THEM.** Somebody reading this
  is a tab away from a registrar's form whose fields are called Type, Name
  and Value, so a record renders as exactly those three parts, in that
  vocabulary, each separately copyable -- a single "copy record" button would
  hand them a line they then have to take apart. Beneath it sits the server's
  `failureDetail` verbatim: the typed reason says WHICH record is wrong and the
  detail says what is IN it, and somebody editing a zone file needs both. That
  pairing is the whole design.
- **`lastCheckedAt` is the heartbeat rule's sharpest case.** The sweep touches
  every non-terminal binding every two minutes forever, so naming that field in
  the arrival-cue fingerprint would strobe the list on a two-minute cycle. The
  fingerprint is `status | failureReason | failureDetail`; the timestamp is
  displayed continuously instead, which is the right home for something always
  true and never news. `test/deployables/domains.test.ts` pins both directions.
- **A STEPPED RAIL IS HONEST HERE, which is unusual.** The four stops are a real
  sequence a binding cannot skip, so the order carries information -- "the
  records check out, we are waiting on a certificate" is a different situation
  from "the records are wrong". `removing` / `removed` are deliberately OFF the
  rail: they are a different journey that can start anywhere, and a fifth stop
  would say a removed domain had got further than a live one. It is a rail of
  its own inside a stop of the deployable's rail, and that is fine for the same
  reason the deploy rail is: the sequence is the binding's own law.
- **There is no re-check button anywhere, and the stop says why.** Retries ride
  the sweep's schedule (design D5) -- a button would invite hammering a
  recursive resolver and an ACME endpoint. An absent control with no account of
  itself reads as something somebody forgot to build, so the footer says what
  DOES happen rather than apologising for what does not. The deploy does not
  wait on any of it: the app is live at its cluster address and the domain
  stays "waiting on your DNS records" beside it.

### Builds, and a run that gets lost (epic memql#4900)

Builds are real now, so three things the surface says are rules rather than
repetitions of what the app already had.

- **A HEARTBEAT ARRIVED, AND THE FINGERPRINT DID NOT MOVE.** A running deploy
  writes `heartbeatAt` every fifteen seconds, and every one of those writes
  broadcasts the whole row. `deploymentFingerprint` is still `status` alone,
  which is this app's arrival-cue rule at its sharpest: the cue would fire
  hardest for the run somebody is already watching move. `autoDeploy` goes the
  OTHER way, into the PACKAGE fingerprint -- it is a field somebody else can
  flip, and the consequence is that pushes start deploying themselves.

- **`abandoned` IS NOT A FLAVOUR OF FAILED.** A run whose node the cluster lost
  gets its own terminal status, its own word in the timeline ("lost", never
  "failed"), and copy saying nothing failed and nothing was published. The
  natural reading of a stopped deploy is that it broke; this one did not, and
  the surface's job is to say so before somebody debugs a build script that is
  fine. Two readings came out of a BROWSER rather than jsdom, which renders the
  same DOM and asserts nothing about either: a lost run was appearing under
  "Deploying now" beside a rail that had stopped, and the rail was drawing it
  as having stopped at Analyze because the evidence rule reads fields a stage
  WRITES and a run that dies part-way writes none of them. The fix for the
  second is `stoppedAt` on the row, and it outranks every inference.

- **RETRY AND REDEPLOY ARE DIFFERENT PROMISES, so they are different buttons in
  different places.** The Head's Retry deploys the source again. Retrying a
  LOST run deploys the bytes that run had already fetched, so it lives on the
  attempt that names the run and nowhere else -- two controls both reading
  Retry and doing different things is the thing being avoided. The auto-deploy
  switch is on the SOURCE stop for the same reason: it answers "when this
  source moves, then what", which is a property of the source rather than of
  any one run.

## Training, the fourth app (memql#4737, re-keyed to the Library in memql#4970)

`src/apps/training/` is teaching MemQL from files: a dropzone into the LIBRARY,
a worklist of the caller's files moving through the analysis, an explicit act
that teaches a knowledge domain from one, a review queue over what that
produced, and a browser of the domains it feeds.

**THE APP NAMED TRAINING NEVER TRAINED, until the re-key.** It uploaded into
the caller's daily cognition space, and the completion path there writes a
`v1:knowledge:document` through `mutationCreateDocument` -- a mutation declared
in no `.memql` file in this tree, with its error swallowed at the call site
(`component/server/plan_store.go:244`). So an upload produced a summary and no
knowledge chunks, and the review queue could only ever show chunks some other
path had written. Nothing on screen said so: a plan reached `succeeded` and the
queue was empty, which is a completely plausible answer.

Keyed to the Library the loop is real, and it is TWO ACTS rather than one: a
file lands as `v1:library:file` and the analysis pass reads it into fileChunks,
and then `libraryTrainFile(fileId, domainId)` ingests those into a domain as
`documentChunk` rows with `source: "fileUpload"` -- which is exactly what the
review queue is for. The surface says so, because hiding the second act is what
made the first one look like it had done something.

**The section is a WORKLIST, not a log.** Every file is a row that says where it
is and offers exactly the act legal from there -- rule 12 applied per row,
because here the state is per row. `unreadable` (a photograph, a zip) offers
NOTHING: no act would make it teach something, so a disabled Teach beside it
would be a control whose only purpose is to be refused.

**THE FILE LEADS AND THE RUN DECORATES.** Two live feeds, and the ordering
matters: the upload route writes the file row synchronously inside the request,
and the analysis pass writes `v1:work:run` from a detached goroutine on
whichever node took the upload. A surface that waited for both would show
nothing for the first moments of every upload. The run earns its place by
carrying what the file row cannot say -- twelve passages of which nine are
searchable, or a photograph with nothing in it, both of which end at `ready` on
the file row.

**The client-side owner filter is GONE**, and that is a security gain rather
than a simplification. `v1:planner:plan` declares no row-authz tier, so its
subscription admitted every subscriber and other people's plans reached this
browser to be filtered here. `v1:library:file` and `v1:work:run` both declare
the composite owner tier, so admission runs on the subscription too
(memql#4309) and they never arrive.

Four more things about it are new rules rather than repetitions of the three
apps before it.

- **A CONCEPT FIELD IS NOT A READABLE FIELD, and the omission is silent.**
  `documentChunk` declares `validationStatus`, `source`, `documentId`,
  `superseded` and the rest; `documentChunkFull` projected none of them. A
  review queue built against that read would have selected on a key no row
  carried, found nothing, and rendered "nothing awaiting review" against a
  cluster full of unvalidated chunks -- an empty list being a completely
  plausible answer is what would have made it survive review. Before rendering
  a concept you have not rendered before, read what its SHAPE projects, not
  what its concept declares. Two shapes grew here, and the growth is the
  feature: `documentChunkDomainLite` gained `validationStatus` so ONE
  `allDocumentChunkDomains` pass yields a complete per-domain rollup, instead
  of a per-domain page walk that counts the first fifty and calls it a total.

- **NOT EVERY FEED IS LIVE, and the honest move is to say which.**
  `component/node/routing.go` carries broadcast rules for `v1:planner:*`, so
  the analysis list is live with no engine work: the attachment handler stamps
  a queued Plan and finishes on a detached goroutine, and the transitions land
  under the person watching. It carries NONE for `v1:knowledge:*`, so the
  chunk surfaces are on-demand reads that print when they were read and re-read
  on window focus. A `LiveList` over the knowledge side would render "Loading
  from the cluster" and then a list that silently never moved -- worse than a
  plain one, because the caption would be claiming wiring that is not there.
  Adding the missing rule is not this app's call: chunk writes are
  high-volume, which is the same ground `v1:worker:invocation` is excluded on.

- **A hosted surface reaches a bff-root route through the marker, and the
  marker had to learn this one.** The OS is served by `component/edge`, so a
  bare same-origin POST to `/spaces/{id}/attachments` resolves to no file in
  the bundle and takes the SPA fallback: index.html, 200, an upload that
  stored nothing and said it worked. `upstreamPath` strips `/_memql` for the
  bff's own roots, and `/spaces` joined `/artifacts` there (memql#4738). The
  transport names that failure explicitly when it happens anyway -- an HTML
  body means the site answered, not the cluster.

- **A WINDOW SITS INSIDE THE DESK PLATE, AND THE DESK PLATE TAKES FILE
  DROPS.** `Desktop.tsx`'s `onHostDrop` turns a dropped file into a Library
  artifact and a desk icon, and a `WindowFrame` renders inside it -- so an
  app's own drop target must `stopPropagation`, or ONE file is uploaded TWICE,
  to two different places, and the second upload is one nobody asked for.
  Stop it on `dragover` too, and stop it even when the target is DISABLED:
  otherwise the desk's own dragover allows the drop and dropping a file on a
  visibly-disabled control produces a desktop icon, which is a stranger answer
  than nothing happening. "Nothing happens where nothing is offered" is the
  same rule the right-click section states.

- **An app that uploads somewhere the shell's provider does not point at
  builds its own, from a CAPABILITY.** `items/upload.ts`'s `UploadHandle` is
  generic over its result now, because the desk's drops return a Library
  artifact and this one returns an attachment; the whole progress /
  in-surface-failure / retry vocabulary is written against that shape, so the
  alternative to a type parameter was a second copy of it. The bearer comes
  from `auth/context.tsx` -- a context handing out `bearer()`, never the
  string, which is the rule `auth/source.ts` already stated and had no way for
  an app to honour.

### What it deliberately does not do

The owner's scenario for this app is entity-level -- "MemQL identified a new
customer, should I add it?" -- and the concepts that would carry it
(`domainEntitySchema`, `entityIndex`, `validationEvent`) are declared in no
`.memql` file in this tree. So the reviewable unit is the CHUNK, which is what
`validationStatus` is actually attached to, and the app says so on the page.
Likewise a domain card is labelled by its `domainId` because
`v1:knowledge:knowledgeDomain` is product-owned and has no list query at all --
that is the truth this engine can tell, not a placeholder. Attaching a domain
to an agent rides `skill.domainIds` and lands with the Agents surface; the
affordance is present, inert, and says where the feature went.

## Files, the fifth app (memql#4721)

`src/apps/files/` is the Library on the desktop: a live folder tree over the
content-bearing rows, an inspector that leads with each file's provenance
story, and production transfers. It replaced the last stub, so `StubApp` and
the `stub()` helper are gone. Five things about it are new rules rather than
repetitions of the four apps before it.

- **THE SEED IS PICKED FOR THE FILTER THAT CANNOT BE SERVER-SIDE.** The
  browse seeds from `libraryArtifactsByLens(lens: "artifact")`, not
  `libraryArtifacts`, because the default read carries `archived != true` and
  a show-archived toggle over it could only ever show the rows that flipped
  while the window was open. The facet read deliberately carries no archive
  conjunct, and the artifact lens IS this app's population (file / document /
  generated_output), so one paged seed holds the complete truth and every
  filter -- archived, kind, source, search, folder scope -- stays a
  client-side fold. `folderId` filtering must be client-side anyway: a row
  promoted before folders existed has no member at all, and only the fold
  reads absence and the empty string as the same answer, the root.

- **ONE UPLOAD PATH, PINNED.** Every surface -- desk drop, upload button,
  drop-onto-window, drop-onto-folder -- rides `items/edgeUpload.ts`:
  one-shot at or under 32 MiB, chunked resumable sessions above, resume by
  re-drop from a 7-day localStorage ledger, refusals verbatim.
  `test/files/onePath.test.ts` fails the build on a second call site, with
  the provider itself as the reachable positive.

- **OPENING A DESK FOLDER IS OPENING THE FILES APP** (epic memql#4842,
  reversing the foundation's under-icon popover). Double-click, Enter, or
  the menu's "Open in Files" lands the window on that folder under the
  Desktop place, carried by the shell's open intent -- fresh window and
  already-open window alike. The desk stays subscription-free by
  construction now: no desk gesture opens a feed, and the one folder
  surface is the app, so the projections and the cue contract cannot fork.

- **AN ACTION THAT ANSWERS COMPUTES BEFORE IT APPLIES.** The desk's
  `sendFileToDesk` / `sendFolderToDesk` / `placeFolderShortcut` return
  placed / focused / full, computed from a state ref and then applied. The
  older "let outcome; set(updater); return outcome" shape reads its answer
  from React's EAGER updater evaluation, which runs exactly when the fiber's
  queue is empty -- the first call answers correctly and the second answers
  "full" against a desk with one file on it.

- **A RAW CONTROL BYTE IN SOURCE MAKES THE FILE INVISIBLE.** The tree
  walker's path separator (`TREE_PATH_SEP`, uploadTree.ts) is written as the
  six-character escape for U+001F, never pasted as the byte itself: a
  literal control byte turned the file binary to grep and to every
  repo-walking gate while its tests stayed green. `uploadResume.ts`'s own
  key separator was the second one to be found this way and is now written
  the same, as the escape for U+0000.

- **A NEW VERSION IS ONE ROW, NOT TWO** (epic memql#4806). "Upload new
  version" in the inspector sends `targetArtifactId` through the SAME
  provider every other upload rides -- so chunking, resume, retry, progress
  and verbatim refusals all apply to it and nothing new learned any of them
  -- and the artifact keeps its id, its folder and its labels. It
  deliberately does not go through `useUploadTasks`: those create a
  placeholder ROW in the list, and a new version must not add a second row
  to the list it is proving it does not disturb. The history lives in the
  inspector below the action that grows it, and is a READ rather than a
  feed: `v1:library:fileVersion` carries no broadcast routing rule, so the
  panel says when it looked and offers to look again instead of implying a
  liveness it does not have.

## Accounts, the sixth app (memql#4800)

`src/apps/accounts/` is the client registry: the companies this instance does
work for, and what of the cluster's is theirs. It is the first app whose
subject is not something the cluster owns, and four things about it are new
rules rather than repetitions of the five before it.

- **A TIE SURFACE BELONGS TO THE DOMAIN THAT OWNS THE CONCEPT, NOT TO THE
  KIT.** Four other apps render and edit an account tie -- Deployables,
  Files, Users, Training -- and every one of them imports `AccountPicker` and
  `useAccountOptions` from `apps/accounts/`. That is deliberately not a
  promotion into `kit/`: the kit is the OS's shared VOCABULARY (rows, chips,
  notices, the live list), and a picker over one domain's concept is not
  vocabulary. Putting it there would make every app depend on a concept most
  of them otherwise know nothing about. `useAccountOptions` opens ONE
  COLLECTION PER MOUNTING COMPONENT, so five surfaces mounting a picker at
  once open five subscriptions -- `live/useLiveCollection.ts` memoises on
  `[connection, key]` and constructs a collection per component; it does NOT
  call the SDK's shared `LiveRegistry`. That is accepted ACROSS apps and is
  not accepted INSIDE one, and the difference is what the two feeds decide:
  two readings inside one app would be free to disagree while deciding
  whether a form or a list renders, which is why `AccountsApp` retains
  exactly one and passes it down. `apps/accounts/tie.tsx` is the long form of
  this and is the account to trust -- this bullet claimed the opposite until
  memql#4827.

- **THE LEDGER IS AN ON-DEMAND READ, AND ALL FOUR BANDS ARE, DELIBERATELY.**
  Three of the four rolled-up concepts DO broadcast (`v1:platform:site`,
  `v1:library:artifact`, `v1:identity:invitation`), so a live ledger is
  buildable. `v1:knowledge:knowledgeDomain` carries no rule and nothing
  broadcasts it, so the fourth band could never be live -- and a ledger where
  three bands move and the fourth silently does not is worse than one where
  none do, because the reader has no way to tell which kind of band they are
  looking at. So all four are read together, print when they were read, and
  re-read on demand. Consistency across a composite surface beat liveness on
  part of it.

- **A REFUSAL IS NOT A ZERO.** `invitationsForAccount` carries
  `requiresOwnerOrAdmin`, so below that floor the engine refuses the read.
  The band renders "Not yours to read" plus the server's own sentence, and
  never a count -- a `0` there would be this window inventing a fact about a
  client. Each band settles on its own for the same reason: one
  `Promise.all` would let the one refusal that WILL happen decide the state
  of three reads that succeeded.

- **THE FIRST-RUN CARD IS GATED ON A ROW, NOT ON A FLAG.** It renders when
  `v1:accounts:account:self` carries no `configuredAt`, read off the feed the
  list already holds rather than through a second by-id read -- one source of
  truth for the row that decides whether a form or a list renders. Saving is
  an ordinary `updateClientAccount`, which stamps the field, so the answer
  lives in the cluster and the card is gone for everybody at once. Nothing is
  remembered in this browser, and no other OS surface gains a prompt: a
  first-run question that ambushes somebody mid-task is one they dismiss, and
  a dismissed question needs somewhere to be remembered.

  While the feed is still seeding, NEITHER renders. An unconfigured self row
  and a feed that has not arrived look identical from the gate, and guessing
  wrong shows a setup form to somebody whose company was named months ago.

### What it deliberately does not do

The Users bullet of the tie task asks for an optional account picker on the
**guest-invite send flow**. There is no guest-invite send flow in this shell,
or in the portal -- a guest invitation is space-scoped, and the OS has no
space surface to send one from. So the field is threaded end to end (proto,
handler, `createGuestInvitation`, and the TS SDK's `sendGuestInvite`) and the
invitation list RENDERS the tie wherever a row carries one; the picker is a
few lines the day a send flow exists to put it on. Building the flow itself
to hang a picker from it would have been a different feature.

Training's domain tag is likewise render-and-filter only, which is what the
design asks for: nothing about routing, attachment or agent behaviour
consults `knowledgeDomain.accountId`, and `domainsForAccount` is the only
query in the tree that mentions it.
## The Bin, the seventh app and the only dock FIXTURE (memql#4784)

`src/apps/bin/` is the archive: everything you threw away, what it was, and
where it came from. Five things about it are new rules rather than repetitions
of the six apps before it.

- **A TRASH CAN THAT CANNOT DESTROY ANYTHING IS A DIFFERENT OBJECT, and the
  app says so on the page.** Every trash can anybody has used is a waiting room
  for deletion; an archive in MemQL is an append-only re-version carrying
  `archived=true`, so the bytes stay, every earlier version stays, and there is
  no expiry anywhere in the product. A surface that looked like a trash can and
  quietly behaved differently would be read as one -- so the difference is
  stated in the header line, in the empty state, and in the settings section
  where somebody goes looking for the retention control that does not exist.
  An absent control with no account of itself reads as something nobody got
  round to building.

- **ONE DRAG CONTEXT NOW SPANS THE SHELL** (`chrome/dragScope.tsx`). The desk
  and the dock owned a `DndContext` each and are SIBLINGS in the layout, so a
  drag begun on a desk icon or in a Files window could never resolve a drop in
  the dock -- dnd-kit only considers droppables inside the context the drag
  started in. That was invisible until something needed to cross the line.
  Handlers claim an id PREFIX and register a REF: a hook that re-registered
  when its closure changed would re-register on every render of the desktop,
  and its cleanup would unregister mid-drag. The alternative -- native HTML5
  dnd for this one target -- is the two-implementations-of-one-behaviour shape
  the arrival-cue section warns about, with "the desk icon can be dragged to
  the Bin but the Files row cannot" as the bug nobody notices for a month.

- **A FIXTURE IS NOT A PIN.** `dockFixture` on the manifest puts the Bin in
  the dock in every session and keeps it OUT of the pin list -- including out
  of a stored desktop document that names it, because a fixture that is also a
  pin can be dragged out of the strip and lost. Pins roam with the desktop; a
  fixture is a property of the shell, so no upgrade path and no corrupt
  document can leave somebody without one. Its context menu offers no pin
  control at all rather than a disabled one.

- **THE BIN IS NOT DRAWN AS A DANGEROUS THING.** Amber is `warn` and red is
  `error` everywhere here, and an archive is neither. The drop target lights in
  the ACCENT, like every other "yes, here" affordance in the OS. (The CONFIRM
  keeps the shell's existing confirm language, matching the Files inspector --
  two different confirms for one action is worse than a colour that leans
  cautious.) A **folder** dropped on the Bin is REFUSED and told where to go:
  archiving a folder is a recursive walk whose confirm names the live count
  inside it, and a dock icon cannot count that. A file from the COMPUTER is
  refused out loud, and `stopPropagation` runs in both drag phases -- the dock
  sits over the desk plate, whose own handler would otherwise upload it at the
  exact moment somebody meant to throw something away.

- **THE STORY LEADS, AND IT IS SAID EXACTLY ONCE.** Where a file came from is
  the fact that decides whether you still want it -- a client's laptop in March
  is a different decision from an agent's output yesterday -- so the row's
  quiet middle is the provenance and the detail panel's header is the machine
  block: its name, its presence, the absolute path (selectable in one gesture,
  because the reader is a window away from a file manager on that machine), and
  the origin link state. Where a machine is named the header SENTENCE stands
  down, rather than announcing the same machine twice in eighty pixels. The
  rows carry no presence dot at all: it means "is that machine reachable right
  now" everywhere in this shell, which beside an archived row is both
  irrelevant and misleading -- a file whose origin is gone would show green for
  as long as the machine that no longer holds it stays awake.

Two smaller things. The list merges TWO feeds (`live/mergedView.ts`, promoted
out of this app when Deployables' list needed the three-feed form), and
`useLiveView` cannot do that: it caches against ONE upstream snapshot's
identity, so a folder arriving while the artifacts are unchanged folds into
nothing -- the list never moves, nothing errors, the folder is simply missing.
And "was filed in" resolves against the ARCHIVED folders, because
`libraryFolders` carries `archived != true` and a folder that went to the Bin
with its contents is invisible to every other surface in the product.

### Origin link states, in Files (epic memql#4783)

A file pushed from a watched folder on a fleet machine carries a state against
its origin -- `synced`, `stale`, `origin_gone` -- rendered as a chip on the row
and rolled up as a dot on the folder. Three rules:

- **ABSENT IS NOT A STATE.** A browser upload has no origin to link to and
  every file stored before the field existed has no member. Reading either as
  `synced` puts an in-sync badge on every file in the Library.
- **`synced` DRAWS NOTHING on a folder.** The reason to mark a folder is to
  make somebody open it, and a green mark on every backed-up folder is noise
  that makes the few needing attention invisible. The rollup reports the WORST
  beneath it, through every ancestor, so the top of a deep tree does not look
  clean.
- **The states come from a THIRD feed over `v1:library:file`**, not from the
  index. Promoting `linkState` onto `v1:library:artifact` needs an automation
  on the file's `node.updated`, and the artifact-to-file archive automation
  already runs the other way -- the pair closes a loop where each write
  publishes an event the other subscribes to. Three concepts at one app root is
  not a violation of the one-feed rule: that rule is per CONCEPT, and two
  concepts cannot disagree.


### Backups, the Files section that sets those states up (memql#4841)

The arrangement half of the same epic: a folder on one of the caller's
machines, kept arriving in a Library folder. It reads
`v1:library:watchedFolder` as a FOURTH feed at the Files app root, and lives in
Files rather than in Fleet because the thing is a folder -- Fleet is machines
and how work is routed to them.

- **THE LINK IS THE ROW.** A backup is a relationship between two named ends,
  so the surface draws one: machine and path on the left, Library folder on the
  right, a wire between them whose SHAPE and colour are the state. The
  direction is drawn because the direction is a rule (one-way forever), and the
  two file counts either side are the point -- the origin's own count is
  something only the machine can answer, and when a backup is behind the
  difference between them is the story.
- **Colour is never the only carrier.** Every tone is also a word on the state
  line and a sentence in the link's accessible name, and the wire changes shape
  as well as hue (solid / dashed / severed / stopped-at-a-bar), so it survives
  greyscale. Motion appears on exactly one tone -- the one where bytes really
  are moving -- and `prefers-reduced-motion` removes it without removing
  information.
- **`paused` beats every fault, and `waiting` beats the rest.** The cockpit
  stops sweeping a paused backup, so its `originState` is the last thing seen
  BEFORE the pause; painting that as a live alarm on something somebody turned
  off is how a person learns to stop reading the colours. And a backup nothing
  has reported on is neither working nor broken -- claiming either invents
  evidence.
- **A backup's badge reads ITS OWN files, matched on `(machine, path)`, never
  the destination folder's rollup.** Anything else in that Library folder -- a
  browser upload, a file dragged in -- has no origin to be stale against, and a
  folder rollup would put "changed on the machine" on a backup because of a
  file that came from no machine at all.
- **`lastSweepAt` is rendered continuously and is ABSENT from the
  fingerprint.** A sweep touches it, and the two counts, on a schedule for
  every backup forever; naming any of them as news strobes the list on the
  sweep's own cycle. The counts still re-render when they move -- only the CUE
  is fingerprint-driven, which is the pair the campaigns app pins.
- **A refusal belongs to the write that produced it.** The writes hook carries
  an `errorId` beside the sentence, because one shared error string over a list
  of rows has no home: on every row it is four copies of one refusal, on none
  it is a write that failed silently.
- **The machine can veto.** `refused_by_policy` is its own `originState`, not a
  flavour of `unreadable`: nothing about the folder is wrong and the repair is
  in that machine's `policy.yaml`. The row is written from a browser and read
  by a cockpit, so its path is one the cluster is naming on somebody else's
  machine -- the situation appsession's `CheckWorkspace` exists for, and the
  same answer.

## Campaigns, the eighth app (epic memql#4827 / #4828 / #4830)

`src/apps/campaigns/` is mail: the audiences it goes to, the copy it is made
of, the mailboxes it leaves from, the sends themselves, and the rules that
send one message when something happens in the cluster. It is the first app
whose subject LEAVES the cluster, and six things about it are new rules
rather than repetitions of the seven before it.

- **A HEARTBEAT IS NOT NEWS, AND THIS IS THE SHARPEST CASE OF IT SO FAR.**
  Fleet's heartbeat moves every fifteen seconds and the Domains sweep every
  two minutes; the drain worker writes `sentCount` / `failedCount` /
  `skippedCount` on every batch of every running send. Naming one in
  `campaignFingerprint` would ring every row in the list, repeatedly, for the
  whole duration of every send in the cluster -- arriving exactly when
  somebody is watching. `recipientCount` is out for a second reason: the
  preflight freezes it at the same instant the status flips, so it would fire
  a second cue for a change the status already announced. The fingerprint is
  `name | status | scheduledAt | audienceId | templateId | senderIdentityId |
  accountId | lastError`.

  **The counters must still RE-RENDER live**, which is the whole point of the
  send bar filling under the person watching it. Re-rendering and ringing are
  different statements and the fingerprint is the only thing that separates
  them, so `test/campaigns/app.test.tsx` pins BOTH: a counter tick leaves
  `data-arrival` null AND puts the new figure on screen; a rename rings. A
  test of one half passes against a cue that fires on everything.

- **THE LIVE/ON-DEMAND SPLIT IS A VOLUME ARGUMENT, AND IT IS RECORDED RATHER
  THAN INFERRED.** `campaign`, `audience`, `template`, `senderIdentity` and
  `emailRule` carry broadcast rules: one row per thing a person authored.
  `delivery`, `engagementEvent` and `recipient` are EXCLUSIONS with their
  reasons written down in `RoutingExclusions()` -- one delivery row per
  recipient per send, one engagement row per open (mail clients prefetch the
  pixel), and, the surprising one, an audience roster, because hand-editing
  is human-paced but a 20,000-address CSV import is a 20,000-event burst
  proportional to a FILE rather than to anything a person did.

  So the ledger, the stats and the roster are on-demand reads that print when
  they were read and offer to look again, and they SAY WHAT THAT COSTS: an
  address added in another window does not appear, and an unsubscribe does not
  flip a row. A `LiveList` over any of them would render "Loading from the
  cluster" and then a list that silently never moved -- worse than a plain one,
  because the caption would be claiming wiring that is not there.

- **AN ABSENT FIGURE IS AN EM DASH WITH A REASON, NEVER A ZERO.**
  `campaignStats` reports a unique open or click count as UNMEASURED when the
  fold behind it hit its bound, and reports no soft-bounce figure at all
  because nothing measures one per campaign. A `0` there would be this window
  inventing a fact about somebody's send -- and a zero open rate is a thing
  operators act on. `Figure` is `{value: number | null, absentBecause}`, an
  absent key and an explicit null are the same answer, and a rate is OF
  DELIVERED rather than of the audience: an open rate over the whole roster
  silently punishes a campaign for its suppressions, which is the opposite of
  what suppressing was for.

- **A SEND IS A BAR, NOT A ROW OF STAT CARDS.** Four numbers in four boxes
  makes a person add them up to learn the one thing they came for. One band,
  the width of the panel, partitions the audience into `sent | skipped |
  failed | pending` -- which is literally what happened to it -- with a legend
  giving the exact figures beneath. It makes the SKIPPED slice visible, the
  compliance-relevant number nobody goes looking for. `pending` is DERIVED
  rather than read, so the slices always sum to the whole and the band can
  never show a gap that reads as a rendering fault; the counts CLAMP at zero,
  because when a frozen estimate and live observations disagree the
  observations win. It is `flex-grow` and four token colours, not a chart
  library, and it carries `role="img"` with an `aria-label` stating the
  figures in words -- a bar a screen reader cannot read is a bar that excluded
  somebody, and the picture's whole content is proportion. Engagement sits
  BELOW it, smaller, as a rate of delivered plus the raw unique-of-total: it
  is a different question about the sent slice only, and a fifth segment would
  put two denominators in one band.

- **THE IMPORT REPORT IS A PANEL THE PERSON DISMISSES.** The operator's next
  action after a CSV import is fixing the file -- open it, go to line 412 --
  so the outcome sentence and the sample bad lines with their numbers stay on
  screen until they say they are done. A toast puts that evidence on a timer,
  which is the reason this shell has none. The upload itself rides
  `items/edgeUpload.ts` like every other transfer in the OS
  (`test/files/onePath.test.ts` fails the build on a second speaker), so
  chunking, resume, retry and verbatim refusals apply with nothing new
  learning them; the artifact id then goes to the engine, which reads the file
  under the CALLER'S OWN actor. The id is not a capability.

- **MERGE TAGS SHOW WHAT THEY RENDER TO, WHICH IS DOCUMENTATION THAT CANNOT
  GO STALE.** Four base tags are a closed set the renderer knows;
  `{{fields.*}}` is not, because those exist only because somebody's CSV had a
  `company` column. No list in this repo could know that, so the strip samples
  a real recipient from a chosen audience -- which is the only way an operator
  discovers `fields.*` at all, and which also catches the case a spelling
  check cannot, a column present but blank for the person you sampled.
  Clicking inserts at the cursor and leaves the cursor after the tag. The test
  send's UNRESOLVED list renders in the same vocabulary, right where the
  copy is being written: a typo'd `{{fields.compnay}}` goes out as its own
  literal text and looks like nothing from this side, so this is the only
  check that catches it before the whole audience does.

- **THE RULES BUILDER IS A SENTENCE, AND THE DELIVERY LANE IS NEVER A
  TOGGLE.** "When a *[thing]* is *[created / changes]* -- optionally *[only
  when ...]* -- email *[template]* to *[who]*." Six labelled boxes would make
  somebody assemble that meaning themselves, from parts that only mean
  anything together. The `who` control is the ONLY place the recipient mode is
  chosen and the two delivery lanes are a CONSEQUENCE of it: each choice says
  in one plain line what it does -- "no unsubscribe footer, and the do-not-mail
  list is not consulted" versus "the do-not-mail list is checked before each
  message, an unsubscribe link is attached, every outcome is recorded" -- as an
  EFFECT rather than as a term of art. The words "operational lane" and
  "marketing lane" appear nowhere in the surface, and a test asserts that.
  Trigger concepts come from the LIVE registry (`listConcepts`, the SDK's own
  `ConceptsListMsg` accessor; the OS had none before this surface), because a
  fixed list would make the newest half of a cluster's schema untriggerable
  with no way to tell. A rule whose bundle failed validation or whose circuit
  breaker tripped renders the ENGINE'S OWN SENTENCE, never a paraphrase.

- **A CLUSTER-WIDE FACT IS A BANNER OVER THE LIST, NEVER A STATUS ON EACH
  ROW.** `authoredAutomationsEnabled` is a global hard stop: with it off, the
  scheduler's `GlobalGate` suppresses every firing for every owner on every
  node, checked before owner-gating and before the breaker -- and NOTHING about
  a rule row changes. Every rule still reads `active`, which is true: they are
  active AND inert, all of them at once. Painting "halted" onto each row would
  claim something about each rule that is not true of any of them, so the fact
  is stated once, above the thing it applies to. Without it an operator reads a
  list of active rules that send nothing and has nowhere on screen to find out
  why, which is the exact silent failure this app exists to remove.

  **ABSENT IS NOT FALSE, and that half is bigger than the banner.** A missing
  settings row, a shape that stops projecting the field, and a failed read are
  all *unknown*; only an explicit `false` shows the banner. The SDK's `rowBool`
  answers `false` for a missing key, so reading the switch through it would put
  "your rules are halted" on every fresh cluster there is -- `kit`'s `boolOr`
  with a `true` fallback is the honest read, and it matches what the engine's
  own gate does with an absent row. A read that FAILED says quietly that it
  could not check, rather than claiming the switch is on: "we could not check"
  is a fact about this window, not about the cluster. Three tests pin the
  silences and one pins the appearance, because the silences are the half that
  turns a banner into a scare.

- **TWO THINGS WEAR THE STATUS `paused`, AND ONLY ONE IS SOMEBODY'S
  DECISION.** `setEmailRuleStatus` is the operator's stop button; a rule can
  also stop after its runs kept failing, and the failure that did it is on
  `lastError`. Saying "you paused this" over the second case throws away the
  only diagnostic there is and is wrong about who did it. The reading is made
  from the EVIDENCE rather than from a mechanism the browser cannot observe --
  a paused rule carrying a run failure is reported as paused *with* that
  failure, and the copy never names who stopped it.

### What it deliberately does not do

**There is no delete anywhere in this app, and every surface says why rather
than leaving somebody hunting for one.** An audience is archived because every
delivery record names it; a campaign is cancelled because what already went
out stays sent; a mailbox is retired because past campaigns name the row and
the reputation and warmup history are keyed on its address. Removing any of
them orphans the evidence a deliverability review is made of.

**A test send is a CAMPAIGN operation, so the template editor borrows one.**
`campaignTestSend` renders the campaign's template through the campaign's
resolved sending identity; there is no such thing as testing copy with no
sender. The editor therefore mounts the campaign detail's own panel against a
campaign that uses the template, and says "no campaign uses this template yet"
rather than showing a disabled control with no account of itself.

**Senders live here rather than in Settings -> Integrations**, even though the
two sit next to each other. Integrations is about CREDENTIALS -- the things an
operator rotates from a shell -- and a sending identity carries none: it is a
campaigns record saying a mailbox exists and may be used. What it cannot do is
make a mailbox sendable; that is the tenant's own policy, and an address
declared here but missing from it comes back as the provider's own 403 on the
campaign's `lastError`.

## Logs, the ninth app -- and a section on every other one (epic memql#4895)

`src/logs/` is the reading surface over the cluster's log store, `src/apps/logs/`
is the app (Stream, Search, Settings), and every app's `*_SECTIONS` now carries
`{ id: "logs", name: "Logs", roles: { min: "admin" } }` right before its
settings section. Five things about it are rules the next live surface gets
wrong by default.

- **THIS CONCEPT DOES NOT BROADCAST, AND THE TAIL POLLS.** `v1:observability:logLine`
  rows never enter the graph -- the store answers from its own hypertable --
  so no `graph.node.*` event exists for a subscription to receive, and a
  `useLiveCollection` over it would render "Loading from the cluster" and then
  a list that never moved. `useLogTail` runs one baseline (`logsTail` with no
  cursor: the newest lines, oldest first), then polls with the newest row's
  `occurredAt` and `id` as the keyset cursor every two seconds while the
  document is visible; an empty answer is "nothing new". A facet change is a
  different reading and re-baselines: the rendered call string is the key.
  There is deliberately NO arrival cue on a log line -- a log is nothing but
  arrivals, and a cue that fired on every one would never stop.

- **`logsSection` IS REQUIRED ON EVERY MANIFEST, like `settingsSection`, and
  it must be admin-floored.** Every read on the log store is admin-and-above
  in the ENGINE (spec L3, enforced in the Go handler), so a section offered
  below that floor opens on a refusal for everyone in it and reads as "this
  app is broken". `logsSectionProblem` checks both existence and the floor --
  on the section, or on the app when the app's own floor is at or above admin
  (the Logs app names its Stream). `AppLogsSection` reads the app's SLICE:
  lines tagged with the app id OR about a concept the app owns, which the
  engine ORs into one scope predicate; the concept list is generated
  constants, never composed ids.

- **THE FRONT END'S OWN LINES ARE CAPTURED, AND THE CAPTURE CANNOT RE-ENTER
  ITSELF.** `src/logs/capture.ts` wraps `console.error` and `console.warn`
  (calling through -- nothing is silenced), listens for window errors,
  unhandled rejections and `pagehide`, and batches to `logsRecordClient`
  through the connection the shell already holds: flush every two seconds or
  at twenty lines, at most fifty per call, a queue cap of two hundred with
  the oldest dropped and counted, identical lines collapsed with
  `attributes.repeat`, `rate_limited` dropped and never retried. The queue is
  module state and never React state, the send is unawaited with its
  rejection swallowed, and a `busy` flag routes anything the capture path
  itself throws or logs to the ORIGINAL console only. Credential-looking
  attribute keys and bearer material in a message are stripped where the
  line is made. `resetCaptureForTest` exists because the installer is
  once-per-page by design.

- **A RENDER ERROR STAYS IN ITS WINDOW.** `chrome/WindowErrorBoundary` wraps
  every app body, keyed by window: React would otherwise unmount the whole
  tree above an uncaught render error -- every window, the dock, the desk --
  in response to a fault in one app. The boundary shows a Notice with the
  error's own sentence and a "Reload app" that remounts the body, and it
  REPORTS the line with the app id and section exactly, from props, where the
  capture's focused-window context can only guess.

- **THE WINDOWED LIST IS A FIRST USE.** Nothing else in the OS is virtualized
  and nothing else needs to be; `src/logs/WindowedList.tsx` renders the rows in
  view plus an overscan at a fixed row height per density, and is promoted to
  `kit/` on the second use rather than invented as an abstraction from one.
  Its `follow` is the PARENT's state: the list reports scrolling away from the
  bottom (and back), and the parent decides what that means. The jump pill
  is absolute against the list's relative root, never fixed -- the desk plate
  is CSS-transformed and becomes a fixed element's containing block.

## Ask voice (epic memql#4747)

The mic toggle is live: hold it to talk, tap it to keep listening, and the
transcript lands in the box as you speak. Five things about it are rules the
next surface that touches audio or the Ask sheet gets wrong by default.

- **`format` IS A LABEL THE SERVER DOES NOT READ.** `AiTranscribeStreamStart`
  carries one, and the obvious browser capture -- `MediaRecorder`, which
  yields webm/opus -- can declare `format: "webm"` and look correct. The
  cluster's default STT provider is `openai-realtime`, and
  `integrations/stt/openai_realtime.go` passes only `SampleRate` through: it
  never reads `Format`, and resamples whatever arrives as though it were
  16 kHz PCM16. Hand it opus and the session opens, chunks flow, and the
  transcript comes back as plausible nonsense. So `ask/pcm16.ts` is a real
  resampler -- stateful across capture blocks, box-averaged rather than
  point-sampled -- and the worklet that feeds it is a buffer and a pipe with
  no arithmetic in it, because a worklet has no test harness and the
  arithmetic is the part worth proving.

- **THE EDGE HAD TO STOP FORBIDDING THE MICROPHONE.** `component/edge`
  answered `Permissions-Policy: microphone=()` on every hosted site, which
  rejects `getUserMedia` with `NotAllowedError` BEFORE the browser prompts --
  indistinguishable from the person declining. Voice would have passed every
  test, worked under `npm run dev` (vite sends no such header), and been dead
  in every cluster while blaming the user. It is `microphone=(self)` now;
  camera and geolocation stay closed, and a Go test says why.

- **DELTAS REPLACE THE FIELD; THEY NEVER APPEND.**
  `AiTranscribeStreamDelta` carries the whole accumulated transcript, not an
  increment -- the opposite of the chat path in the same component.
  Appending renders "openopen theopen the fleet". The field is `readOnly`
  while the mic writes it, never `disabled`, so it stays focusable.

- **THE LEVEL NEVER ENTERS REACT STATE.** It moves at the frame rate; state
  would re-render the streaming answer log sixty times a second to animate one
  ring. It is `--os-mic-level`, written from a rAF loop that runs only while
  the mic is live. The ring itself is a `box-shadow` on the existing 30px
  button -- the shell's cue language, and the geometry spec C promised would
  not change when voice landed.

- **A REFUSAL IS A STANDING FACT, NOT A PHASE.** `denied` outlives the
  attempt that found it, so the control keeps explaining itself while the
  person types. It covers a genuine refusal AND a Permissions-Policy block,
  because browsers report them identically -- which is why the sentence names
  the browser instead of accusing the reader of a choice they may not have
  made. Text stays fully usable throughout, and nothing is a dialog.

There is no input-LANGUAGE setting and no microphone PICKER, and both
absences are deliberate: `ai_transcribe_stream.go` accepts `language_hint` and
discards it (the language is pinned cluster-wide), and every browser already
offers a per-site input device in the address bar. A control that changes
nothing is worse than an absent one.

## Themes, the marketplace (epic memql#4745)

`src/themes/` is the pack format, the loader that refuses a broken one, three
built-in packs, and the drawer that sells them. Five things about it are new
rules rather than repetitions.

- **A PACK IS DATA, NOT A STYLESHEET.** The foundation's `tokensHref` could
  be neither validated (a fetched stylesheet is opaque to the page that
  fetched it) nor trusted (a pack setting `--os-cell-w` breaks the desk grid;
  one zeroing `--os-duration-cue` disables the arrival cue for a reader who
  never asked for reduced motion). A pack is JSON carrying VALUES and the
  shell writes the CSS. Values are whitelisted by character set -- the posture
  `component/edge`'s `validHost` takes, and for the same reason.

- **A THEME CHANGES HOW THE OS LOOKS, NEVER HOW IT BEHAVES.** The format
  carries 21 colour/depth tokens twice (dark and light) plus bounded wallpaper
  parameters. The type scale, the radii, the grid cell and the motion
  durations are not in it at all. A marketplace must not be able to sell a
  desktop that will not scroll.

- **THE ACCENT IS NOT A STATUS COLOUR.** Amber is `warn` and red is `error`
  everywhere in this shell, so an amber-accented pack puts the status hue on
  every primary button and live dot in the OS. Accents come from the cool half
  of the wheel; a test refuses a hue between red and yellow.

- **THE STORE IS A DRAWER, BECAUSE THE PRODUCT IS THE DESKTOP.** Pointing at
  a card restyles the desk, the dock, the wallpaper and every open window
  behind the panel; leaving snaps them back. That is why the Themes tile
  CLOSES the Launcher (a full-screen glass overlay would hide the preview),
  why the drawer takes the right edge, and why its backdrop is the one modal
  backdrop here that does not tint. Ask is the precedent: a surface whose
  subject is the desktop itself cannot be a window on the desktop. Focus
  previews too -- a keyboard reader who could only preview by choosing would
  be shopping by trying things on permanently. The preview is session state,
  absent from `documentFromState`, so a pointer cannot roam anybody's machine.

- **A CARD IS TWO MINIATURE DESKTOPS, DRAWN FROM THE PACK'S OWN VALUES.** Not
  a swatch strip (six colours say nothing about what they do) and not a
  screenshot (it goes stale, and it cannot show the other mode). The pair is
  the epic's per-pack light-and-dark verification made visible;
  `test/themes/contrast.test.ts` is the same verification as a number.

`installedPacks` lives in `DesktopDocument` -- so a theme roams with the desk
it was installed on -- added WITHOUT bumping `version`, which is the
icon-group lift's precedent: a bump discards the desktop of anyone on an older
bundle, and nobody should lose their desks because a theme list arrived. Packs
are re-validated on the way IN rather than trusted because they were stored.

Where packs come from is a CSP question, not a preference: the edge answers
`connect-src 'self' ...`, so the OS cannot fetch one from a vendor's origin at
all. Today they ship in the bundle or arrive as a file somebody drops on the
drawer; the cluster's own Library is the productized next step, and the
commerce cut is recorded in
`docs/superpowers/specs/2026-09-01-os-voice-themes-handoff-design.md`.

## Work, the tenth app (the work spine, sub-project A)

`src/apps/work/` is the human surface of the work spine: a person's goals, the
runs that carry them out, the steps each run took, and the places it had to
stop and ask. Design record:
[2026-09-05-work-spine-design.md](../../docs/superpowers/specs/2026-09-05-work-spine-design.md)
(sections B, D, E and I). Five things about it are new rules rather than
repetitions of the nine apps before it.

- **THE SPINE'S WEIGHT IS THE PRODUCT'S CLAIM, AND IT IS DRAWN IN INK RATHER
  THAN IN HUE.** The system works a goal out once and replays it afterwards
  without a model unless reasoning is genuinely needed. Nobody can check that
  from a table of forty-seven rows that all look alike -- they would have to
  read every row to find the three that cost something. So a run has a thread
  down its left edge: a deterministic step is a hollow node on a hairline (a
  hundred of them read as TEXTURE, not as a hundred things to read) and a
  reasoning step is a filled node on a segment of solid ink, with the cost
  readout on that row and nowhere else. The eye finds where the machine thought
  before a word is read.

  Hue was the wrong axis and the reason is this shell's own vocabulary: amber
  is `warn`, red is `error`, and the accent is live/primary/yes-here, so a
  colour-per-kind legend would put status hues on a partition that has nothing
  to do with status and a reasoning step in accent would read as "this step is
  fine". A weight axis survives greyscale, survives every theme pack, and is a
  SECOND channel rather than the only one -- the kind is also a word on the row
  and in its accessible name.

- **ABSENT IS NOT ZERO, AND HERE THAT IS THE HEADLINE RATHER THAN A DETAIL.**
  The SDK's `rowNumber` answers `0` for a missing key, which is right for a
  count and wrong for every figure on a run's `spent`. Epic A1 writes none of
  them, so `runSpend` reports each as `null` and the panel renders an em dash:
  "0 model calls" on a run that made three is the single most damaging thing
  this surface could say, because *it reached no model* is the claim the
  product makes. The same rule governs `kind == ""` -- A1 leaves `function`
  steps unclassified, and reading a blank as `deterministic` would put "no
  model was called" on a step that may well have called one. Unclassified is
  its own band slice and its own count, never folded into the free half.

- **A HEARTBEAT IS NOT NEWS, AND THIS IS THE SHARPEST CASE IN THE SHELL SO
  FAR.** Fleet's heartbeat moves every fifteen seconds and the Domains sweep
  every two minutes; a running run writes `heartbeatAt` at EVERY STEP BOUNDARY
  and broadcasts the whole row each time. Naming it in `runFingerprint` would
  ring hardest for the run somebody is already watching move. `spent` is out
  for the campaigns app's second reason instead: the counters must RE-RENDER
  live -- that is the point of watching a run spend -- and must not RING, and
  the fingerprint is the only thing separating those. `test/work/rows.test.ts`
  pins both directions.

  The counterpart is that **a parked run wears a STANDING MARK**, not a cue: a
  run can wait for days, and a ring that decays on the clock would be seen only
  by whoever happened to be looking when it parked. That pairing is the
  Deployables update chip's.

- **APPROVALS ARE THE APP'S REASON TO EXIST, AND THE DECISION IS MADE BESIDE
  THE EVIDENCE.** Every human gate in the spine is one `v1:work:approval` row
  and the run does not move until somebody decides it -- the defect the design
  record names about the old planner is that its gates were canvas cards on a
  cognition space, and an engine-only cluster registers no canvas concept, so
  those approvals were already invisible. A one-click Approve in the list is
  the obvious design and the wrong one: `artifactHash` is over the exact
  command, patch, message or draft, and the builtin refuses a decision whose
  artifact moved. So the inbox is a triage list (oldest first -- a queue is
  answered from the front, and there is deliberately no sort control) and the
  acts are on one bar beside the classifier's evidence, rendered in the data
  voice and never paraphrased: the rule id is where somebody goes to change the
  policy.

  **`answer` is the one contract this window guesses at.** It is declared
  `object` on both the concept and the builtin and epic A2 owns the executor
  that reads it, so `answerPayload` sends back the option the approval itself
  offered (`{value, label}`) or `{text}` for a question with none -- written
  down in one function rather than spread through the surface.

- **THE JOURNAL IS NOT LIVE AND SAYS SO; THE STEPS FEED BELONGS TO THE PAGE.**
  `v1:work:goal`, `run`, `step` and `approval` broadcast; `modelCall` and
  `observation` deliberately do not, on volume grounds (one row per model
  request, one per tool result). A `useLiveCollection` over either would render
  "Loading from the cluster" and then a list that silently never moved, which
  is worse than a plain read because the caption would claim wiring that is not
  there -- so the journal reads when asked, prints when it looked, and says
  what that costs. And the STEPS feed is retained by the run page rather than
  by the app root: a per-run timeline at the root would subscribe a window to
  every step of every run this person owns to draw one of them, which is the
  rule the Deployables app wrote down about deployment timelines.

  **`workStepsForOwnerRun` carries `@unbounded`, which excludes `sort`**, so
  the timeline is ordered by `seq` client-side. A timeline drawn in fold order
  reshuffles itself the moment any step updates -- exactly when somebody is
  watching it.

### What the browser found that the suite could not

The whole suite was green over every one of these, because jsdom lays nothing
out. They came out of a rendered pass in both modes, which is the acceptance
DESIGN.md asks for.

- **A flex item's default `min-width: auto` refuses to shrink below its
  content.** A goal statement is a SENTENCE rather than a name, so it pushed
  `.os-row` to 856px inside a 664px column and the list scrolled sideways --
  `white-space: nowrap` on the text inside does nothing, because the item
  around it is what will not give. It is scoped rather than added to
  `.os-row-name` in the kit: every other list in this shell names a thing, and
  letting those start truncating would change eight apps to fix one.
- **The hairline was invisible, so there was no thread.** At `--os-rail` (ink
  at 14%) a 1px line does not render against the dark ground, and the spine
  appeared only where a reasoning step drew it in ink -- four unrelated marks
  rather than one thread thickening.
- **An empty aside reserved half the window** on both Goals and Approvals to
  say what a clickable row already says (rule 9). It is absent when there is
  nothing in it, which is the shape Bin already had.
- **A button centres its own text**, so a goal statement long enough to wrap
  drew its second line centred inside a left-aligned paragraph.
- **The context outlived the identifier.** `flex: 1 1 auto` on the run row's
  "what it is for" line and a shrinkable name meant the NAME lost first, so a
  narrow column drew "re..." beside the whole of the goal it belongs to.
