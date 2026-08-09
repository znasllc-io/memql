---
title: View Elements and the Fitness Contract
audience: public
status: stable
area: concepts
sinceVersion: 0.15.0
owner: znas
---

# View Elements and the Fitness Contract

**Status:** authoritative reference

A **view** is how a concept's rows are shown. A view is composed of
**elements** -- a table, a calendar, a checklist, a chart -- and each
element renders rows exactly one way. This page is the contract that
decides **which elements can render a given concept, and with which of
its fields**.

The elements live in
[`@znasllc-io/memql-view-kit`](../../../sdk/ts-viewkit) and are shared
by every concept-agnostic surface: the portal, the VS Code concept
panel, anything else built on the package. It is the companion to
[display cards](display-cards.md): a display card says what a row is
CALLED, fitness says what shape a row IS.

The point is the same as the display card's. **A concept declared today
gets a calendar today**, because the calendar asked for "a datetime
field", not for a concept it was taught about. There is no
concept-specific rendering code anywhere in the package, and a test
fails the build if a concept id appears in a renderer.

---

## 1. The two halves

**A concept profile** describes what a row set actually carries.
**An element spec** describes what an element needs. Fitting one against
the other produces a **fit**: a verdict, the concrete field chosen for
each of the element's slots, and what is missing.

```
   rows + ConceptInfo  ──profileConcept──▶  ConceptProfile
                                                  │
   ElementSpec.requires ─────────fitElement───────┤
                                                  ▼
                                             ElementFit
                                   verdict · bindings · unmet · score
```

### Why a row sample and not a schema

The wire's `ConceptInfo` carries id, entity, description and the display
card. It does **not** carry per-field types. Rather than ship a
second, client-side copy of the DSL's type table -- which would drift
from the loader on its first divergence -- a profile is derived from the
rows themselves. What the rows carry is, for rendering purposes,
precisely the question being asked.

A profile records, per field: its **kind**, how many rows carry a value
for it, how many distinct values it takes, and which display-card slots
name it.

| Kind | What produces it |
|---|---|
| `text` | a non-empty string that is not a timestamp |
| `datetime` | a string that parses as an ISO-8601 date or instant |
| `number` | a finite number |
| `boolean` | `true` / `false` |
| `list` | an array |
| `object` | a nested object |

A DSL `enum(...)` is a `text` field that happens to take few distinct
values. That is deliberate: a grouped board does not need to know a
field was declared as an enum, it needs to know the column count will be
small, and `distinct` answers that for an enum and a low-cardinality
string alike.

---

## 2. What an element requires

An element declares a list of **requirements**. A requirement names a
**slot** -- the element's own word for the role, never a field name --
and says what could fill it.

```ts
{
  slot: "start",
  description: "the date each row sits on",   // prose, used in the explanation
  kinds: ["datetime"],
  min: 1,                                     // 0 makes the slot optional
  max: 1,                                     // or "all" for a plural slot
  prefer: ["primary"],                        // display-card slots, in order
  preferNames: ["startsAt", "dueAt", "date"], // field-name families, in order
  distinctMax: 12,                            // "must be a category, not free text"
  explicitOnly: true,                         // never guess this one
  degraded: "rows render as a moment, not a span",
}
```

Two of those knobs carry most of the design:

- **`prefer`** points at a display-card slot rather than a field. It is
  how the checklist finds a to-do's `done` field without knowing what a
  to-do is: the concept declared `status="done"`, so "whatever this
  concept calls its status" resolves to it. The same checklist renders
  any other concept with a boolean lifecycle flag.
- **`explicitOnly`** turns off guessing for a semantically loaded slot.
  Any boolean will *type-check* as "is this an all-day event"; almost
  all of them are wrong. For such a slot a wrong binding is worse than a
  reported gap, because the render is confidently incorrect rather than
  visibly incomplete.

### Resolution order

For each requirement, in declaration order, over the fields not already
bound to an earlier slot of the same element:

1. an explicit override from the caller,
2. the display-card slots named in `prefer`, in order,
3. the field names in `preferNames`, in order,
4. every remaining kind-compatible field, skipping row plumbing (`id`,
   `concept`, `type`, `schema`, `partition`) -- **skipped entirely** for
   an `explicitOnly` slot.

Steps 1-3 bypass the row-plumbing skip: naming a field explicitly, or
declaring it as the display card's primary, is a decision already made.
A field binds to at most one slot per element, which is why an element
needing two timestamps declares the more specific one first.

**Naming a slot settles it.** Steps 2-4 run only for slots the caller
did *not* speak about. If `options.bindings` mentions a slot at all,
the automatic resolution is skipped for it -- so an **empty list is how
a caller declines a slot**, which is how a predefined view asks the stat
strip for a row count and no summed measures (`revocationEpoch total` is
a true number and a meaningless one). The same rule means a misspelled
field name reports the slot unmet rather than quietly substituting
whatever the scan liked next, which a caller who named a field would
have no way to notice.

