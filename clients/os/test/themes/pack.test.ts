import { describe, expect, it } from "vitest";

import { GRAPHITE } from "../../src/themes/builtins";
import {
  OS_THEME_TOKENS,
  themePackCss,
  validateThemePack,
  type OsThemePack,
} from "../../src/themes/pack";

// The loader is the epic's stated requirement: "a loader that refuses
// incomplete token sets -- a theme that omits a token silently inherits
// another theme's color, which reads as a broken product". Every refusal
// below is a named one, because an author handed "invalid theme" has nowhere
// to start.

function pack(mutate: (p: Record<string, unknown>) => void = () => {}): Record<string, unknown> {
  const base = JSON.parse(JSON.stringify({ ...GRAPHITE, id: "test-pack", builtIn: undefined }));
  delete base.builtIn;
  mutate(base);
  return base;
}

describe("validateThemePack", () => {
  it("accepts a complete pack", () => {
    const load = validateThemePack(pack());
    expect(load.ok).toBe(true);
  });

  it("names the colours a pack is missing, and which mode", () => {
    const load = validateThemePack(
      pack((p) => {
        const light = (p.tokens as Record<string, Record<string, string>>).light!;
        delete light.rail;
        delete light["rail-hover"];
      }),
    );
    expect(load.ok).toBe(false);
    if (load.ok) return;
    expect(load.refusal).toBe("missing-tokens");
    expect(load.detail).toContain("2 light colours");
    expect(load.detail).toContain("rail");
    expect(load.detail).toContain("rail-hover");
  });

  it("refuses a pack that defines only one mode", () => {
    const load = validateThemePack(
      pack((p) => {
        delete (p.tokens as Record<string, unknown>).light;
      }),
    );
    expect(load.ok).toBe(false);
    if (load.ok) return;
    expect(load.detail).toContain("defines no light mode");
  });

  it("refuses a value that could escape its own declaration", () => {
    // The whitelist is the point: a pack is data from somewhere else, and the
    // CSS is written by us. `;` closes the declaration, `}` closes the block.
    for (const hostile of [
      "red; } :root { display: none",
      "url(https://elsewhere.test/x.png)",
      'red" onload="x',
      "var(--os-ground)/**/",
    ]) {
      const load = validateThemePack(
        pack((p) => {
          (p.tokens as Record<string, Record<string, string>>).dark!.accent = hostile;
        }),
      );
      expect(load.ok, `accepted ${hostile}`).toBe(false);
      if (!load.ok) expect(load.refusal).toBe("bad-token-value");
    }
  });

  it("refuses wallpaper numbers that would pin a GPU", () => {
    for (const wallpaper of [
      { seed: 1, cell: 2, density: 1, linkChance: 1, linkReach: 600 },
      { seed: 1, cell: 110, density: 4, linkChance: 0.1, linkReach: 260 },
      { seed: 1, cell: 110, density: 0.5, linkChance: 0.1, linkReach: 99999 },
      { seed: 1, cell: 110, density: 0.5, linkChance: 0.1 },
    ]) {
      const load = validateThemePack(pack((p) => void (p.wallpaper = wallpaper)));
      expect(load.ok, JSON.stringify(wallpaper)).toBe(false);
      if (!load.ok) expect(load.refusal).toBe("bad-wallpaper");
    }
  });

  it("refuses an id that is not an id, and one that is not a pack at all", () => {
    expect(validateThemePack(pack((p) => void (p.id = "Not An Id"))).ok).toBe(false);
    expect(validateThemePack(pack((p) => void (p.id = "x"))).ok).toBe(false);
    expect(validateThemePack("a string").ok).toBe(false);
    expect(validateThemePack(null).ok).toBe(false);
  });

  it("drops fields it was not asked for", () => {
    const load = validateThemePack(
      pack((p) => {
        p.builtIn = true;
        (p.tokens as Record<string, Record<string, string>>).dark!.somethingElse = "#fff";
      }),
    );
    expect(load.ok).toBe(true);
    if (!load.ok) return;
    // `builtIn` decides whether a pack is stored in the desktop document; a
    // downloaded file claiming it would make itself unstorable.
    expect(load.pack.builtIn).toBeUndefined();
    expect(Object.keys(load.pack.tokens.dark).sort()).toEqual([...OS_THEME_TOKENS].sort());
  });
});

describe("themePackCss", () => {
  const css = themePackCss({ ...(GRAPHITE as OsThemePack), id: "test-pack" });

  it("writes both modes twice, in tokens.css's own order", () => {
    // The two prefers-color-scheme blocks and the two explicit data-theme
    // blocks have equal specificity, so the explicit choice only wins by
    // coming last. Reversing them is a one-line edit that breaks the mode
    // toggle for installed packs only, which nothing else would catch.
    const order = [
      '@media (prefers-color-scheme: dark)',
      '@media (prefers-color-scheme: light)',
      ':root[data-os-theme="test-pack"][data-theme="dark"]',
      ':root[data-os-theme="test-pack"][data-theme="light"]',
    ];
    let at = -1;
    for (const needle of order) {
      const found = css.indexOf(needle, at + 1);
      expect(found, `${needle} out of order`).toBeGreaterThan(at);
      at = found;
    }
  });

  it("is more specific than the built-in's bare :root", () => {
    // Graphite is the unqualified :root block, so a pack must OVERRIDE it
    // rather than replace it -- one attribute is exactly enough.
    expect(css).toContain(':root[data-os-theme="test-pack"]');
    expect(css).not.toMatch(/^:root \{/m);
  });

  it("emits every token, and only tokens", () => {
    for (const token of OS_THEME_TOKENS) {
      expect(css).toContain(`--os-${token}:`);
    }
    // No ergonomics. A pack cannot move the grid, resize the type or zero the
    // motion tokens somebody's reduced-motion setting is meant to own.
    for (const forbidden of ["--os-cell-w", "--os-duration-cue", "--os-radius", "--os-font"]) {
      expect(css).not.toContain(forbidden);
    }
  });
});
