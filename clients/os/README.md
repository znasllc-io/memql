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
the shared kit in `src/kit/`. Product apps are stubs until their epics
land (#4721 #4725 #4733 #4737 #4741).

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
