// The theme-pack format, and the loader that refuses a broken one.
//
// ===========================================================================
// A PACK IS DATA, NOT A STYLESHEET
// ===========================================================================
// The foundation's registry type carried `tokensHref` -- "where the pack's
// token stylesheet lives" -- and that is the one part of spec G this epic
// changes, for two reasons that both point the same way.
//
// It cannot be VALIDATED. The epic requires a loader that refuses an
// incomplete token set, because "a theme that omits a token silently inherits
// another theme's color, which reads as a broken product". A fetched
// stylesheet is opaque to the page that fetched it; the only way to learn what
// it defines is to apply it and look.
//
// It cannot be TRUSTED. A stylesheet is arbitrary CSS. A pack that sets
// `--os-cell-w` breaks the desk grid, one that zeroes `--os-duration-cue`
// silently disables the arrival cue for a reader who never asked for reduced
// motion, and one carrying a `content:` rule can write text onto the shell.
//
// So a pack is JSON carrying VALUES, and the CSS is something this file
// writes. Everything a pack can say is a colour or a depth; the tokens that
// decide ERGONOMICS -- the type scale, the radii, the grid cell, the motion
// durations -- are not in the format at all. A theme changes how the OS
// LOOKS. It cannot change how it behaves or where anything is.

/**
 * The mode-dependent tokens, in `tokens.css`'s own order.
 *
 * This is the list `[data-os-window], [data-os-widget], [data-os-sheet]`
 * re-inherits, which is what makes it the canonical one -- a token absent
 * from there does not reach a window and is not part of a theme.
 * `test/themes/tokens.test.ts` reads tokens.css and fails when the two
 * disagree, so a token cannot be added to the shell and forgotten here.
 */
export const OS_THEME_TOKENS = [
  "ground",
  "plate",
  "raised",
  "ink",
  "muted",
  "line",
  "glass",
  "glass-solid",
  "accent",
  "accent-fg",
  "accent-soft",
  "warn",
  "error",
  "shadow-item",
  "shadow-float",
  "shadow-window",
  "field-dot",
  "field-link",
  "field-numeral",
  "rail",
  "rail-hover",
] as const;

export type OsThemeToken = (typeof OS_THEME_TOKENS)[number];
export type OsThemeTokens = Record<OsThemeToken, string>;

/** The wallpaper's geometry. Mirrors wallpaper/field.ts's FieldOptions. */
export interface OsThemeWallpaper {
  seed: number;
  cell: number;
  density: number;
  linkChance: number;
  linkReach: number;
}

export interface OsThemePack {
  /** Stable id; the value of `data-os-theme` on the document root. */
  id: string;
  /** Human label for the picker and the marketplace card. */
  name: string;
  /** The pack's own version. Displayed, never compared. */
  version: string;
  author: string;
  /** One line for the card. Optional -- a pack may let its colours speak. */
  description?: string;
  /** Both looks. A pack defining one mode is not a theme (see below). */
  tokens: { dark: OsThemeTokens; light: OsThemeTokens };
  wallpaper: OsThemeWallpaper;
  /**
   * True for a pack that ships in the bundle. Built-ins render on the first
   * frame offline and are never written to the desktop document -- the bundle
   * already has them, and a stored copy that outlived a release would be a
   * stale theme nobody could update.
   */
  builtIn?: boolean;
}

/** Why a pack was refused. The surface renders `detail`, not this. */
export type PackRefusal =
  | "not-a-pack"
  | "bad-id"
  | "bad-field"
  | "missing-tokens"
  | "bad-token-value"
  | "bad-wallpaper";

export type PackLoad =
  | { ok: true; pack: OsThemePack }
  | { ok: false; refusal: PackRefusal; detail: string };

const ID = /^[a-z][a-z0-9-]{1,31}$/;
const TEXT = /^[^\u0000-\u001f]{1,64}$/;
/**
 * A description is a SENTENCE, so it gets its own bound.
 *
 * One cap for both was the first cut, and every built-in pack failed it --
 * "The instrument look. Graphite grounds, brand green on signal duty." is 66
 * characters. A name and a description are different lengths of thing, and a
 * shared constant is how a card ends up with a truncated sentence or a
 * paragraph where a label goes.
 */
const SENTENCE = /^[^\u0000-\u001f]{1,160}$/;

/**
 * The characters a token value may contain.
 *
 * A WHITELIST, deliberately -- the same posture component/edge's validHost
 * takes, for the same reason: a denylist of `;{}` would miss the next
 * character that turns out to matter. Every value in tokens.css is a hex
 * colour, an `rgba(...)` or a shadow triple, and all three live inside this
 * set. What the set EXCLUDES is the interesting part: no `;` or `}` to close
 * the declaration and open another, no `:` or `/` so a `url(` cannot name
 * anything, and no quotes.
 */
