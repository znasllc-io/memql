# src/ui -- the portal's component vocabulary

Every screen composes these primitives; no page defines its own button,
input, tab strip, or heading size. This directory sits deliberately OUTSIDE
`src/views/` (the guarded predefined-view tree whose file list a repo-root
test counts), which is what dissolved the old reason for the composer's
copied layout primitives.

## The type scale

| Role | Treatment | Where |
|---|---|---|
| h1 | `text-xl font-semibold tracking-tight` | `PageHeader`, once per page |
| band caption | `text-xs font-semibold uppercase tracking-wide text-muted` | `Band` (h2 on a page, h3 inside a composed section) |
| body | `text-sm`; supporting prose `text-muted` | everywhere |
| data | `font-mono` via `DataText` (number/string tints, id, time) | graph values only, never chrome |
| display | `font-display text-display` | Squada One: wordmark + big-number moments only |

## The control metrics

| Token | Utility | Value | Wears it |
|---|---|---|---|
| `--memql-control-h` | `h-control` | 2.25rem (36px) | `TextInput`, `Select`, `Button size="sm"` -- anything on a form line |
| `--memql-control-h-sm` | `h-control-sm` | 1.75rem (28px) | `Button size="xs"`, chips and segmented controls inside dense data |

Both live in `brand/tokens.css`, shared with the identity pages, because a
control height that differs between the sign-in page and the console is the
same defect as a palette that differs between them. **A control never states a
vertical padding.** Its height is the token; padding is horizontal only. Give a
line control `py-*` back and it stops matching the button beside it, which is
exactly the bug memql#4502 removed.

`Textarea` is the deliberate exception: its height is its content, so it keeps
vertical padding and carries no control token.


## The page frame

**Every routed page renders its body inside `<Container>`.** There is one
content width -- the full width of the shell's main area, with the shell's
gutter -- and a page, pane, frame or view never sets `max-w-*`, `mx-auto` or a
fixed width on its own root.

Measure belongs to CONTENT, never to the frame:

- a paragraph caps its own line length (`max-w-prose` on the `<p>`),
- a form caps its own field width (`max-w-3xl` on the `<form>`),
- an `EmptyState` centres itself.

That distinction is what lets one page carry both a form that wants a short
measure and a table that wants every pixel. Capping the PAGE gets the table
wrong to make the form right.

The two full-viewport cards -- `SignInPage` and `AuthCallbackPage` -- render
outside the shell and are the only exceptions. `Dialog` and the row-detail
aside are components with their own widths, not page frames.

**Why a rule and not taste.** Before memql#4262 six page roots hand-rolled
their own width and the rest did not, so the column jumped as an operator moved
between sections: Concepts and Integrations at `max-w-5xl`, campaign editing at
`3xl`, Sites full width but its own detail page at `3xl`. Every one of those was
a reasonable local choice. `portal_page_frame_test.go` at the repo root now
fails the build on a width token in a page root.

`portal_control_vocabulary_test.go` sits beside it and enforces this
directory's own rule the same way: a raw `<input>` / `<select>` / `<textarea>`
/ `<button>` outside `src/ui/`, a copy of the inset recipe, or `items-end` in
anything but the three allowlisted header layouts fails the build. Every
allowlist entry carries its reason. The rule was already written in this file
before that gate existed, and the drift accumulated anyway -- which is the
argument for the gate rather than for restating the rule.

## Composition rules

- Buttons: `Button` -- tones primary / quiet / danger, sizes sm / xs. One
  primary per screen at most; `type="submit"` is explicit.
- A button that NAVIGATES is `ButtonLink` -- same tones and sizes, one shared
  class recipe, but an `<a href>`. The element is the point: an anchor lets the
  browser own the gesture, which is what a `vscode://` deep link, middle-click
  and copy-link all need.
- Forms: `Field` wraps label/hint/error; `TextInput` / `Select` /
  `Textarea` share one inset style.
- **Controls in a row go inside `FormRow`, and the buttons that end that row go
  inside `FormActions`.** `FormRow` is `items-start`; `FormActions` reproduces
  the height of `Field`'s label line so its buttons top-align with the controls
  beside them. Buttons in a form row are size `sm` -- `sm` IS the control line.
- **`items-end` is banned in a form row.** Bottom-alignment makes the tallest
  child decide where everyone else's bottom edge lands, so a field carrying a
  hint pushes its hint-less neighbour's control off the line and a bare button
  parks on the hint's baseline. Nothing is wrong with any single control; the
  row is wrong. Top-alignment inverts it: everything starts at the same y and
  what hangs below is free to differ. The three surviving `items-end` uses are
  right-aligned page HEADER columns (`PageHeader`, `ComposeLayout`,
  `ComposerPage`), which are not form rows, and they are allowlisted by name.
- **A checkbox or a radio is `Checkbox` / `RadioGroup`, never a raw native.**
  Both keep a native `<input>` underneath -- `accent-color` recolours it and
  the keyboard, indeterminate state and form participation come free -- and
  both align the box to the FIRST line of the label, because these labels carry
  sentences and a centred box floats into the middle of a paragraph.
- A combo box is `TextInput list={...}`, not a page's own copy of the inset
  recipe.
- Secondary nav is always `Tabs` (routed underline strip).
- Sections are `Band` horizons; bare surfaces are `Panel`.
- Loading is a shaped `Skeleton`, never a spinner; empty is `EmptyState`
  with a verb where one exists; destructive actions confirm in
  `ConfirmDialog`.
- A note the reader has to act on (or decide not to) is a `Callout`: a title
  plus the consequence, tinted by tone. The title says the thing and the
  family tints it, the same rule `Badge` follows -- which is why it takes a
  title rather than colouring a bare paragraph. `role="alert"` is set for
  `danger` only: an alert interrupts a screen reader mid-sentence, which is
  right for "that write failed" and wrong for a standing observation that is
  true on every render.
- A person is an `Avatar`: two initials on an `accent-subtle` ground, sizes
  sm / md / lg. Three rules, and each is load-bearing rather than stylistic.
  **Initials only** -- `v1:identity:user` carries no image field, so there is
  nothing to render and a gravatar-style lookup would put an operator's email
  hash on a third-party wire to draw a circle. **`aria-hidden`, always** --
  the NAME is carried by the link or heading beside it, so an avatar that
  also announced it would read the same person twice; the component takes no
  `label` because there is no correct value for one. **One ground, not a
  per-person hue** -- colour-coding people encodes identity in a channel
  somebody may not be able to see, and it would have to stay legible in both
  themes beside the accent bar the nav rows already use.
- The five predefined view BODIES (`src/views/*View.tsx`) still may not
  contain raw row markup or iteration -- they compose `<ViewElement>` plus
  these primitives, and repo-root guard tests enforce it.
