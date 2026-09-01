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
  provenance dot (green = reachable, amber = not); open hands off to VS
  Code. A desk folder is a SHORTCUT to a `v1:library:folder` (epic
  memql#4721 amends the foundation's local icon-groups): its popover is a
  live view the popover itself retains, desk create/rename are Library
  mutations, and remove-from-desk removes the shortcut and never
  archives. Widgets are desk-resident cards; Ask ships first.
- **Ask** is chrome, not a module: the dock orb, the desk widget and every
  title bar open the same streaming surface.
- **Roles**: one predicate (`system/roles.ts`) gates apps and app sections
  from `MyAccess.clusterRole`. Presentation only — row authz stays the
  engine's.
- **Theming**: `--os-*` token packs on the root and every window/widget/
  sheet root (`data-os-theme`); mode (light/dark/system) is orthogonal.
  The wallpaper (the memory field) paints from tokens.
- **Persistence**: `system/store.ts` (`DesktopStore`) — versioned
  localStorage; desks, items, pins, theme. Never windows.
- Phone keeps its own chrome (tab bar, one app at a time, Ask sheet);
  layout is keyed off pointer/hover, never width alone.

Pure state machines live in `src/system/` (tested without React); chrome
in `src/chrome/`; the app/widget contracts in `src/system/registry.ts`;
the shared kit in `src/kit/`. Every app is real: Settings, Fleet, Users,
Deployables, Training and Files (#4721) -- the last stub went with Files,
and `StubApp` with it.

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

## Deployables, the third app (memql#4725)

`src/apps/deployables/` is the sites this cluster serves, the **deploy map**,
and the two writes that change either -- create a site, publish a Library zip
to one. Three things about it are new rules rather than repetitions of the two
apps before it.

- **One feed, three surfaces, one selection.** The list, the map and the detail
  panel are readings of a single `LiveCollection` retained at the app root and
  passed down. A second `useSites()` inside the map would open a second
  subscription and run a second seed, and the two would then be free to
  disagree about what the cluster currently holds -- which is the one failure
  an app that is a picture and a table of the same thing must not have. The
  selection is shared for the same reason: walking a cluster on the map and
  switching to the list lands on the same deployable.
- **`v1:platform:site` broadcasts BOTH created and updated**, unlike
  `v1:identity:user`, which broadcasts creates and deliberately not updates
  because the row churns on `lastSeenAt`. That asymmetry is why Users re-reads
  a person on open and this app does not, and it is what makes the epic's
  headline true with no engine work: a CI publish through
  `POST /sites/{id}/bundles` flips `bundleRef` on a node nobody in the browser
  is talking to, and the row changes under the person watching it. Read the
  ROUTING RULES before deciding what a concept's live feed does.
- **The cue is a mechanism now, not a list feature.** `live/useArrivals.ts` is
  the fold `LiveList` used to own. The map is the first live surface in the OS
  that is not a list, and a site whose bundle just flipped has to announce
  itself there exactly as it does in the list beside it. Promoted rather than
  copied -- a second copy of a cue is a cue that drifts, and "the map pulses on
  a heartbeat while the list does not" is the bug the fingerprint rule exists
  to stop.

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
reaches; only the **Actions** section is admin+, and that is presentation over
writes the Go hostname policy and `sitePublishFromArtifact` remain the
authority on.

- **The slug rules are mirrored for a keystroke-rate answer, and say so.**
  Cluster-wide uniqueness and the cluster-owner exemption are deliberately NOT
  mirrored -- a browser cannot answer either -- so those refusals arrive from
  the server and render verbatim, because the server's sentence names the
  colliding site and a friendlier paraphrase would drop the one fact that
  helps.
- **A publish refusal is keyed by its stable reason and rendered as a
  sentence**, never as the token. An error carrying NO known reason keeps its
  own message: inventing a friendly sentence for an unknown failure is how a
  real fault gets mistaken for a user error.
- **Nothing is inserted locally.** A created site arrives on its own broadcast
  with the arrival cue, exactly like one somebody else created. The detail
  panel marks a `bundleRef` flip because the VALUE changed, not because a tick
  fired -- an `updated` tick fires for a rename too, and a marker driven by it
  would announce a publish that did not happen.

## Training, the fourth app (memql#4737)

`src/apps/training/` is teaching MemQL from files: a dropzone into the
attachment analysis pipeline, the analysis plans running live beside it, a
review queue over what the pipeline extracted, and a browser of the knowledge
domains it feeds. Four things about it are new rules rather than repetitions of
the three apps before it.

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

- **THE DESK POPOVER IS THE APP'S SURFACE, MOUNTED BY THE SHELL.** A desk
  folder's popover renders a live, folder-scoped view that the popover
  itself retains -- its own collection, deliberately not the Files window's,
  because the desk must work with no window open and the app root's feed
  dies with its window. It lives in `apps/files/DeskFolderPopover.tsx` so the
  projections and the cue contract stay the app's.

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
  repo-walking gate while its tests stayed green.

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
  of them otherwise know nothing about. `useAccountOptions` keys its
  collection on one string, so four surfaces mounting it at once share one
  subscription rather than opening four.

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
