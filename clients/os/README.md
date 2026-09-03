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

### Custom domains: the Domains panel (memql#4805)

`page/stops/Domains.tsx`, mounted as the deployable page's Where-it-lives stop
for a cluster owner (epic memql#4885), is a client's own domain bound to a
site -- the add flow, the two DNS records to create, the live typed status, and
the remove. Three things about it are worth knowing before touching it.

- **The surface is TWO RECORDS AND WHAT WE SEE AT THEM.** Somebody reading this
  panel is a tab away from a registrar's form whose fields are called Type, Name
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
  would say a removed domain had got further than a live one.
- **There is no re-check button anywhere, and the panel says why.** Retries ride
  the sweep's schedule (design D5) -- a button would invite hammering a
  recursive resolver and an ACME endpoint. An absent control with no account of
  itself reads as something somebody forgot to build, so the footer says what
  DOES happen rather than apologising for what does not.

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

## Packages, inside Deployables (epic memql#4794)

`src/apps/deployables/packages/` is the other half of what a deployable IS: a
site is what serves, and a package is where it came from. It ships a live
packages list, a package detail with the analysis report and the deployment
timeline, an always-present confirm gate, and the per-site lifecycle the portal
used to own. Five things about it are new rules rather than repetitions of the
five apps before it.

- **THE ONE-FEED RULE IS PER CONCEPT, NOT PER APP.** Deployables now retains
  TWO collections at its root -- sites and packages -- and that is not a
  violation of the rule the app was built on. What must never happen is two
  subscriptions over the SAME concept, free to disagree about what the cluster
  holds; two concepts cannot disagree, because they are describing different
  things. A package's deployment TIMELINE is a third, and it is retained by the
  detail panel rather than the root, because keeping every package's timeline
  live would subscribe the window to every deploy in the cluster to render one.

- **A FILTER RE-BASELINES THROUGH `useLiveView`'s KEY, not through a `key` prop
  on the list.** Revealing rows the browser already had is not the cluster
  sending them, so the Archived toggle must not fire the arrival cue for every
  newly-visible row. `viewKey` is where a re-baseline is expressed
  (`live/liveView.ts` rebuilds on it), and it is one line rather than an
  unmount.

- **A CUE AND A STANDING MARK ARE DIFFERENT STATEMENTS, and the update needs
  both.** The arrival ring says "this just changed" and decays on the clock;
  the `update` chip says "there is a newer version than the one you are
  running" and stays until somebody deploys. A cue alone would make the news
  visible only to whoever happened to be looking. A chip alone would make it
  arrive in silence. `updateAvailable` is named in the fingerprint DESPITE
  being written by a ten-minute poll, because the engine's feed only writes it
  when the upstream actually moved -- so a flip is news by construction rather
  than a heartbeat.

- **THE STAGE RAIL DRAWS WHAT DID NOT HAPPEN.** `page/Rail.tsx`, over the
  readings in `page/rail.ts`, is the one new visual idea here, and the reason
  it earns a sequenced device -- normally the most over-used structure in
  software design -- is that a deploy has a LAW about its order (`stage ->
  roll -> publish`, reversed on rollback). The part that matters is the
  SKIPPED stages: an app-only package draws them, dimmed, with the reason
  beside them, because "nothing had to restart" is what explains a deploy that
  took seconds and a person counting missing steps cannot find it. `railFor`
  is pure and exported for exactly that reason -- what the rail SAYS is the
  assertion, not what it renders.

  Two smaller rules came out of looking at it in a browser rather than in
  jsdom, which has no CSS engine and would have passed either version: the
  unreached stages dim their CONTENT rather than the whole row, or the
  connector goes with them and the rail stops dead where the run is; and they
  carry no glyph, because a crossed circle reads as "forbidden" and put five
  refusal symbols on a healthy deploy.

- **THE CONFIRM GATE IS A PANEL, NOT A DIALOG.** The report is the size of a
  page, and a scrolling modal is a worse page. More importantly a refusal
  inside a modal that then closes is a refusal nobody can re-read, which is the
  same reason this shell has no toasts. The gate lives on the ROW
  (`status: "awaiting_confirm"`) rather than in component state, so somebody
  who closed the window finds their deploy exactly where they left it.

Two things it deliberately does not do. There is no raw `bundleRef` editor
anywhere (D13: parity covers the four features somebody uses, not the operator
escape hatch -- a field that accepts any URI is a way to point a live site at
nothing). And a `systemOwned` row renders NO lifecycle controls at all, not
disabled ones: the seeded portal and OS sites are exempt from the lifecycle
entirely, and six greyed-out buttons are six controls somebody has to read past
to learn they are not for them. The server refuses those writes regardless --
the presentation is the courtesy, `component/memql/platform_site_status_guard.go`
is the gate.

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
