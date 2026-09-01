// The packs that ship in the bundle.
//
// ===========================================================================
// ONE LAW CONSTRAINS EVERY PALETTE: THE ACCENT IS NOT A STATUS COLOUR
// ===========================================================================
// This token set reserves amber for `warn` and red for `error`, and the whole
// shell reads them that way -- the provenance dot, the domain rail, a publish
// refusal. A theme with an amber or red ACCENT would put the status hue on
// every primary button, focus ring and live dot in the OS, and status would
// stop meaning anything. So a pack's accent comes from the cool half of the
// wheel, and the three below take green (the brand), indigo and cyan.
//
// That is a design law of this token system rather than taste, which is why
// it is written here rather than left for the next palette to rediscover.
//
// The second constraint is verified rather than asserted: every pack has to
// be legible in BOTH modes, and test/themes/contrast.test.ts computes the
// ratios. "Per-pack light and dark verification" is the epic's phrase, and a
// number is the only form of it that survives a redesign.

import type { OsThemePack } from "./pack";

/**
 * Graphite -- the shipped look, restated as data.
 *
 * IT IS DUPLICATED FROM tokens.css ON PURPOSE, and the duplication is gated
 * (test/themes/builtins.test.ts reads the stylesheet and compares every
 * value). The CSS copy has to stay: it is the unqualified `:root` block, so
 * the shell paints correctly on the first frame, offline, before any of this
 * module has run. The data copy has to exist: the marketplace card draws each
 * pack's two miniature desktops from its own values, and graphite would
 * otherwise be the one pack in the list that could not show itself.
 */
export const GRAPHITE: OsThemePack = {
  id: "graphite",
  name: "Graphite",
  version: "1.0.0",
  author: "MemQL",
  description: "The instrument look. Graphite grounds, brand green on signal duty.",
  builtIn: true,
  wallpaper: { seed: 9, cell: 110, density: 0.5, linkChance: 0.14, linkReach: 260 },
  tokens: {
    dark: {
      ground: "#07090a",
      plate: "#0b1110",
      raised: "#0e1311",
      ink: "#e8e6dd",
      muted: "#9ca395",
      line: "rgba(232, 230, 221, 0.08)",
      glass: "rgba(11, 17, 16, 0.55)",
      "glass-solid": "#0e1311",
      accent: "#5ccda7",
      "accent-fg": "#07090a",
      "accent-soft": "rgba(92, 205, 167, 0.12)",
      warn: "#e0a63c",
      error: "#e05b4d",
      "shadow-item": "0 1px 2px rgba(0, 0, 0, 0.3)",
      "shadow-float": "0 12px 32px rgba(0, 0, 0, 0.42)",
      "shadow-window": "0 24px 64px rgba(0, 0, 0, 0.5)",
      "field-dot": "rgba(232, 230, 221, 0.055)",
      "field-link": "rgba(92, 205, 167, 0.05)",
      "field-numeral": "rgba(232, 230, 221, 0.04)",
      rail: "rgba(232, 230, 221, 0.14)",
      "rail-hover": "rgba(232, 230, 221, 0.3)",
    },
    light: {
      ground: "#f2f4ef",
      plate: "#ffffff",
      raised: "#e9ede6",
      ink: "#191d1a",
      muted: "#5b615c",
      line: "rgba(25, 29, 26, 0.1)",
      glass: "rgba(255, 255, 255, 0.62)",
      "glass-solid": "#fbfcfa",
      accent: "#047d5a",
      "accent-fg": "#ffffff",
      "accent-soft": "rgba(4, 125, 90, 0.1)",
      warn: "#a86a08",
      error: "#b23b2e",
      "shadow-item": "0 1px 2px rgba(25, 29, 26, 0.08)",
      "shadow-float": "0 12px 32px rgba(25, 29, 26, 0.12)",
      "shadow-window": "0 24px 64px rgba(25, 29, 26, 0.16)",
      "field-dot": "rgba(25, 29, 26, 0.05)",
      "field-link": "rgba(4, 125, 90, 0.05)",
      "field-numeral": "rgba(25, 29, 26, 0.045)",
      rail: "rgba(25, 29, 26, 0.16)",
      "rail-hover": "rgba(25, 29, 26, 0.34)",
    },
  },
};

/**
 * Vellum -- warm paper and drafting ink.
 *
 * The foundation's light mode is porcelain, "never cream", and that was the
 * right call for the default: cool grounds keep a dense operator console
 * calm. Vellum is the deliberate opposite and exists to prove the contract
 * carries a whole CHARACTER rather than a hue -- warm paper, warm ink, and an
 * indigo accent that reads as a pen rather than a highlight. Its wallpaper is
 * sparser and longer-linked than graphite's: fewer marks, more ruling.
 */
