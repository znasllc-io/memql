# brand/ -- one source for what MemQL looks like

Two surfaces wear this brand, and neither owns it:

| Surface | Build | How it imports these files |
|---|---|---|
| `clients/portal` | Tailwind v4 via `@tailwindcss/vite`, bundled by Vite | `@import` from `src/styles/index.css` |
| `component/identity/web` | Tailwind v4 standalone CLI, output embedded in the Go binary | `@import` from `tailwind/input.css` |

One is a TypeScript app, the other a Go binary. They share no package manager
and no config format, so the shared layer is **plain CSS custom properties** --
the one format both consume -- and it sits at the repo root because neither
side owns it.

## The files

| File | What it is |
|---|---|
| `tokens.css` | The palette, type scale and radius rhythm as `--memql-*` roles. One `light-dark()` definition per colour; `color-scheme` is the entire theme switch. |
| `theme.css` | The Tailwind v4 `@theme inline` bridge, plus the document-level base layer. `inline` is load-bearing -- see the note in the file. |
| `fonts.css` | `@font-face` for the three faces. Relative `./fonts/` URLs, which both builds resolve correctly in different ways -- see the note in the file. |
| `fonts/` | Inter Variable, JetBrains Mono (400/500/700), Squada One. Latin subsets, woff2, 132 KB total. |
| `mark.svg` | The 9-node graph polyhedron, in `currentColor`. |
| `favicon.svg` | The same mark, with the accent baked per `prefers-color-scheme` (a favicon has no inherited colour to take). |

## The rule

**Import these files. Never copy them.** `brand_shared_source_test.go` at the
repo root fails the build when either surface defines a `--memql-*` token, an
`@theme` block, or an `@font-face` for a brand face of its own -- because two
copies of a palette is two palettes, and they diverge in the week nobody is
looking.

What is *not* shared, deliberately:

- **view-kit's `--vk-*` contract** stays in `clients/portal/src/styles/viewkit.css`.
  view-kit is a portal concern; the identity pages render no charts.
- **Per-customer branding** is a runtime override, not a file here. Identity
  serves `GET /static/brand.css`, which emits a `:root` block over these
  defaults from cluster settings (memql#4269). The overridable set is small and
  named on purpose: an operator can put their colour on the sign-in page, and
  cannot make the product unreadable through a settings form.

## Changing a colour

Change it here, once. Then check the contrast table in `tokens.css` still
tells the truth -- it is measured, and a value moving without the table moving
is how a palette quietly stops meeting WCAG.
