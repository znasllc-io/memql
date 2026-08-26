# Views: Layouts, Personality, Regeneration (Epic 2) -- Design

- **Date:** 2026-08-26
- **Status:** approved (brainstorm session with the owner; layout vocabulary
  and join depth chosen against live mockups/options)
- **Scope:** the arrangement system (`sdk/ts-viewkit/`), the portal's view +
  page rendering (`clients/portal/`), the `v1:portalviews:view` concept and
  its DSL, the concepts wire projection (`component/grpc` + `memql.proto` +
  `sdk/ts`), AI suggest domains, and the WebGL scene layer.
- **Wire note:** the `ConceptInfo` additions are additive proto fields and a
  new suggest domain string -- no removals, no changed required fields. Call
  out in the PR body per repo policy anyway (new response fields a client may
  consume).
- **Siblings:** Epic 1 "Portal declutter + identity" (Synapse affordance this
  epic reuses), Epic 3 "Local models on the fleet".

## Why

Composed and predefined views all render as the same vertical stack of
equally-weighted elements -- "no personality; you drop a counter, a list, a
whatever" (owner). The composer cannot see schemas or relationships (the wire
drops both), AI suggestions have never worked (the domain was never
registered), multi-concept views are stacked silos, and ~28 hand-built pages
ignore the element system entirely. The owner's directives: layouts as a
first-class dimension; data displayed WELL per element; the same mechanism
across ALL pages ("we're not gonna have a custom page for one thing -- even
the profile page"); views regenerable by AI on demand, cached forever, with
version history; WebGL scenes as composable elements; the goals map upgraded
visually.

## Locked decisions

| # | Decision | Choice (owner-approved) |
|---|---|---|
| D1 | Layout vocabulary v1 | ALL of: stack (default/fallback), dashboard, split, focus, gallery |
| D2 | Multi-concept depth | Lookup columns + linked sections; NO engine joins in this epic |
| D3 | Regeneration persistence | Cached forever in the graph; AI runs only on explicit regenerate |
| D4 | Regeneration scope | Per-user override rows; a regeneration never repaints another user's console |
| D5 | Version history | Append-only versions with a strip: Original / v1 / v2 ...; revert = re-write as newest |
| D6 | Coverage | The arrangement system is the PAGE system: all pages converge onto layouts + elements + registered widgets; regeneration works on every arranged page |
| D7 | Composer interaction | Two-pane (live preview + inspector), pointer-drag reorder; no freeform canvas |
| D8 | AI compose entry | The Epic-1 Synapse affordance ("describe a view", "regenerate this view") |

## A. Engine enablers

1. **Schema + relationship projection.** `ConceptInfo`
   (`component/grpc/memql.proto:63-96`) gains two additive fields:
   `fields` (repeated: name, kind, required, enum values, description --
   projected from the stored `Schemas` JSON of the concept's current version)
   and `relationships` (repeated: type, as, field, target, direction).
   `conceptInfoFromConcept` (`component/grpc/concepts_handlers.go:20`) is the
   single projection function feeding both the one-shot list and the
   follow-mode delta stream, so both paths change at once. Mirror in
   `sdk/ts/src/client/types.ts`.
2. **`profileConcept` becomes schema-first**
   (`sdk/ts-viewkit/src/fitness.ts:169`): the field list comes from the
   declared schema; row sampling remains as enrichment only (distinct counts,
   presence). Fields with no loaded value are no longer invisible.
3. **Register `viewArrangement`** -- the domain the client has called since
   day one (`clients/portal/src/compose/suggest.ts:61`) but nothing ever
   registered. Registration lives in core beside `knowledge`
   (`component/memql/suggest_knowledge.go` pattern), backed by the existing
   `composeViewArrangement` prompt (`dsl/portalviews/prompts.memql:53`),
   whose schema is extended for layouts + roles (it never shipped working, so
   it is free to reshape).
4. **New `viewCompose` domain + prompt:** input = free-text description + a
   registry digest (concept ids, descriptions, fields, relationships) +
   layout vocabulary; output = a full draft `{name, sections: [{conceptId,
   layout, entries}]}` validated and repaired client-side exactly like stored
   rows. Powers "describe a view".
5. **Suggest usage passthrough (nice-to-have):** where the provider reports
   token usage, `AiSuggestResult` carries it so Synapse's token float learns
   real numbers; absent usage falls back to the Epic-1 estimate rule.

## B. The layout system

**Grammar.** An arrangement entry today is `{element, band, title?,
bindings?}` in a flat ordered list, and layout is hardcoded as a vertical
stack (`ArrangementBands.tsx`). It gains, additively:

- Per section (concept) : `layout: "stack" | "dashboard" | "split" | "focus"
  | "gallery"` (absent = stack -- every stored row stays valid).
- Per entry: `role: "hero" | "supporting" | "standard"` (absent = standard).

The `arrangements` field is engine-opaque (`[]object!`,
`dsl/portalviews/concepts.memql:53`), so this is a client/view-kit schema
change only.

**Renderer.** New `ArrangementLayout` in view-kit maps (layout, bands, roles)
onto CSS-grid slot templates:

| Layout | Slots |
|---|---|
| stack | today's bands, unchanged |
| dashboard | reading row (tiles) across the top; shape elements side by side; roll below |
| split | roll element (rowList/table) left; `detail` right, driven by selection |
| focus | one hero (chart / map / calendar / scene / proportion) at 70%; supporting column right |
| gallery | display-card grid over the population; reading elements as a compact header row |

**Repair, not trust** (existing rule, extended): unknown layout -> stack;
focus with no hero -> promote the best-fit candidate or fall back to stack;
split with no detail pairing -> stack; roles on elements that cannot express
them are ignored. `sanitizeArrangement` remains the one gate and never
rewrites the stored row.

**Predefined views become seed arrangements.** The five registry entries
(`clients/portal/src/views/registry.ts:104-155`) gain `seed` arrangement
data; the five body modules (`UsersView.tsx` et al.) are deleted; `ViewPage`
renders seeds through the same renderer composed views use.
`portal_view_composition_test.go` tightens: a predefined view is DATA -- no
body modules exist to hand-render anything.

## C. Element personality -- how data reads

Schema-driven display rules applied inside elements (view-kit), so every
consumer improves at once:

- **Columns from schema, not sample:** table/rowList column sets start from
  `@displayCard` primary/secondary, then required fields, capped sensibly;
  plumbing fields stay hidden (`NON_DISPLAY_FIELDS`).
- **Per-kind cell rendering:** datetimes humanized ("2 days ago") with the
  exact value in the data voice on hover/detail; enums as status pills
  (`status.css` families); booleans as dots with labels, never "true";
  numbers right-aligned in the mono data voice with compact notation; ids as
  `DataText` id styling; relationship fields as resolved lookups (F).
- **Emphasis follows role:** a hero statTile renders its numeral at display
  scale (the one sanctioned big-number moment per page); a hero chart gets
  full-bleed height; supporting elements compress (tighter padding, smaller
  captions).
- **Cards come from `@displayCard`** everywhere (gallery layout, rowList
  card mode), so a concept's own identity drives its card.

## D. Living pages -- regenerate, cache forever, versions

**The concept.** `v1:portalviews:view` gains additive fields: `kind
enum("composed","override")` (default composed) and `targetPageId string`
(set on overrides: a predefined view id like `views.users`, or a converged
page id like `fleet.machines`, `me.settings`). Mutations/queries take the
new optional args (`update{}` is read-merge; all additive).

**Resolution order at render:** the caller's active override row for the
page id -> else the seed (predefined registry / page manifest) -> never AI.
Override rows are live-subscribed like any owner-scoped concept.

**Regenerate** is a Synapse action in the arranged page's header (Epic 1
affordance: token float, optional typed/voiced hint such as "more visual,
lead with the chart"). It calls `viewArrangement` with the concept profile(s),
the CURRENT arrangement, the layout vocabulary, and the hint; the repaired
result is written as the newest version of the override row. **AI runs only
here** -- never at render. First regenerate creates the override; subsequent
ones append versions.

**Versions.** Time-series rows give history free: the strip renders
**Original · v1 · v2 · ...** (walked via `asOf`, the deployables pattern);
selecting an old version previews it; "Use this version" re-writes that
arrangement as the newest version. Nothing is ever destroyed; Original (the
seed) is always present and needs no row.

**Guardrails.** A page manifest may declare `required` entries (e.g. the
machines population element, the addMachine widget); sanitize re-inserts
them if a regeneration dropped them. The AI chooses from fitted candidates
and registered widgets only -- it can rearrange the page, not remove its
purpose or invent controls.

## E. Composer v2

Route unchanged (`/compose`). Two panes:

- **Left: the live view** -- real elements over the real concept walk,
  updating as the draft changes; click an element to select it in the
  inspector.
- **Right: the inspector** -- section list (concepts, with relationship
  chips); layout picker (five thumbnails); per-band entry list with
  pointer-drag reorder (replaces the up/down buttons; no freeform canvas);
  per-entry role, title, bindings and (new) field pickers driven by the
  projected schema; the fit explanations stay.

**AI entries:** "Describe it" on the Views gallery and in the composer --
the Synapse affordance -> `viewCompose` -> a full editable draft
(`origin: "suggested"`); per-section "Suggest an arrangement" now actually
works (registered domain). Save path unchanged
(`createComposedView`/`updateComposedView` with the extended arrangement).

## F. Multi-concept: lookup columns + linked sections

- **Lookup columns.** A binding may name a relationship path
  (`ref:<as>.<field>`, e.g. the plan's `ownerAgent.name`). The renderer
  batch-resolves target rows by id through a new SDK helper (per-page LRU +
  request coalescing; the relationship metadata says which concept to read)
  and renders the value per its kind, linked to the target's row detail.
  Unresolvable refs render as the id in the data voice -- never blank.
- **Linked sections.** The composer can link section B to section A via a
  declared relationship; selecting a row in A filters B to related rows.
  V1 filters the loaded walk client-side and says so ("showing rows related
  to <selection>, from loaded data") -- honest about the boundary. A
  predicate-capable read path is the filed follow-up, not this epic.
- **No engine joins** (D2). Every read stays a single-concept authorized
  walk; row authz is untouched.

## G. WebGL scenes as elements, and the map glow-up

- **Element kind `scene`.** Options carry `sceneId` from a scene registry.
  Scenes are predefined, data-bound modules; any layout can host one
  (natural home: focus hero). Lazy-chunk discipline extends the existing
  rule: only scene modules import three/fiber/drei
  (`nexusMap.test.tsx` pattern).
- **V1 scenes:** `goalMap` -- the Nexus map with its special behaviors
  intact, now also placeable as an element; `conceptGraph` -- any concept's
  rows as nodes with relationship edges, the Constellation identity made
  live and generic.
- **Visual upgrade of the map** (owner: "the 3D models don't look very
  good"): placeholder cubes replaced with beveled/rounded instanced
  geometry; materials from brand tokens with rim light and soft ground
  contact; DPR-aware AA; the measured motion timings (`motion.ts`), demand
  frame loop, reduced-motion behavior, accessibility event list and
  no-WebGL fallback all preserved exactly. Final material/lighting tuning
  happens against the running app in visual QA.
- **Nexus unification:** the goal page renders as a focus-layout
  arrangement with the `goalMap` scene as hero -- the proof that even the
  richest page speaks the same grammar.

## H. Convergence -- the arrangement system IS the page system

End-state (owner directive): no bespoke page layouts. Every portal page is
an arrangement -- layout + elements + **widgets** -- and is therefore
regenerable, versioned, and consistent.

- **Element kind `widget`:** a registered interactive portal component
  (closed registry in `clients/portal/src/widgets/`): addMachine,
  routingPolicyEditor, profilePreferences, invitePerson, deployControls,
  ... Widgets participate in arrangements like elements; sanitize drops
  unknown ids; the AI may PLACE registered widgets, never invent them.
  Forms/editors thereby stop being an excuse for custom pages.
- **Phase 1 (this epic):** Fleet machines + workbenches, Artifacts,
  Deployables, the Me profile tabs (the exemplar non-data page), and the
  Nexus goal page (G). Each gets a page manifest (seed arrangement +
  required entries) and drops its hand-built layout.
- **Phase 2 (filed as follow-up):** admin/cluster surfaces, integrations,
  stores, home console (which becomes a regenerable dashboard arrangement).
- **Excluded surfaces:** sign-in/auth, the composer editor itself, dialogs.
- The composition guard grows with each converged page so none regresses to
  bespoke row markup.

## Testing

- View-kit: layout renderer fixtures per (layout x band mix x roles);
  sanitize repair round-trips (unknown layout/role/widget, missing hero,
  required re-insertion); schema-first profiling; per-kind cell rules.
- Portal: resolution order (override -> seed), version strip walk + revert,
  regenerate flow with a stubbed suggester, lookup batch resolution +
  unresolvable rendering, linked-section filtering + its labelling, widget
  registry gating, scene lazy-chunk isolation test extended.
- Engine: projection unit tests (fields + relationships, both wire paths),
  suggest-domain registration, dslconformance over the new/edited prompts
  and concept fields.
- Visual QA both themes at the end, including the map's new materials.

## Rollout (3 PRs -- policy maximum, earned here)

1. **Enablers + grammar:** A (projection, registrations, prompts) + B
   (layouts, seeds-as-data) + C (element personality).
2. **Living pages:** D (overrides, versions, regenerate) + E (composer v2) +
   F (lookups, linked sections).
3. **Scenes + convergence:** G (scene kind, conceptGraph, map glow-up) + H
   phase 1 (manifests, widgets, migrations).

Each issue gets its own `Closes #N` line; wire-affecting PR 1 calls out the
additive `ConceptInfo` fields for the frontend relay.
