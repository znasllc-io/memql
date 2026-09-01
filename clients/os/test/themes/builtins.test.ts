import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { BUILT_IN_PACKS, GRAPHITE } from "../../src/themes/builtins";
import { OS_THEME_TOKENS, validateThemePack } from "../../src/themes/pack";

const root = join(dirname(fileURLToPath(import.meta.url)), "../..");
const tokensCss = readFileSync(join(root, "src/styles/tokens.css"), "utf8");

/** The `--os-*` declarations inside the block containing `needle`. */
function blockContaining(needle: string): Record<string, string> {
  const at = tokensCss.indexOf(needle);
  if (at < 0) throw new Error(`tokens.css no longer contains ${needle}`);
  const open = tokensCss.lastIndexOf("{", at);
  const close = tokensCss.indexOf("}", at);
  const out: Record<string, string> = {};
  for (const line of tokensCss.slice(open + 1, close).split("\n")) {
    const m = /^\s*--os-([a-z-]+):\s*(.+);\s*$/.exec(line);
    if (m) out[m[1]!] = m[2]!.trim();
  }
  return out;
}

/** The block whose declarations are all `inherit` -- see the test below. */
function reinheritBlock(): Record<string, string> {
  return blockContaining("--os-ground: inherit;");
}

describe("the built-in packs", () => {
  it("all pass the loader they will be listed beside", () => {
    // A built-in that could not survive validateThemePack would be a pack the
    // marketplace shows and refuses to install a copy of -- and the loader's
    // only reachable positive is that the shipped packs go through it.
    for (const pack of BUILT_IN_PACKS) {
      const load = validateThemePack({ ...pack, builtIn: undefined });
      expect(load.ok, `${pack.id}: ${load.ok ? "" : load.detail}`).toBe(true);
    }
  });

  it("graphite's data is the stylesheet, value for value", () => {
    // GRAPHITE IS DUPLICATED, and this is what makes the duplication safe.
    // The CSS copy has to stay (it is the unqualified :root block, so the
    // shell paints on the first frame with none of this module run); the data
    // copy has to exist (the marketplace card draws each pack from its own
    // values, and graphite would otherwise be the one pack that could not
    // show itself). Drift between them is silent: the desktop would wear one
    // palette and its own card would advertise another.
    const dark = blockContaining('--os-ground: #07090a;');
    const light = blockContaining('--os-ground: #f2f4ef;');
    for (const token of OS_THEME_TOKENS) {
      expect(GRAPHITE.tokens.dark[token], `dark --os-${token}`).toBe(dark[token]);
      expect(GRAPHITE.tokens.light[token], `light --os-${token}`).toBe(light[token]);
    }
  });

  it("the token list is the one windows re-inherit", () => {
    // tokens.css re-inherits exactly the mode-dependent tokens onto every
    // window, widget and sheet root. That list IS the definition of what a
    // theme can change, so a token added to the shell and forgotten here
    // would be one a pack cannot set -- it would silently keep graphite's
    // value under every other theme.
    // Found by its CONTENT rather than by its selector: the selector list
    // appears twice in tokens.css (the structural block leads with `:root,`),
    // and matching on the text would silently read the wrong one.
    const inherited = reinheritBlock();
    expect(Object.keys(inherited).sort()).toEqual([...OS_THEME_TOKENS].sort());
  });

  it("keeps the accent off the status colours", () => {
    // The shell reads amber as `warn` and red as `error` everywhere. A theme
    // with a status-coloured accent puts that hue on every primary button and
    // live dot in the OS, and status stops meaning anything.
    for (const pack of BUILT_IN_PACKS) {
      for (const mode of ["dark", "light"] as const) {
        const tokens = pack.tokens[mode];
        expect(tokens.accent, `${pack.id} ${mode}`).not.toBe(tokens.warn);
        expect(tokens.accent, `${pack.id} ${mode}`).not.toBe(tokens.error);
        const { h } = hsl(tokens.accent);
        // Everything from red through yellow is status territory here.
        expect(h < 20 || h > 60, `${pack.id} ${mode} accent hue ${Math.round(h)} is status-coloured`).toBe(true);
      }
    }
  });

  it("each pack has its own wallpaper, not a copy of graphite's", () => {
    // A theme is its tokens PLUS its wallpaper parameters. A pack that ships
    // graphite's field is a recolour, and the contract says more than that.
    const fields = BUILT_IN_PACKS.map((p) => JSON.stringify(p.wallpaper));
    expect(new Set(fields).size).toBe(BUILT_IN_PACKS.length);
  });
});

/** Hue of a hex colour, 0-360. Only hex; every accent in the tree is one. */
function hsl(hex: string): { h: number } {
  const m = /^#([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) throw new Error(`not a hex colour: ${hex}`);
  const n = parseInt(m[1]!, 16);
  const r = ((n >> 16) & 255) / 255;
  const g = ((n >> 8) & 255) / 255;
  const b = (n & 255) / 255;
  const max = Math.max(r, g, b);
  const min = Math.min(r, g, b);
  const d = max - min;
  if (d === 0) return { h: 0 };
  let h: number;
  if (max === r) h = ((g - b) / d) % 6;
  else if (max === g) h = (b - r) / d + 2;
  else h = (r - g) / d + 4;
  h *= 60;
  return { h: h < 0 ? h + 360 : h };
}
