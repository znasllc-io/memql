---
title: "Visual QA: views, layouts, personality, regeneration"
audience: internal
status: stable
area: ops
sinceVersion: 0.9.36
owner: znas
---

# Visual QA: views, layouts, personality, regeneration

- **Date:** 2026-08-26
- **Epic:** memql#4661 -- task memql#4675
- **Method:** a temporary Vite QA harness (`clients/portal/qa.html` + `qa/main.tsx`)
  mounting the real `AppRoutes` over a stubbed `Connection`, driven in Chrome.
  Deleted after the sweep. jsdom performs no layout and resolves no custom
  properties, so heights, document overflow, grid tracks and theme tokens
  cannot be asserted in vitest -- they can only be measured in a browser
  against the real stylesheet.

## What was checked, and what it measured

Measurement rather than inspection wherever a number exists. "It looked fine"
is not evidence a grid collapsed; a resolved `grid-template-columns` is.

| Check | How | Result |
|---|---|---|
| No page-level horizontal scroll | `documentElement.scrollWidth - clientWidth` on 16 routes | 0 everywhere |
| No element overflowing without its own scroller | `scrollWidth > clientWidth` with `overflow-x: visible` | none |
| Every converged page renders as an arrangement | count of `.vk-arrangement` | present on all 13 arranged routes |
| All five layouts render their grid | resolved `grid-template-columns`, driven through the composer's real picker | stack 1 track, dashboard 2 equal, split 3:2, focus 7:3, gallery 1 |
| Narrow-width collapse | the same pages in a 760px iframe (a media query evaluates against the iframe's viewport, so this is the real breakpoint) | every grid to 1 track, overflow 0 |
| Cell personality | counts of `.vk-cell-{time,pill,bool,number,data,ref,absent}` | all present on `/views/users`; lookups resolving on `/views/agents` |
| Never-blank rule | `<td>` with empty text content | 0 |
| Both themes | contrast of pill / numeral / elapsed-time against their painted ground | light 16.8:1, dark 15.2:1 |
| Console | 16 routes with the listener armed from page load | no errors, no warnings |

The console result is a NULL result, so it carries a reachable positive: a
deliberate `console.warn` probe was emitted at the end of the sweep and came
back, which is what makes "no errors" a statement about the pages rather than
about the instrument.

## Findings, and what happened to each

### 1. The composer previewed a layout that would not ship -- FIXED

The live preview rendered the DRAFT; a saved view renders the draft put
through `sanitizeArrangement`. So choosing `split` with no detail pane drew a
split in the composer and a stack everywhere else, and a `focus` with no hero
drew a lead column the saved view would not have had.

This is the one defect the sweep existed to find: it is invisible to the unit
tests, because they assert on the value and the value was correct -- what was
wrong was that the person was shown a different one.

`ComposerSection` previews the sanitized arrangement now. The DRAFT is
untouched, so the layout somebody chose is still what gets written and becomes
live the moment they add the element it needs; what changed is that the
fallback is visible immediately instead of at save time.

Verified after the fix, through the real picker: `split` previews as stack,
`focus` promotes exactly one hero, the other three honour their layout, and
the picker still shows the person's own choice as pressed.

### 2. Enum pills nearly vanished in dark -- FIXED

`.vk-cell-pill` was a border alone, and the border is `--vk-border`, which a
host is free to make very quiet. In the portal's dark theme it resolves to
about 1.5:1 against the ground -- at which point a pill reads as plain text and
the "this is one of a closed set" signal is gone.

The pill now carries a wash as well: `color-mix(in srgb, currentColor 8%,
transparent)`. Derived from `currentColor` rather than a token, so it needs no
new variable and is correct in both themes by construction -- 8% of the text
colour is a faint lift on a light ground and a faint lift on a dark one. A
fixed `rgba()` would have had to pick a direction and would be wrong in one.

### 3. Three repo gates, red for real reasons -- FIXED

Found by `make test` rather than by looking, and all three were consequences of
this epic:

- the embedded-file inventory (one new prompt template, `composeView.tmpl`);
- the control-vocabulary gate: the regenerate affordance built a raw `<input>`
  instead of the kit's `TextInput`, and its `items-end` needed the
  page-header-column exemption the gate's own message asks for;
- the page-frame gate: two converged pages render `<ArrangedPage>` rather than
  `<Container>` directly. The gate now accepts either, because ArrangedPage IS
  the frame for a converged page and a page using it holds both files -- what
  is still refused is a routed body that renders neither.

## What this sweep did NOT verify, stated plainly

**The map's final material and lighting judgement.** WebGL is genuinely
available in the harness (unlike jsdom), but the stubbed connection has no plan
world, so `/nexus` renders its empty-goals state and the `goalMap` scene never
mounts. The geometry change is structural and the values are asserted by
`mapMaterials.test.ts` -- every assertion there has a failure mode that renders
perfectly and looks wrong -- but "does this read as a solid or as a
placeholder" is a judgement made by looking, and it has not been made against a
real goal.

That needs the local k3d cluster with a plan that has run
(`make up` / `make dev NODE=edge`), and it is the one item of this task's
acceptance criteria that is outstanding. It is a tuning pass over
`clients/portal/src/nexus/map/materials.ts` -- one file, by design, so that the
pass is a reviewable diff.

**Reduced motion and the no-WebGL fallback** are covered by
`nexusMap.test.tsx` behaviourally rather than visually here, for the same
reason: the scene did not mount.

## Evidence

The paired light/dark captures are attached to the PR rather than committed.
The repo holds four images and all four are product assets -- a logo, two
marks, an editor icon -- so a QA screenshot would be new precedent for
never-changing binary weight in a public tree, plus a path-coverage exemption,
in exchange for something a PR conversation already carries well.

What they show, on `/views/users` in both themes: the dashboard layout, the
stat tile, the proportion rail, and the cell personality -- enum pills, boolean
dots labelled with the field's own name (never the word "true"), humanized
datetimes with the exact instant on hover, and right-aligned compact mono
numerals (`1.2M` for 1,247,932).

What is reproducible without them, and is the more durable evidence, is the
table above: every row of it is a number a later reader can re-measure the
same way.
