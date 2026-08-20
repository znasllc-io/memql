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

## Composition rules

- Buttons: `Button` -- tones primary / quiet / danger, sizes sm / xs. One
  primary per screen at most; `type="submit"` is explicit.
- Forms: `Field` wraps label/hint/error; `TextInput` / `Select` /
  `Textarea` share one inset style.
- Secondary nav is always `Tabs` (routed underline strip).
- Sections are `Band` horizons; bare surfaces are `Panel`.
- Loading is a shaped `Skeleton`, never a spinner; empty is `EmptyState`
  with a verb where one exists; destructive actions confirm in
  `ConfirmDialog`.
- The five predefined view BODIES (`src/views/*View.tsx`) still may not
  contain raw row markup or iteration -- they compose `<ViewElement>` plus
  these primitives, and repo-root guard tests enforce it.