export const VELLUM: OsThemePack = {
  id: "vellum",
  name: "Vellum",
  version: "1.0.0",
  author: "MemQL",
  description: "Warm paper and drafting ink. Quieter grounds, indigo instead of green.",
  builtIn: true,
  wallpaper: { seed: 17, cell: 150, density: 0.34, linkChance: 0.2, linkReach: 320 },
  tokens: {
    dark: {
      ground: "#171410",
      plate: "#1e1a15",
      raised: "#241f19",
      ink: "#ece5d6",
      muted: "#a09585",
      line: "rgba(236, 229, 214, 0.1)",
      glass: "rgba(30, 26, 21, 0.58)",
      "glass-solid": "#231e18",
      accent: "#8f9ee8",
      "accent-fg": "#171410",
      "accent-soft": "rgba(143, 158, 232, 0.14)",
      warn: "#dda63f",
      error: "#e0705d",
      "shadow-item": "0 1px 2px rgba(0, 0, 0, 0.34)",
      "shadow-float": "0 12px 32px rgba(0, 0, 0, 0.46)",
      "shadow-window": "0 24px 64px rgba(0, 0, 0, 0.54)",
      "field-dot": "rgba(236, 229, 214, 0.05)",
      "field-link": "rgba(143, 158, 232, 0.05)",
      "field-numeral": "rgba(236, 229, 214, 0.04)",
      rail: "rgba(236, 229, 214, 0.14)",
      "rail-hover": "rgba(236, 229, 214, 0.3)",
    },
    light: {
      ground: "#f0ece2",
      plate: "#faf7f0",
      raised: "#e6e0d2",
      ink: "#26221c",
      muted: "#655e54",
      line: "rgba(38, 34, 28, 0.12)",
      glass: "rgba(250, 247, 240, 0.66)",
      "glass-solid": "#f6f2e9",
      accent: "#3d4ea8",
      "accent-fg": "#ffffff",
      "accent-soft": "rgba(61, 78, 168, 0.12)",
      warn: "#8a5c08",
      error: "#a83c2c",
      "shadow-item": "0 1px 2px rgba(38, 34, 28, 0.09)",
      "shadow-float": "0 12px 32px rgba(38, 34, 28, 0.13)",
      "shadow-window": "0 24px 64px rgba(38, 34, 28, 0.17)",
      "field-dot": "rgba(38, 34, 28, 0.06)",
      "field-link": "rgba(61, 78, 168, 0.06)",
      "field-numeral": "rgba(38, 34, 28, 0.05)",
      rail: "rgba(38, 34, 28, 0.18)",
      "rail-hover": "rgba(38, 34, 28, 0.36)",
    },
  },
};

/**
 * Cobalt -- a night bridge.
 *
 * Where graphite is neutral and vellum is warm, cobalt is cold: grounds with
 * a blue cast and a cyan accent, for the long low-light sitting that operator
 * work actually is. Its wallpaper is the densest of the three and its links
 * are the shortest -- a close-range field rather than a sparse lattice.
 */
export const COBALT: OsThemePack = {
  id: "cobalt",
  name: "Cobalt",
  version: "1.0.0",
  author: "MemQL",
  description: "A cold night bridge. Blue grounds, cyan signal, the densest field.",
  builtIn: true,
  wallpaper: { seed: 31, cell: 88, density: 0.6, linkChance: 0.1, linkReach: 200 },
  tokens: {
    dark: {
      ground: "#050a12",
      plate: "#0a1220",
      raised: "#0e1828",
      ink: "#dee8f5",
      muted: "#8d9cb2",
      line: "rgba(222, 232, 245, 0.09)",
      glass: "rgba(10, 18, 32, 0.58)",
      "glass-solid": "#0d1524",
      accent: "#49b8f0",
      "accent-fg": "#050a12",
      "accent-soft": "rgba(73, 184, 240, 0.13)",
      warn: "#e0a63c",
      error: "#e8615a",
      "shadow-item": "0 1px 2px rgba(0, 0, 0, 0.35)",
      "shadow-float": "0 12px 32px rgba(0, 0, 0, 0.5)",
      "shadow-window": "0 24px 64px rgba(0, 0, 0, 0.58)",
      "field-dot": "rgba(222, 232, 245, 0.06)",
      "field-link": "rgba(73, 184, 240, 0.06)",
      "field-numeral": "rgba(222, 232, 245, 0.045)",
      rail: "rgba(222, 232, 245, 0.15)",
      "rail-hover": "rgba(222, 232, 245, 0.32)",
    },
    light: {
      ground: "#eef2f7",
      plate: "#ffffff",
      raised: "#e2e9f2",
      ink: "#111a26",
      muted: "#55627a",
      line: "rgba(17, 26, 38, 0.1)",
      glass: "rgba(255, 255, 255, 0.64)",
      "glass-solid": "#f8fafd",
      accent: "#0b6fa8",
      "accent-fg": "#ffffff",
      "accent-soft": "rgba(11, 111, 168, 0.1)",
      warn: "#9c6206",
      error: "#b0392f",
      "shadow-item": "0 1px 2px rgba(17, 26, 38, 0.08)",
      "shadow-float": "0 12px 32px rgba(17, 26, 38, 0.12)",
      "shadow-window": "0 24px 64px rgba(17, 26, 38, 0.16)",
      "field-dot": "rgba(17, 26, 38, 0.055)",
      "field-link": "rgba(11, 111, 168, 0.06)",
      "field-numeral": "rgba(17, 26, 38, 0.045)",
      rail: "rgba(17, 26, 38, 0.16)",
      "rail-hover": "rgba(17, 26, 38, 0.34)",
    },
  },
};

/**
 * Graphite is FIRST, and that is the fallback order too: an id naming nothing
 * installed resolves to this one, because it is the pack whose CSS is already
 * in the bundle unconditionally.
 */
export const BUILT_IN_PACKS: readonly OsThemePack[] = [GRAPHITE, VELLUM, COBALT];

export const BUILT_IN_THEME_ID = GRAPHITE.id;

export function isBuiltInId(id: string): boolean {
  return BUILT_IN_PACKS.some((p) => p.id === id);
}