---

## 3. Reading a fit

```ts
{
  element: "calendar",
  verdict: "partial",
  score:   0.833,
  bindings: { start: ["dueAt"], label: ["title"] },
  unmet: [{ slot: "end", required: false, reason: "no field ... named endsAt, endAt, endTime" }],
}
```

| Verdict | Meaning |
|---|---|
| `full` | every requirement bound, including the optional ones |
| `partial` | every REQUIRED requirement bound; something optional is not. Usable, degraded, and `unmet` says how |
| `unfit` | a required requirement could not be bound, or the set has fewer rows than the element's minimum |

`bindings` answers "the calendar needs to know WHICH date field to
plot". Every slot binds a **list** because some slots are inherently
plural (a table's columns); read a single-valued slot with
`boundField(fit, "start")`.

`score` ranks the elements that fit; it never decides whether one does.
Required slots weigh 1, optional slots 0.5.

**Ranking.** `fitElements` returns the whole library best-first: full
before partial before unfit, then by score, then by **specificity** (how
many required slots the element has). The last rule is what keeps the
list list from winning everything -- it requires nothing of a concept
and therefore always fits, so it sorts below any element that actually
engaged with the concept's shape.

### Explaining a fit to a person

`explainFit` turns a fit into prose, built out of the requirement
descriptions the element author wrote:

> Calendar fits todo, with limits. It uses dueAt for the date each row
> sits on. It uses title for what to call each row on the grid. Nothing
> supplies the instant each row ends (no field the concept points at for
> it, and none named endsAt, endAt, endTime), so each row renders as a
> single moment rather than a span.

> Map cannot render calendarEvent. It needs the north-south coordinate,
> and there is no field the concept points at for it, and none named
> latitude, lat.

One implementation, so a composer, a picker and a tooltip cannot each
invent a different explanation for the same fact.

---

## 4. The elements

Every element below was built against a real concept in the tree, and
its tests read that concept out of `dsl/` rather than describing it from
memory.

| Element | Requires | Built against |
|---|---|---|
| **List** | nothing | anything -- the universal fallback |
| **Record** | nothing | anything |
| **Stat tiles** | nothing; optional numeric measures | aggregates over any concept |
| **Table** | at least one scalar field | `v1:cluster:node`, `v1:identity:user` |
| **Calendar** | a datetime; optional end, label, all-day flag, detail | `v1:calendar:calendarEvent` |
| **Checklist** | a boolean and a label; optional deadline, grouping | `v1:todos:todo` |
| **Timeline** | a datetime; optional label, detail, status | `v1:deployment:deployment`, `v1:identity:auditEvent` |
| **Board** | a low-cardinality text or boolean; optional label, detail | `v1:cluster:node` (its `health` enum) |
| **Bar chart** | a category; optional measure (else row counts) | `v1:observability:codeMetric` |
| **Line chart** | a datetime and a measure | `v1:observability:codeMetric` |
| **Pie chart** | a category; optional measure | `v1:observability:codeMetric` |
| **Proportion rail** | a category (text or boolean); optional measure | `v1:identity:user` (its `role` and `active` splits) |
| **Map** | a latitude and a longitude | nothing yet -- see below |

**The map is the honest case.** No concept in the tree carries
coordinates today. The map still ships, still declares what it needs,
and reports `unfit` for every concept that exists -- so no picker offers
it, and anyone who asks why gets the sentence above. The day a concept
declares a lat/lon pair, the map appears with no code change. A test
pins that state and fails when it changes.

---

## 5. Charts: one visual system

Bar, line, pie and the proportion rail share one file, one categorical
palette in one fixed slot order, one axis and gridline frame, one tick
stepping and one number format. Separate renderings would be separate
palettes within a release.

- **Colour does one job per form.** A bar chart's length already encodes
  its value, so every bar takes the *same* hue -- colouring bars by
  their value spends the identity channel re-encoding what the bar
  already says. A line chart takes one hue per series, a pie or a rail
  one per slice.
- **Two forms answer the same question in different room.** The pie and
  the proportion rail both show share-of-whole and declare the same
  requirements, and they fold past the palette through one shared
  routine so a row set cannot report a different number of categories
  depending on which is on screen. The difference is space: a pie needs
  a 320x240 block, the rail is one line tall. That is what lets a page
  header carry "how does this population divide" above the population
  itself, which is how every predefined view in the portal opens
  (memql#3319). Layout is the axis a form is allowed to differ on.
- **A boolean category is not "true" / "false".** A boolean grouping
  field goes through `statusText`, so an active/inactive split reads
  "active" and "not active" -- the same rule the status badge follows
  (memql#3303). A legend does not show the field's name, so the value
  alone says nothing.
- **The slot order is a safety mechanism, not decoration.** The palette
  is validated for colour-vision deficiency: worst adjacent separation
  9.1 (light) / 8.4 (dark) against a >= 8 target, worst normal-vision
  separation 19.6 / 19.3 against a >= 15 floor.
- **Colour is never the only carrier.** Every bar and slice is
  direct-labelled with its value, every line's last point is labelled
  with its series name, and two or more series always get a legend.
  Three light-mode hues sit below 3:1 contrast on a light surface, and
  those labels are the required relief.
- **Past six series the palette stops.** A seventh folds into a neutral
  "Other" rather than inventing a hue.
- **One axis, always.** The line chart binds exactly one measure
  automatically. Plotting a call count and an error rate against one
  axis is the dual-axis mistake in disguise, so a second series is only
  ever an explicit choice by whoever composed the view.
- **Light and dark are both selected.** The dark palette is the same
  hues re-stepped for a dark surface and validated against it, not an
  automatic inversion.

Charts are SVG in the same VNode tree as everything else, so they are
testable as strings and need no charting dependency. Interactivity
follows the package's rule -- no inline handlers, ever: each mark
carries an SVG `<title>` (a real hover tooltip with no JavaScript at
all) plus `data-vk-category` / `data-vk-value` for a host that wants
more, and the `<svg>` is `role="img"` with a summarising `aria-label`.

### Theming

Every colour goes through a `--vk-*` custom property. The chart palette
adds a second tier because a hue cannot be theme-neutral the way a grey
at 15% alpha can:

- `--vk-chart-1` .. `--vk-chart-6`, `--vk-chart-other`,
  `--vk-chart-grid`, `--vk-chart-axis`, `--vk-chart-surface` -- the host
  override, and the only tokens a host sets.
- `--vk-chart-N-default` -- view-kit's own answer, redefined per theme
  inside the stylesheet. Internal.

A host that sets nothing gets the validated palette, following
`prefers-color-scheme`. A host rendering dark chrome on a light OS
either maps its own tokens onto `--vk-chart-*` or passes
`{ theme: "dark" }`, which stamps `data-vk-theme` on the chart.

---

## 6. Adding an element

1. Write the renderer against a **real concept**: read its definition in
   `dsl/<domain>/concepts.memql` first, so the slots come from fields
   that exist rather than fields you imagined.
2. Declare the requirements. Reach for `prefer` before `preferNames`,
   and `preferNames` before letting the generic scan pick.
3. Add it to `VIEW_KIT_ELEMENTS`, most-specific first.
4. Add a rule for every class you emit -- the anti-drift test fails on
   an unstyled class and on a rule nothing emits.
5. Add a test that reads the real concept and asserts the bindings.

What you may not do is branch on which concept you are rendering. If a
concept needs different treatment, express the difference as a
**requirement its fields can satisfy** -- that is the whole mechanism,
and it is enforced: a concept id literal or a comparison against
`concept.id` / `concept.entity` in any source file fails the suite.

---

## 7. Predefined views, and the line they may not cross

Five concepts get a hand-designed screen in the portal, because they
are the ones an operator lives in: people (`v1:identity:user`), agents
(`v1:agents:agent`), customers (`v1:identity:account`), deployments
(`v1:cluster:deployment`) and audit (`v1:identity:auditEvent`). They
live in `clients/portal/src/views/`, addressed at `/views/:viewId` and
`/views/:viewId/rows/:rowId` (memql#3319).

**A predefined view is a LAYOUT CHOICE OVER THESE ELEMENTS.** It picks
elements, names their requirement slots, and arranges the bands. It
does not render a row, and it does not read a display card -- it names
an element's slot and lets the fitness profile resolve the field, the
same as everything else. All five follow one grammar, so learning one
screen teaches five:

| Band | Question | Typical element |
|---|---|---|
| reading | how many are there? | stat tiles |
| shape | how does that divide? | the proportion rail |
| roll | which ones, specifically? | table, timeline or board |

**Where the line is, and how it is held.** If a view needs something no
element provides, the answer is a new **element**, not markup in the
view -- otherwise the library stops being what makes a new concept work
for free, and the designed screens drift onto a second renderer. The
proportion rail is the worked example: these views needed share-of-whole
in one line of height, the pie needs a block, so the rail was added to
the library and every concept has it now.

The third tier -- a view a PERSON composes at runtime, over a concept
nobody designed for -- is built on the same elements and the same
requirement declarations, and is documented in
[composed views](composed-views.md). An element gains one further piece
of metadata for it: `band`, saying which of the three questions above the
element answers, so a newly written element takes its place in a composed
arrangement with no change to the composer.

That rule is mechanical, not editorial.
`portal_view_composition_test.go` (repo root, so weakening it edits Go)
scans the view tree and fails on row markup (`<table>`, `<tr>`, `<ul>`,
svg primitives), on iteration that could produce a row (`.map`,
`.forEach`), on a second VNode-to-React bridge, and on reading
`displayCard` directly. It cannot tell whether the element a view chose
is the *right* one, and it does not police a single field read off one
object -- its own header says so at length.