const VALUE = /^[A-Za-z0-9 ,.()#%-]{1,160}$/;

export function validateThemePack(raw: unknown): PackLoad {
  if (!raw || typeof raw !== "object") {
    return { ok: false, refusal: "not-a-pack", detail: "That file is not a theme pack." };
  }
  const doc = raw as Partial<OsThemePack>;

  if (typeof doc.id !== "string" || !ID.test(doc.id)) {
    return {
      ok: false,
      refusal: "bad-id",
      detail: "A theme id is 2 to 32 characters: lowercase letters, digits and hyphens.",
    };
  }
  for (const [field, value] of [
    ["name", doc.name],
    ["version", doc.version],
    ["author", doc.author],
  ] as const) {
    if (typeof value !== "string" || !TEXT.test(value)) {
      return { ok: false, refusal: "bad-field", detail: `This theme has no usable ${field}.` };
    }
  }
  if (
    doc.description !== undefined &&
    (typeof doc.description !== "string" || !SENTENCE.test(doc.description))
  ) {
    return { ok: false, refusal: "bad-field", detail: "This theme's description is not usable." };
  }

  const tokens: { dark?: OsThemeTokens; light?: OsThemeTokens } = {};
  for (const mode of ["dark", "light"] as const) {
    const given = (doc.tokens as Record<string, unknown> | undefined)?.[mode];
    if (!given || typeof given !== "object") {
      return {
        ok: false,
        refusal: "missing-tokens",
        detail: `This theme defines no ${mode} mode. A theme has to define both -- light or dark is the reader's choice, not the theme's.`,
      };
    }
    const bag = given as Record<string, unknown>;
    const missing = OS_THEME_TOKENS.filter((t) => typeof bag[t] !== "string" || bag[t] === "");
    if (missing.length > 0) {
      const noun = missing.length === 1 ? "colour" : "colours";
      return {
        ok: false,
        refusal: "missing-tokens",
        detail: `This theme is missing ${missing.length} ${mode} ${noun}: ${missing.join(", ")}.`,
      };
    }
    const bad = OS_THEME_TOKENS.find((t) => !VALUE.test(String(bag[t])));
    if (bad) {
      return {
        ok: false,
        refusal: "bad-token-value",
        detail: `This theme's ${mode} ${bad} is not a colour this shell will use.`,
      };
    }
    tokens[mode] = Object.fromEntries(
      OS_THEME_TOKENS.map((t) => [t, String(bag[t])]),
    ) as OsThemeTokens;
  }

  const wallpaper = validateWallpaper(doc.wallpaper);
  if (!wallpaper) {
    return {
      ok: false,
      refusal: "bad-wallpaper",
      detail: "This theme's wallpaper settings are missing or out of range.",
    };
  }

  return {
    ok: true,
    pack: {
      id: doc.id,
      name: doc.name as string,
      version: doc.version as string,
      author: doc.author as string,
      ...(doc.description ? { description: doc.description } : {}),
      tokens: { dark: tokens.dark!, light: tokens.light! },
      wallpaper,
    },
  };
}

/**
 * Wallpaper numbers, each bounded.
 *
 * The bounds are not taste. `cell` at 4 makes the field a solid sheet of dots
 * that pins a laptop GPU; `density` at 1 with a small cell is the same thing;
 * an unbounded `linkReach` makes every dot a candidate for every other and
 * turns generation quadratic across the whole screen. A pack must not be able
 * to ship an OS that will not scroll.
 */
function validateWallpaper(raw: unknown): OsThemeWallpaper | null {
  if (!raw || typeof raw !== "object") return null;
  const w = raw as Record<string, unknown>;
  const num = (key: string, min: number, max: number): number | null => {
    const v = w[key];
    if (typeof v !== "number" || !Number.isFinite(v) || v < min || v > max) return null;
    return v;
  };
  const seed = num("seed", 0, 2 ** 31);
  const cell = num("cell", 40, 400);
  const density = num("density", 0, 1);
  const linkChance = num("linkChance", 0, 1);
  const linkReach = num("linkReach", 0, 600);
  if (
    seed === null ||
    cell === null ||
    density === null ||
    linkChance === null ||
    linkReach === null
  ) {
    return null;
  }
  return { seed, cell, density, linkChance, linkReach };
}

/**
 * The CSS for one pack.
 *
 * Four blocks, in tokens.css's own order, and the ORDER IS LOAD-BEARING: the
 * two `prefers-color-scheme` blocks and the two explicit `data-theme` blocks
 * have equal specificity, so an explicit light/dark choice only wins by coming
 * last. Every selector carries `[data-os-theme="<id>"]`, one attribute more
 * specific than the built-in's bare `:root` -- graphite stays the unqualified
 * default and a pack overrides it, rather than the shell rendering unstyled
 * until an attribute lands.
 */
export function themePackCss(pack: OsThemePack): string {
  const block = (selector: string, tokens: OsThemeTokens, scheme: string | null) =>
    [
      `${selector} {`,
      ...(scheme ? [`  color-scheme: ${scheme};`] : []),
      ...OS_THEME_TOKENS.map((t) => `  --os-${t}: ${tokens[t]};`),
      "}",
    ].join("\n");

  const root = `:root[data-os-theme="${pack.id}"]`;
  return [
    "@media (prefers-color-scheme: dark) {",
    block(`${root}:not([data-theme="light"])`, pack.tokens.dark, null),
    "}",
    "@media (prefers-color-scheme: light) {",
    block(`${root}:not([data-theme="dark"])`, pack.tokens.light, null),
    "}",
    block(`${root}[data-theme="dark"]`, pack.tokens.dark, "dark"),
    block(`${root}[data-theme="light"]`, pack.tokens.light, "light"),
  ].join("\n");
}
