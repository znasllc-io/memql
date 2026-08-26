# Portal Declutter + Identity (Epic 1) -- Design

- **Date:** 2026-08-26
- **Status:** approved (brainstorm session with the owner; visual decisions made
  against live mockups in the design companion)
- **Scope:** MemQL Portal (`clients/portal/`) + `brand/` tokens + one small
  engine addition (a generic `uiAssist` AI-suggest domain). No proto changes,
  no breaking wire changes, no frontend-team ping required.
- **Builds on:** epic memql#4502 (form kit, one scroll region, scrollbars) --
  shipped. This epic assumes that foundation.
- **Sibling specs (same session):** Epic 2 "Views: layouts, personality,
  regeneration" and Epic 3 "Local models on the fleet" -- separate documents.

## Why

Owner's brief, condensed: the portal is overwhelming (17 rail items + one per
saved view, duplicated icons, Fleet listed three times in two places), wordy
(developer-facing copy: raw concept ids as page eyebrows, env vars and Go
method names in body copy, engine error strings printed verbatim), and visually
anonymous ("pretty normal"). Wanted: minimal, clean, real-estate-preserving,
production-grade, "our own thing" -- modern but not sci-fi -- with subtle
animations, a per-section info affordance (video placeholder + explanation),
and a first-class AI assist affordance everywhere input happens.

## Locked decisions

| # | Decision | Choice (owner-approved) |
|---|---|---|
| D1 | Rail shape | 7 flat items, no group captions; Views is one destination with a gallery page |
| D2 | Menu-vs-tabs rule | The rail lists areas; a page's sub-surfaces are its tabs; never both |
| D3 | Chrome | "Daylight": near-neutral light mode; dark mode unchanged |
| D4 | Signature | "Constellation": the 9-node mark as the one expressive motif |
| D5 | Raw technical detail | Plain sentence for everyone; owner/admin-only collapsed "Technical details" disclosure |
| D6 | AI affordance | "Synapse": one floating button + focus-ring scope (placement A) |
| D7 | Synapse glyph | Impulse (filled node + single ring), still at idle, refined during visual QA |
| D8 | Token feedback | Game-style float: red estimated-token number on fire; quiet "first run" cue when no history |

## A. Navigation

**Rail (top to bottom):** profile row (unchanged), then Console `/`, Nexus
`/nexus`, Views `/views`, Concepts `/concepts`, Fleet `/fleet`, Library
`/artifacts`, Cluster `/integrations`. Rail footer (connection + replica)
unchanged. No group captions remain; the collapsed 56px rail is 7 icons.

**Rail placement is not URL shape** (existing principle). Existing routes are
kept; the new destinations are frames with tab strips across them:

- **Views gallery (new page at `/views`):** cards for the 5 built-in views
  (Users, Accounts, Agents, Deployments, Audit), then the caller's composed
  views, then a "New view" action (absorbs Compose's rail row; `/compose/*`
  routes unchanged). Agents lives here (no longer a Nexus rail row); its URL
  stays `/views/agents`.
- **Fleet:** already `FleetFrame` with three tabs. Fix: `LocalAppsPage` must
  render inside `FleetFrame` (today it hand-rolls its own header and the tab
  strip disappears on navigation -- `clients/portal/src/fleet/LocalAppsPage.tsx:52-60`).
- **Library:** new `LibraryFrame` with tabs Artifacts (`/artifacts`) and
  Deployables (`/deployables`).
- **Cluster:** new `ClusterFrame` with tabs Integrations (`/integrations`),
  Data origins (`/data-origins`), Stores (`/stores`), Providers
  (`/admin/providers` -- today reachable only by typed URL), Tokens
  (`/admin/tokens`), Keys (`/admin/keys`), Settings (`/admin/settings`).
  Tab visibility follows the same role gating the rail rows have today
  (Integrations visible to all; the rest admin/owner). `/cluster` is not a
  route; the rail item points at the first visible tab.
- **Concepts:** gains an admin-only Modules tab (`/modules` unchanged);
  `ConceptsPage` already uses `Tabs`.
- **Nexus:** `/nexus` unchanged (Goals is the landing; Map/Constructs/Replay
  stay per-goal tabs).

