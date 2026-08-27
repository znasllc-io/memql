# MemQL OS -- Desktop Shell (Foundation) -- Design

- **Date:** 2026-08-26
- **Status:** approved (voice brief + in-session Q&A with the owner; the
  window-model choice was made against ASCII previews)
- **Scope:** `clients/os/` (full redesign of the shell that #4705/#4706
  shipped), `scripts/os/build.sh` (build the `file:` SDK dependency),
  `.github/workflows/ci.yml` (an `os-checks` lane -- the OS suite currently
  runs in no lane), this spec. No proto changes, no wire changes, no
  frontend-team ping (this IS the frontend).
- **Replaces:** the slot-manager model locked in epic #4704. The owner has
  seen the shipped shell and rejected it ("needs a complete redesign -- an
  actual operating system"). The #4704 cuts that survive are listed in
  section A0; the slot/strip/tile chrome does not.
- **Follow-up epics** (filed, not built here): one per app -- Artifacts,
  Deployables, Fleet, Users, Training, Settings content -- plus theme
  marketplace, roaming desktop, Ask voice, VS Code artifact handoff.

## Why

Owner's brief, condensed: the shipped OS shell "looks freaking terrible" and
does not behave like an operating system. Wanted: a real desktop as the
landing surface; apps that open in windows with minimize / full-screen /
close; a bar to pin and re-open apps; a launcher app that shows what is
installed; files and folders on the desktop with drag-and-drop; virtual
desktops holding at most two apps each, spilling into a new desktop
automatically; widgets that live on the desktop without opening an app; AI as
a first-class citizen (ask by voice or text, one affordance reachable
everywhere); real-time lists driven by gRPC subscriptions with a subtle "this
changed while you watched" cue; apps and in-app sections gated by the user's
role; themeable from day one (a marketplace will sell themes); subtle,
restrained effects; clean and simplistic throughout; reuse UI elements where
it makes sense. The foundation must be correct and future-proof -- the app
epics build on its contracts.

## Locked decisions

| # | Decision | Choice (owner-approved) |
|---|---|---|
| D1 | Metaphor | A real desktop: desks, windows, dock, launcher, desktop items. The slot manager, research strip, and launcher header tiles are removed |
| D2 | Window model | Smart auto-placement: one app opens large and centered; a second splits the desk; drag a title bar to swap sides or throw to another desk. Cap 2 windows per desk; opening a third auto-creates the next desk. Min / full-screen / close on every window; one window per app |
| D3 | Files | Desktop files are Library artifact shortcuts -- the OS has no file system. Provenance dot: green = content reachable now, amber = origin machine offline / not synced. Open hands off to VS Code (`vscode://znasllc.memql/open`). Dropping a host file uploads via `POST /artifacts` |
| D4 | Folders | Desktop-level only; open as a popover grid, never a window |
| D5 | Widgets | First-class desk residents registered like apps and role-gated the same way. The Ask widget ships first |
| D6 | AI | One name -- **Ask** -- three surfaces: dock orb, desk widget, title-bar affordance carrying the app's context tag. Text path wired now over sdk-core `ai`; the mic toggle is present but wiring voice is its own epic |
| D7 | Live data | One sdk-core `Connection` owned by the shell; a `LiveList` primitive (over `LiveCollection`) gives every list the arrival cue. The Fleet stub app is the exemplar |
| D8 | Roles | The app registry carries allowed roles; launcher, dock, and open-by-id all filter on `clusterRole`. App manifests declare role-gated sections for in-window nav |
| D9 | Look | Brand-anchored, dark-first "instrument" look: graphite grounds on the brand dark axis, porcelain (not cream) light mode, brand green demoted to signal duty only. Squada One in tiny doses. Signature: the memory-field wallpaper + ghost desk numeral |
| D10 | Theming | A theme is a CSS token pack applied via `data-os-theme` on the root and on every window root. Mode (light/dark/system) stays orthogonal. Marketplace later; the contract ships now |
| D11 | Persistence | A `DesktopStore` interface; v1 is versioned localStorage. A graph-backed "roaming desktop" is a follow-up epic |
| D12 | Skeleton apps | Launcher + Settings are real; Artifacts, Deployables, Fleet (live machine list), Users, Training are openable stubs. Every real app is its own epic |
| D13 | Small screens | Keep the pointer/hover layout split. Phone: no desks or windows -- dock becomes a tab bar, apps open one at a time full screen, Ask is a sheet. iPad: desks and windows with touch drags |
| D14 | CI | An `os-checks` lane (install / typecheck / test / build) keyed on its own change bucket, added to `ci-required` |

## A0. What survives from #4704/#4705/#4706

Same identity, same OAuth client and PKCE flow, same `MyAccess` facts, no
second RBAC. The `os.<domain>` named host, bundle seeding, runtime-config
loading, and deploy wiring. The pointer/hover three-way layout split. The
light/dark/system mode mechanism and storage. Safari (including iOS) support:
no Chromium-only APIs, `backdrop-filter` with solid fallback.
`prefers-reduced-motion` zeroes every motion token. Sign out always reachable
in chrome. The test scaffolding (vitest + testing-library on jsdom).

Removed: `src/slots/`, `src/research/`, the launcher header, the coming-soon
tile as chrome (it moves into the Launcher app), and the warm-cream token
palette.

## A. The shell model

### Desks

A **desk** is a virtual desktop holding at most two windows. State is an
ordered list of desks; exactly one is active. Opening an app when the active
desk is full creates a new desk immediately after it, places the window
there, and switches to it. A desk with no windows, no items, and no widgets
that the user did not explicitly create is garbage-collected when left. At
least one desk always exists.

Desk navigation: a dot pager rendered above the dock (click to switch;
current desk emphasized), horizontal plate-slide animation between desks,
and keyboard bindings scoped to the desktop surface (not fired while a
window's content has focus): `Ctrl+Shift+ArrowLeft/Right` to switch,
`Ctrl+Shift+Digit` to jump. Dragging a window's title bar onto a pager dot
or holding it at the screen edge throws the window to that desk (subject to
the cap; a full target desk refuses with a shake-free "no vacancy" cue --
the pager dot dims).

### Windows and placement

Placement is computed, never freehand:

- One window on the desk: centered, large (a token-driven fraction of the
  desk, clamped to a max content width).
- Two windows: side-by-side halves with the desk gutter between them.
- Dragging a title bar over the other half swaps sides (live preview).
- Minimize collapses the window toward its dock icon; the dock icon carries
  the running dot and restores it.
- Full-screen takes the desk (the other window, if any, is temporarily
  covered, not moved); leaving full-screen restores the two-up layout.
- Close removes the window; a remaining window animates back to centered.

A window is one instance per app: launching an app that is already open
focuses its window (switching desks if needed). Window chrome: app icon +
name, the app's current section as a breadcrumb, then Ask-in-context,
settings gear (jumps to the app's settings section), minimize, full-screen,
close. Apps never open second windows; navigation happens inside the window
via the app's own section nav (a slim in-window sidebar once the manifest
declares sections).

### Dock

One bar, bottom center, glass. Left: the Launcher button (fixed). Center:
pinned apps, then running-but-unpinned apps; a running app carries the dot
under its icon; drag to reorder pins; right-click (long-press on touch) for
Pin/Unpin + Close. Right cluster: the Ask orb, the connection dot (the SDK
connection's state), a monospace clock, and the avatar menu (theme mode
light/dark/system, sign out). Pins and order persist via `DesktopStore`.

### Launcher

A full-screen glass overlay (dock button or `Ctrl+Shift+Space`): search
field, the app grid (role-filtered), and a Widgets tab listing available
widgets with "Add to desk". The theme-marketplace tile lives here, visibly
"coming soon", opening nothing. Esc or click-out closes.

## B. Desktop items

The desktop surface of each desk carries **items** on a snap grid: files,
folders, and widgets. All drag interactions use dnd-kit (`@dnd-kit/core`);
items snap to the grid, collisions resolve to the nearest free cell,
positions persist per desk.

### Files (artifact shortcuts)

A desktop file references a Library artifact (`v1:library:artifact` id +
denormalized title/kind/source for instant paint). Sources of files on the
desktop in this foundation:

- **Drop from the host OS**: uploads through the existing
  `POST /artifacts` route with the artifact promotion the Library already
  does, then places a shortcut where dropped. Upload progress renders on the
  icon; failure renders a plain error state with retry (no toast stack).
- Future (app epics): "send to desktop" from the Artifacts app.

The icon: a quiet file glyph by kind, the title beneath (two-line clamp),
and the **provenance dot** in the corner:

- **green** -- content reachable now: bytes live in the cluster (uploaded /
  workbench / agent / exported), or `source=computer_use` and the producing
  machine is online.
- **amber** -- not reachable: `source=computer_use` whose
  `producedByWorkerId` machine is offline, or content the caller cannot
  fetch. Hover/inspect names the origin ("made on <machine> by computer
  use", "uploaded here").

Opening a file (double-click / Enter; single tap on touch) fires the
existing VS Code handoff URL shape with `kind=artifact` and the artifact id,
against the runtime-config `domain`. The extension side of that `kind` lands
in its own follow-up epic; until then the OS shows its normal "VS Code did
not answer -- is the MemQL extension installed?" fallback after a timeout,
which is also the permanent UX for a machine without VS Code. There is no
in-OS editor by design.

### Folders

Right-click (long-press) the desk: New folder. A folder holds file shortcuts
(not widgets, not other folders -- flat by design, revisit only with
evidence). Opening renders a popover grid anchored to the folder icon; drag
items in/out; rename inline; deleting a folder returns its items to the
desk. Folder contents persist via `DesktopStore`.

### Widgets

A widget is a desk-resident card, grid-spanning (each widget declares a size
in grid cells), living beside icons and windows. Registered in the same
registry namespace as apps, role-gated identically, added from the
Launcher's Widgets tab or the desk context menu, removed via its own
overflow menu. Widgets render inside a token-carrying root (same contract as
window roots) so themes reach them. The foundation ships the **Ask widget**;
the framework (registration, sizing, placement, persistence, role gating) is
the deliverable.

## C. Ask -- AI as a first-class citizen

One surface, one name, three entry points:

- **Dock orb**: always present, opens a compact Ask sheet anchored above the
  dock.
- **Ask widget**: the same surface resident on a desk.
- **Title-bar Ask**: the same surface, opened carrying the app's context tag
  (`app:<id>` + the current section) so an app-scoped question is one click.

The surface: a single input row (text field, mic toggle, send), a compact
answer area streaming below it, and the context tag when present. The text
path is wired end-to-end in this foundation over sdk-core's `ai` chat
streaming, using the cluster's default provider policy. The mic toggle is
rendered from day one; pressing it in this foundation shows the
"voice arrives with the Ask voice epic" state -- the control's geometry does
not change when voice lands. Esc closes; the sheet never takes a desk slot
and never counts against the window cap (AI is chrome, not a module -- the
one #4704 cut the owner reaffirmed).

## D. Live substrate

The shell owns exactly one sdk-core `Connection`, built after sign-in from
the runtime config and the auth context, exposed via a React context. The
dock's connection dot renders its status (`connected` green, `reconnecting`
amber, `disconnected` hollow); reconnect/backoff/rotation stay the SDK's
job.

**`LiveList`** is the one list primitive every OS surface uses for live
data: it wraps a `LiveCollection` snapshot and renders rows with the arrival
behavior the owner described -- a new row slides in with a short
rise-and-settle, carries a "new" tick that decays after a few seconds, and
an updated row pulses once. It also renders the `LiveState` (seeding /
live / degraded / disconnected) as a quiet caption, so a frozen list is
never mistaken for an empty cluster. Reduced motion replaces movement with
an opacity step.

The **Fleet stub app** is the foundation's live exemplar: a read-only
machine list (name, platform, labels, online dot from `lastSeenAt` within
the 30s window -- same derivation the portal uses) over the worker
registration rows, whose graph events already carry broadcast routing rules
to browser subscribers. It proves connection, LiveCollection, LiveList, and
the dot language end-to-end. Everything else about Fleet lives in its epic.

## E. Roles

`MyAccess` facts (userId / primaryEmail / clusterRole) already load at boot.
The registry filters on them:

- An app or widget manifest declares `roles` -- either "every signed-in
  user" or a minimum role over the cluster ladder (reader < writer <
  developer < admin < owner). The launcher grid, dock rendering, open-by-id,
  and widget placement all consult the same predicate -- there is exactly one
  "can this actor see this app" function.
- A manifest's `sections` each carry an optional role too; the in-window nav
  renders only admitted sections. The Settings app demonstrates: About (all)
  and Appearance (all) plus a Cluster section stub (admin+).

This is presentation gating only -- the engine's row admission remains the
authority on every read; a hidden app is a UX decision, not a security
boundary, and the spec says so where the predicate lives.

## F. Visual language

Brand-anchored; the brand green is the signal, never the paint.

### Tokens (`--os-*`, defined on `:root` and inherited into every
`data-os-window` / widget root)

Dark (default): ground `#07090a` (brand dark bg), desk plate `#0b1110`,
raised `#0e1311`, ink `#e8e6dd` / muted `#9ca395` (the brand fg pair), line
`rgba(232,230,221,0.08)`, glass `rgba(11,17,16,0.55)` + solid fallback
`#0e1311`, accent = brand `#5ccda7`. Light: porcelain ground `#f2f4ef`
(brand bg -- cool, not cream), plate `#ffffff`, raised `#e9ede6`, ink
`#191d1a` / muted `#5b615c` (the brand fg pair), accent `#047d5a`. Status trio, used only for
state dots and badges: ok/live = the accent green, pending/offline
`#e0a63c`, error `#e05b4d`. Radius 16/10; hairline borders; shadow tokens
per elevation (desk item / floating chrome / window). Motion tokens: 140ms
fast / 260ms medium, one standard ease
(`cubic-bezier(0.22, 1, 0.36, 1)`), all zeroed under reduced motion.
Fonts come from `brand/fonts.css` by relative import (the brand gate forbids
`@font-face` outside `brand/`): Inter for UI, JetBrains Mono for ids /
clock / provenance, Squada One only where section F names it.

### The signature: the memory field

The wallpaper is a canvas painting a sparse lattice of time-dots with rare,
glacially drifting links -- the desktop literally sits on the memory graph.
Near-threshold opacity (it reads as texture first, image never), colors from
tokens so themes restyle it, static frame under reduced motion, and it
pauses entirely when the tab is hidden. The active desk's oversized ghost
numeral -- Squada One, single-digit, anchored bottom-right beneath every item
-- is the one loud typographic moment and the desk you are on is always
legible at a glance. Squada One appears in exactly three places: the boot
mark, the ghost numeral, and the Launcher's wordmark row.

### Motion inventory

Press-settle on every interactive (scale 0.98, fast token), window
place/swap/restore slides (medium), desk plate slide (medium), minimize
toward dock, live-arrival rise + decaying tick, focus ring (2px accent,
offset 2px, always visible on keyboard focus). Nothing bounces, nothing
loops, nothing autoplays sound.

## G. Theming architecture

A **theme** is a named CSS token pack: every `--os-*` token, the wallpaper
parameters, and nothing else. Applied as `data-os-theme="<id>"` on the
document root; window and widget roots carry the attribute too so a future
per-window mix stays a CSS-only change. Mode (light/dark/system) remains the
existing orthogonal mechanism -- a theme defines both looks. The foundation
ships the built-in default ("graphite") and the registry type
(`{ id, name, tokensHref? }`); the marketplace epic sells packs into that
contract. No picker UI beyond mode in this foundation.

## H. App and widget contracts (what the app epics build against)

```ts
interface OsAppManifest {
  id: string;                     // stable, kebab-case
  name: string;                   // dock/launcher label
  icon: LucideIcon;
  roles?: RoleRequirement;        // absent = every signed-in user
  sections?: OsAppSection[];      // in-window nav; first admitted = default
  settingsSection?: string;       // section id the title-bar gear jumps to
  component: React.ComponentType<OsAppProps>;
}
interface OsAppSection { id: string; name: string; roles?: RoleRequirement }
interface OsAppProps {
  sectionId: string;
  navigate: (sectionId: string) => void;
  askContext: (tag: string) => void;   // augment the window's Ask context
}
interface OsWidgetManifest {
  id: string; name: string; icon: LucideIcon;
  roles?: RoleRequirement;
  size: { w: number; h: number };      // grid cells
  component: React.ComponentType;
}
```

Apps register statically in `src/apps/registry.ts` (the foundation is not a
plugin loader; runtime app delivery is a later question and deliberately not
answered here). The stub apps ship a shared `StubApp` body that names the
app, its epic, and its declared sections, so the shell is honest about what
is real. `LiveList`, the provenance dot, the Ask context call, and the
role predicate are exported from one `src/kit/` index -- the reuse surface
the owner asked for.

## I. Engineering shape

```
clients/os/src/
  system/      desks.ts windows.ts placement.ts desktop.ts dock.ts
               registry.ts roles.ts store.ts        (pure; unit-tested)
  chrome/      Desktop.tsx DeskPager.tsx Dock.tsx WindowFrame.tsx
               LauncherOverlay.tsx StatusCluster.tsx ContextMenu.tsx
  items/       FileIcon.tsx Folder.tsx provenance.ts upload.ts vscode.ts
  widgets/     registry.ts WidgetFrame.tsx ask/AskWidget.tsx
  ask/         AskSurface.tsx askController.ts
  live/        connection.tsx LiveList.tsx arrival.ts fleetExemplar.ts
  apps/        registry.ts launcher/ settings/ artifacts/ deployables/
               fleet/ users/ training/ (stubs share StubApp.tsx)
  kit/         index.ts (LiveList, ProvenanceDot, RolePredicate, Ask hooks)
  wallpaper/   MemoryField.tsx field.ts (pure geometry, tested)
  app/ auth/ cluster/ styles/            (kept, rewired)
```

- **Packages added**: `@dnd-kit/core` (+ `@dnd-kit/sortable`),
  `lucide-react`, `@znasllc-io/memql-sdk-core` via `file:../../sdk/ts`. No
  animation library; motion is CSS + WAAPI on tokens.
- **Build**: `scripts/os/build.sh` gains the sdk/ts dist prerequisite
  (same shape as the portal script; idempotent). The Docker portal-build
  stage already COPYs `sdk/ts`, `brand`, `clients/os`, `scripts/os` -- no
  Dockerfile change expected; the stage-isolation build in CI proves it.
- **CI**: new `osclient` change bucket (`clients/os/**`, `sdk/ts/**`,
  `scripts/os/**`, `brand/**`) driving a new `os-checks` job (install /
  typecheck / test / build, node 22, portal-checks shape), listed in
  `ci-required.needs`.
- **Persistence**: `DesktopStore` (load/save the desktop document:
  desks, items, folders, widget placements, dock pins, wallpaper/theme
  choice) -- versioned localStorage implementation; every read tolerates
  absence/corruption by resetting to the seeded default desktop.
- **Testing**: TDD on the pure system modules (placement math, cap-2 +
  auto-spill + garbage-collection, swap/throw, dock pin/order, role
  predicate, provenance derivation, arrival reducer, store versioning,
  field geometry). Component tests: open-app -> window appears placed;
  third app -> new desk; minimize -> dock restore; launcher role filtering;
  folder drop; Ask surface opens from all three entry points; stub apps
  render sections. Existing auth/theme/layout tests stay green.
- **A11y floor**: every interactive reachable by keyboard, visible focus,
  window chrome buttons labelled, the desk announced on switch
  (`aria-live=polite`), context menus are real menus, reduced-motion
  respected everywhere.

## J. Out of scope (filed as follow-up epics)

Artifacts app; Deployables app (including the deploy map); Fleet app beyond
the live exemplar; Users app (invites); Training app; Settings cluster
content; theme marketplace + store; roaming desktop (graph-backed
`DesktopStore`); Ask voice (transcribe wiring); VS Code `kind=artifact`
handoff (extension side); phone chrome beyond the tab-bar shell; per-window
theme mix; runtime app delivery/plugin loading.

## K. Acceptance

- Signing in lands on a desktop: memory-field wallpaper, ghost desk
  numeral, dock, empty-desk hint. No slot chrome anywhere.
- Opening two apps splits the desk; a third opens on a new desk and
  switches to it; the pager shows position; throw and swap work by drag.
- Windows minimize to the dock, go full-screen, close; a running app shows
  its dot; pins persist across reloads.
- Files dropped from the host upload and appear with a provenance dot;
  opening fires the VS Code handoff; the fallback message appears when
  nothing answers. Folders hold files via drag.
- The Ask orb, widget, and title-bar entry all open the same surface; text
  questions stream answers; the mic toggle renders its coming-soon state.
- The Fleet stub lists machines live: a registration created while the
  window is open arrives with the rise-and-tick cue; the connection dot
  tracks the SDK connection.
- A reader-role session does not see admin-gated apps or sections; the
  Settings app shows the section pattern.
- Light, dark, and system modes all render the new palette; reduced motion
  stills the wallpaper and every transition; keyboard-only operation works.
- `make os-test`, `make os-typecheck`, `make os-build` pass; the `os-checks`
  lane gates the PR; the Docker portal-build stage still builds.
