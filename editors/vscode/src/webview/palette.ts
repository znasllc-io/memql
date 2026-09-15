// MemQL's editor and webview palette. Brand accents, data tints and light
// neutrals follow brand/tokens.css. The dark working surfaces are deliberately
// lifted toward the brand foreground for long editing sessions (owner request).
// This adaptation is local to the extension; it does not repaint MemQL OS.
// Tests compare the unchanged roles with the canonical brand source and gate
// contrast. Both native themes are generated from this same palette.

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

/** The light palette: current canonical brand neutrals and accents. */
export const LIGHT: Palette = {
  bg: "#f7f7f5",
  surface: "#ffffff",
  raised: "#efefec",
  border: "#e5e6e2",
  "border-strong": "#d2d4cf",
  fg: "#191d1a",
  muted: "#5b615c",
  subtle: "#7e837b",
  accent: "#047d5a",
  "accent-deep": "#026842",
  "on-accent": "#ffffff",
  "on-accent-hover": "#ffffff",
  danger: "#b3362a",
  "data-number": "#0f766e",
  "data-string": "#b45309",
};

// An sRGB lift of existing brand surfaces, rather than an unrelated dark hue.
// Small, ordered lifts keep the editor, rail, hover and selection distinct.
function lift(surface: string, amount: number): string {
  const foreground = "#e8e6dd";
  return "#" + [1, 3, 5].map((offset) => {
    const base = parseInt(surface.slice(offset, offset + 2), 16);
    const ink = parseInt(foreground.slice(offset, offset + 2), 16);
    return Math.round(base + (ink - base) * amount).toString(16).padStart(2, "0");
  }).join("");
}

/** Dark charcoal work surfaces with the canonical mint/amber syntax accents. */
export const DARK: Palette = {
  bg: lift("#07090a", 0.09),
  surface: lift("#0b1110", 0.10),
  raised: lift("#0e1311", 0.12),
  border: lift("#18231e", 0.12),
  "border-strong": lift("#213029", 0.16),
  fg: "#e8e6dd",
  muted: "#9ca395",
  subtle: "#6c726a",
  accent: "#5ccda7",
  "accent-deep": "#026842",
  "on-accent": "#07090a",
  "on-accent-hover": "#ffffff",
  danger: "#e0705f",
  "data-number": "#98ffe0",
  "data-string": "#cbb083",
};