**Saved views leave the rail.** The rail's live `useSavedViews` block is
removed; the gallery lists them. **Redirects:** none required (no URL moves).
The rail's `localStorage` sub-section keys (`memql-portal-rail-section-*`)
become obsolete; remove the disclosure mechanism with them.

**Command palette (new):** Cmd+K / Ctrl+K. Sources: the 7 destinations, every
tab, the 5 built-in views, the caller's composed views (live), concepts from
the registry, and page-level actions ("New view", "Add machine", "Invite
someone"). Fuzzy match, keyboard-first, renders in the existing Dialog
vocabulary. This is what makes the flat rail safe: nothing is more than one
keystroke away.

## B. Icons

One unique icon per destination; additions land in the curated barrel
`clients/portal/src/ui/icons.ts` (pages never import lucide directly).

| Destination | Icon |
|---|---|
| Console | Gauge |
| Nexus | Orbit |
| Views | LayoutGrid (now legitimately the views icon) |
| Concepts | Boxes |
| Fleet | Cpu (frees Monitor for the theme toggle only) |
| Library | Archive |
| Cluster | Network |

The collisions dissolve with the rows that carried them (Bot, Plug, Shield,
per-view LayoutGrid). Tab strips stay text-only. The `VIEW_ICONS` fallback
(`Boxes`) is retired with the rail rows that used it; gallery cards carry the
view's name, concepts, and layout -- no per-view icon needed.

## C. Chrome -- "Daylight" light mode

Light-mode token values move toward neutral paper; **dark mode values are
untouched.** All changes in `brand/tokens.css` as the light half of the
existing `light-dark()` pairs, with the header contrast table re-measured
(brand rule: moving a value without moving the table is a defect). The
identity web UI inherits automatically (shared brand source, memql#4266).

| Token | Light today | Light new |
|---|---|---|
| `--memql-bg` | `#f2f4ef` | `#f7f7f5` |
| `--memql-surface` | `#ffffff` | `#ffffff` (unchanged) |
| `--memql-surface-raised` | `#e9ede6` | `#efefec` |
| `--memql-fg` | `#14201a` | `#191d1a` |
| `--memql-fg-muted` | `#586159` | `#5b615c` |
| `--memql-fg-subtle` | `#7c847b` | `#82877f` |
| `--memql-border` | `#d6ddd4` | `#e5e6e2` |
| `--memql-border-strong` | `#c2cabf` | `#d2d4cf` |
| accent family | unchanged | unchanged |

Exact values are subject to the contrast re-measure; targets: body text AA on
all grounds, muted text >= 4.5:1 on `--memql-bg` and `--memql-surface`.
Everything else (radius rhythm, no shadows, no gradients, two-voice
typography, density) is explicitly preserved.

## D. Signature -- Constellation

One component: `clients/portal/src/ui/Constellation.tsx`. The 9-node mark
geometry (shared with `MemqlMark`/`brand/mark.svg`) rendered large, with an
assemble animation: nodes scale in staggered, edges draw once; then still.
Props: `size` (sm/md/lg), `animate` (default once-on-mount), reduced-motion
renders the final frame statically.

Where it appears (and nowhere else): `EmptyState` (opt-in flag, used for
first-run empties like "No machines yet"), the `PageGuide` dialog header /
video placeholder poster, `SignInPage`, `NotFoundPage`. It is the one
expressive element; it does not spread to decorative contexts.

## E. Motion

Two brand tokens: `--memql-motion-dur: 160ms`, `--memql-motion-ease:
cubic-bezier(0.2, 0, 0, 1)`; bridged through `brand/theme.css` so both
surfaces can use them. Applied system-wide in the portal:

- Hover/focus washes on interactive surfaces (rail items, cards, ghost
  buttons, tabs) -- background/border/color transitions only.
- Tab underline slides between positions.
- Dialogs fade + settle (scale 0.98 -> 1).
- List first paint: staggered rise (max 12 rows, 20ms steps); the existing
  `row-wash` keyframe stays for live arrivals.
- Breathing (the existing 2.4s pulse) stays reserved for genuinely live
  things: the connection mark, online dots, streaming states.

Idle screens are still. `prefers-reduced-motion` disables all of it (extend
the existing three-layer opt-out; the new tokens make that one media query).

## F. The eye guide -- `PageGuide`

- `clients/portal/src/ui/PageGuide.tsx`: an Eye icon button rendered by
  `PageHeader` next to the title (small, quiet, accent on hover). Opens a
  `Dialog` size `xl`:
  1. **Video slot:** 16:9 area. Until a real video exists for the page id, a
     placeholder: Constellation poster + "Guide video coming soon". Videos
     land later keyed by page id with no code change (`guide.videoRef`
     optional field; absent = placeholder).
  2. **What you're looking at** -- 2-4 sentences, operator voice.
  3. **How it works** -- short bullets: where the data comes from (in product
     terms), what the actions do, what to expect.
  4. **Technical details** (collapsed, rendered only for owner/admin): the
     concept id(s) behind the page, relevant env keys, links into docs. This
     is where the removed eyebrow/internals content lives on.
- Content registry: `clients/portal/src/guides/` -- one typed entry per page
  id (`{id, title, body, how[], technical?, videoRef?}`), plain TS objects.
- Coverage is enforced (see H): every rail destination and every
  Fleet/Library/Cluster tab must register a guide entry.

## G. Copy -- operator voice

Voice rules (added to `clients/portal/src/ui/README.md`, the kit's
constitution):

1. Name what the person controls or sees; never how the engine is built.
2. Sentence case, plain verbs, no filler; an action's name states its outcome
   and stays identical through the flow (button -> confirmation -> toast).
3. No concept ids, env vars, Go identifiers, node internals, or design
   rationale in user-facing text. Those live in the guide's Technical details
   or in docs.
4. Errors: state what happened and what to do next, in the interface's voice.
   Never apologize, never vague. Raw detail goes behind the disclosure (D5).
5. Empty states invite action ("Add a machine to run work on a computer you
   own"), never explain rendering policy.

Concrete sweeps (from the audit; exemplars, not exhaustive):

- `PageHeader` `eyebrow` (mono concept id) is replaced by a plain-language
  subtitle on every page (`MachinesPage`, `LocalAppsPage`, `AppSessionPage`,
  `WorkbenchesPage`, `ViewLayout`, `ArtifactsPage`, `DeployablesPage`,
  `StoresPage`, `CampaignsPage`, `StoreDetailPage`, `GoalLayout`). The mono
  eyebrow style remains available only where the value IS data (row detail).
- `admin/KeysPage.tsx:134-151` (env vars + `RotationSupported()` in body
  copy), `integrations/IntegrationsPage.tsx:135-140` (resolver internals),
  `compose/ComposerPage.tsx:174-176` ("Saved as a row in ..."),
  `fleet/WorkbenchesPage.tsx:126-158` (raw node ids as headings, "the remote
  flag is an assertion" copy), and the error callouts that explain UI design
  reasoning (`MachinesPage.tsx:109-112`, `WorkbenchesPage.tsx:192-195`,
  `me/SessionsTab.tsx:80-82`, `fleet/RoutingPolicyEditor.tsx:101-103`) are
  all rewritten under the rules above; displaced technical content moves to
  the page's guide entry.
- New `clients/portal/src/ui/ErrorNotice.tsx`: plain sentence (+ optional
  next step), with the owner/admin-only "Technical details" disclosure
  carrying the raw engine string. All `{state.error}` renders migrate to it.
- `admin/WriteOutcome.tsx` keeps the audit id + trail link (it is evidence)
  restyled as a quiet mono chip inside the outcome line.
- The rail footer's replica id + version stay (operational fact, deliberate
  placement per memql#4316/#4317).

## H. Enforcement (repo-root Go gates, mirroring existing precedent)

1. `portal_copy_voice_test.go`: no `v1:[a-z]+:[a-z]` concept-id literal in
   JSX text/props outside an allowlist (concept browser, DataText id
   contexts, `guides/`, tests).
2. Guide coverage: every rail destination + Fleet/Library/Cluster tab id has
   a `guides/` entry (test walks the nav definition and the registry).
3. Error discipline: no direct render of a raw error state outside
   `ErrorNotice` (mechanical check on `{...error...}` render patterns with an
   explicit allowlist).
4. Existing gates (`portal_control_vocabulary_test.go`,
   `portal_page_frame_test.go`, `portal_view_composition_test.go`,
   `brand_shared_source_test.go`) stay green throughout; icon uniqueness is
   asserted in a portal unit test over the nav definition.

## I. Synapse -- the AI touch

**What it is:** one floating button (bottom-right of `main`, above the scroll
region), the Impulse glyph, present on every routed page. The section holding
focus/hover (a registered scope) gets a subtle accent ring + a tiny "AI
scope" tag; the button acts on that scope. Click = prompt popover (type);
press-and-hold = voice (release to run). The reply is applied to the UI --
prefilled fields, a composed draft -- **never a submit**. Manual input remains
primary everywhere.

**Scope system:** `SynapseProvider` in the shell; `useSynapseScope(id,
{label, fields, apply})` registers a section (forms opt in with a field
schema: name, type, current value, constraints). Focus tracking: last
focused/hovered registered scope wins; the popover names the active scope in
words so the target is never a surprise. Pages with no registered scope get
page-level actions only (open guide, navigate) -- the button never pretends.

**Execution path:** existing `aiSuggest` client surface with a new generic
domain `uiAssist` (the wire `domain` is a free string -- no proto change).
Request: `{scope: {id, label, fields}, prompt, page}`. Reply: `{patches:
[{field, value}], note?}` validated client-side; unknown fields dropped;
values coerced to the field's type or dropped. Applied to the form draft with
a brief accent wash per field, in order.

**Engine half (this epic):** register the `uiAssist` suggest domain in core
(the `RegisterSuggestDomain` registry, beside `knowledge`), backed by a new
`dsl/portalviews/prompts.memql` prompt `uiAssistFill` (structured output,
`@defaultProvider("chat54Mini")`) + template. Strict instruction: values only
from the user's prompt and the provided scope; no invention of identifiers.

**Voice:** browser `MediaRecorder` capture on hold; released audio goes
through the existing batch `AiTranscribeMsg`; transcript becomes the prompt
(shown in the popover, editable, so voice is never a black box). Streaming
transcription is an available upgrade path, not required for v1.

**Token float:** on fire, a small red number rises from the button and
dissipates (~950ms): the estimate, from a per-scope rolling average of actual
usage kept in `localStorage`. First run for a scope: a quiet accent "first
run" cue instead of a number. When the suggest reply carries usage, it
updates the average; when it does not, estimate from prompt+schema size
(chars/4) and mark the float with a tilde. Reduced motion: static fade, no
rise. The float is `aria-hidden`; the popover's status line announces the
same fact for screen readers.

**Hard rules:** never submits or navigates; acts only on the active scope;
every Synapse-triggered call renders the cost; the button idles still (its
ring animates only on hover, while listening, and while a request runs).

**Exemplar wirings (this epic):** Add-machine form, Routing policy editor,
New artifact form. Epic 2 reuses the same affordance for "describe a view"
and "regenerate this view".

## Out of scope (owned by sibling epics)

- Element/view-kit personality, layout vocabulary, view regeneration +
  version history, "all pages compose from elements" convergence, WebGL
  element class, Nexus map visual upgrade -- **Epic 2**.
- Local models on the fleet, provider routing, installer/VS Code detection --
  **Epic 3**.

## Testing

- Unit: nav definition (7 items, unique icons, role gating), palette sources,
  guide registry coverage, ErrorNotice role gating, Synapse scope selection +
  patch application (schema coercion, never-submit), token-average math.
- Existing suites updated: `app.test.tsx`, `fleetMachines/localApps` (frame
  fix), `predefinedViews` untouched semantics.
- Engine: suggest-domain registration test beside the registry; prompt
  renders against its schema (dslconformance covers the construct).
- Visual QA sweep both themes at the end (the #4511 pattern), including the
  Impulse glyph tuning at real sizes.

## Rollout (repo policy: 2 PRs, 3 max)

1. **PR 1 -- foundation:** tokens (C) + motion (E) + Constellation (D) +
   PageGuide component + guides registry seed (F) + ErrorNotice (G) + gates
   landed disabled-then-enabled within the PR (H) + engine `uiAssist` domain.
2. **PR 2 -- the sweep:** rail/nav/frames/palette (A, B) + copy sweep (G) +
   Synapse system + exemplars (I) + gate enablement + visual QA evidence.

Both PRs carry `Refs`/`Closes` lines per issue once the epic is filed (one
keyword per issue -- `Closes #a, #b` closes only the first).
