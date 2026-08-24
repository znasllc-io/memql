// The MemQL palette, as data (memql#4419, D3).
//
// UPSTREAM IS `brand/` AT THE REPOSITORY ROOT, and the hexes below are
// memql#4177's exactly -- the same values the portal redesign ships and
// memql.io renders. They are transcribed rather than imported: `brand/` is CSS
// consumed by two build systems that share no package manager
// (clients/portal's Vite and component/identity/web's Tailwind CLI), and this
// extension is a third with a strict webview CSP that loads no external
// stylesheet at all. brand_shared_source_test.go scans those two trees and
// deliberately does not scan this one; memql#4196's header records why. That
// makes drift POSSIBLE at the moment the portal palette changes, so the
// mitigation is procedural and written down: editors/vscode/README.md's
// Appearance section carries the release step, and this header names the
// source so nobody has to guess where the truth is.
//
// WHY DATA AND NOT CSS. These values used to live inside brandStyleBlock() as
// CSS text, which was fine while CSS was their only consumer. It stopped being
// fine when the extension had to emit VS Code color-theme JSON from the same
// palette (memql#4420): a generator cannot read a template literal, so the
// alternative was a second hand-typed copy of every hex in the theme files --
// exactly the drift that this repo's generate-then-gate pairs exist to
// prevent. One map, three consumers: brandStyleBlock(), buildEditorTheme(),
// and scripts/generate-themes.mjs through the second of those.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

/**
 * The token names, which are also the CSS custom-property suffixes:
 * `"border-strong"` is emitted as `--memql-border-strong` and read as
 * `var(--memql-border-strong)`.
 *
 * Kebab-case rather than camelCase so there is NO derivation between the key
 * and the property name. A `camelToKebab()` helper would be one more thing
 * that can be wrong, and its failure mode is a custom property nothing
 * defines -- which CSS resolves to the initial value in silence.
 */
export type PaletteKey =
  | "bg"
  | "surface"
  | "raised"
  | "border"
  | "border-strong"
  | "fg"
  | "muted"
  | "subtle"
  | "accent"
  | "accent-deep"
  | "on-accent"
  | "on-accent-hover"
  | "danger"
  | "data-number"
  | "data-string";

/** One theme's worth of values. Typed as a total record, so a missing token is a compile error. */
export type Palette = Readonly<Record<PaletteKey, string>>;

/**
 * Every token, in emission order.
 *
 * The order is the order the CSS blocks and the reviewer's eye both take, so
 * the light and dark blocks of `brandStyleBlock()` line up line for line in a
 * diff.
 */
export const PALETTE_KEYS: readonly PaletteKey[] = [
  "bg",
  "surface",
  "raised",
  "border",
  "border-strong",
  "fg",
  "muted",
  "subtle",
  "accent",
  "accent-deep",
  "on-accent",
  "on-accent-hover",
  "danger",
  "data-number",
  "data-string",
];

/** The light palette -- the portal's default, and this extension's. */
export const LIGHT: Palette = {
  bg: "#f2f4ef",
  surface: "#ffffff",
  raised: "#e9ede6",
  border: "#d6ddd4",
  "border-strong": "#c2cabf",
  fg: "#14201a",
  muted: "#586159",
  subtle: "#7c847b",
  accent: "#047d5a",
  "accent-deep": "#026842",
  "on-accent": "#ffffff",
  "on-accent-hover": "#ffffff",
  danger: "#b42318",
  "data-number": "#0f766e",
  "data-string": "#b45309",
};

/** The dark palette. */
export const DARK: Palette = {
  bg: "#07090a",
  surface: "#0b1110",
  raised: "#0e1311",
  border: "#18231e",
  "border-strong": "#213029",
  fg: "#e8e6dd",
  muted: "#9ca395",
  subtle: "#6c726a",
  accent: "#5ccda7",
  "accent-deep": "#026842",
  "on-accent": "#052e21",
  "on-accent-hover": "#ffffff",
  danger: "#f97066",
  "data-number": "#98ffe0",
  "data-string": "#cbb083",
};
