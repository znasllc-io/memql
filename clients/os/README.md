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
  Code. Folders are popovers. Widgets are desk-resident cards; Ask ships
  first.
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
the shared kit in `src/kit/`. Settings, Fleet and Users are real; the
remaining product apps are stubs until their epics land (#4721 #4725
#4737).

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
  `authSessionsForSubject` is filtered on its argument and nothing else, which
  is safe when the one caller passes the caller's own JWT `sub` and unsafe the
  moment a browser passes an id somebody clicked. The answer was a second
  query (`sessionsForSubjectAdmin`) with its own gate and its own shape, not a
  narrowing of the first -- narrowing it would have refused the self-service
  revoke path for every non-admin in the cluster.
- **Promote on the second use, which is now.** Fleet's row, live view, clock
  and time formatters moved into `kit/` and `live/` rather than being imported
  across apps or copied. `.os-machine` and `.os-fleet` remain as CSS aliases,
  because the shared behaviour is what had to move. The measure that it was
  the right size: Users ships two classes of its own.
