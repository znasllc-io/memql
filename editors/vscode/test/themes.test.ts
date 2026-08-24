// The two committed editor colour themes, and the gate that keeps them honest
// (memql#4420, D3).
//
// WHY THEME FILES EXIST AT ALL. Tree rows, the activity bar, tabs and the
// status bar are drawn by the WORKBENCH. An extension cannot style them -- it
// gets ThemeIcon and ThemeColor and nothing else, which brandTokens.ts has
// recorded since memql#4196. So the only way the chrome around the MemQL
// panels can wear the brand is for VS Code's OWN theme to be a MemQL theme.
// Hence two of them, opt-in from the theme picker.
//
// WHY THEY ARE COMMITTED AND ALSO GENERATED. A VSIX packs FILES, not build
// steps, so the JSON has to be in the tree. But a hand-maintained theme file
// is a second copy of the palette, and the whole reason palette.ts exists is
// that a second copy drifts. So: one generator, output committed, and this
// file regenerates in memory and compares. Same generate-then-gate shape as
// `make frontdoor` / `make frontdoor-*-check`.
//
// THE COMPARISON IS BYTE-FOR-BYTE, not structural. A structural comparison
// would let the committed file drift in formatting until a reviewer could no
// longer read its diff, and would quietly accept a file someone hand-edited in
// a way that happened to parse the same. "Run the generator and commit what it
// wrote" is the only way to satisfy this, which is exactly the contract.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import {
  THEME_FILES,
  THEME_NAMES,
  buildEditorTheme,
  serializeTheme,
  type ThemeVariant,
} from "../src/theme/editorThemes.js";
import { DARK, LIGHT } from "../src/webview/palette.js";

const ROOT = path.resolve(__dirname, "..", "..");
const VARIANTS: ThemeVariant[] = ["dark", "light"];

interface Manifest {
  contributes: {
    themes?: { label: string; uiTheme: string; path: string }[];
  };
}

const manifest = JSON.parse(
  fs.readFileSync(path.join(ROOT, "package.json"), "utf8"),
) as Manifest;

// -----------------------------------------------------------------------------
// The drift gate
// -----------------------------------------------------------------------------

for (const variant of VARIANTS) {
  test(`the committed ${variant} theme is what the generator emits`, () => {
    const file = path.join(ROOT, THEME_FILES[variant]);
    assert.ok(
      fs.existsSync(file),
      `${THEME_FILES[variant]} is missing -- run \`node scripts/generate-themes.mjs\` and commit it`,
    );
    assert.equal(
      fs.readFileSync(file, "utf8"),
      serializeTheme(buildEditorTheme(variant)),
      `${THEME_FILES[variant]} disagrees with palette.ts -- run \`node scripts/generate-themes.mjs\` and commit both`,
    );
  });
}

test("the drift gate can actually fail", () => {
  // The reachable positive. Both assertions above compare a file against a
  // function; if the function ever returned the file's own contents by some
  // accident of caching, they would pass forever while checking nothing. This
  // shows the comparison discriminates: a theme built from a DIFFERENT palette
  // must not match the committed dark file.
  const committed = fs.readFileSync(path.join(ROOT, THEME_FILES.dark), "utf8");
  assert.notEqual(committed, serializeTheme(buildEditorTheme("light")));
});

// -----------------------------------------------------------------------------
// Every colour comes from the palette
// -----------------------------------------------------------------------------

for (const variant of VARIANTS) {
  test(`every colour in the ${variant} theme is a value from that palette`, () => {
    // The claim the whole design rests on: "generated from a single extracted
    // palette.ts". A hand-picked hex in one of these files would be a colour
    // that does not move when the brand does -- and the drift gate above would
    // happily keep it there forever, because it only checks that the file
    // matches the generator, not that the generator used the palette.
    const palette = variant === "dark" ? DARK : LIGHT;
    const allowed = new Set(Object.values(palette));
    const theme = buildEditorTheme(variant);

    const strays: string[] = [];
    for (const [key, value] of Object.entries(theme.colors)) {
      if (!allowed.has(value)) strays.push(`colors["${key}"] = ${value}`);
    }
    for (const rule of theme.tokenColors) {
      const fg = rule.settings.foreground;
      if (fg !== undefined && !allowed.has(fg)) {
        strays.push(`tokenColors[${rule.scope.join(",")}].foreground = ${fg}`);
      }
    }
    assert.deepEqual(strays, [], "route every theme colour through palette.ts");
  });
}

test("both themes define exactly the same colour keys", () => {
  // A key present in one theme and absent from the other is a surface that
  // wears the brand in one and VS Code's default in the other -- which is the
  // "several products at once" complaint this epic exists to fix, reintroduced
  // one theme at a time.
  assert.deepEqual(
    Object.keys(buildEditorTheme("dark").colors).sort(),
    Object.keys(buildEditorTheme("light").colors).sort(),
  );
});

test("the themes cover the workbench surfaces the design names", () => {
  // D3 lists these by name. Asserting the PREFIXES rather than exact keys
  // leaves room to add `list.focusOutline` without editing a test, while still
  // failing if a whole surface -- the status bar, say -- is dropped.
  const colors = buildEditorTheme("dark").colors;
  const keys = Object.keys(colors);
  for (const surface of [
    "editor.",
    "sideBar.",
    "activityBar.",
    "statusBar.",
    "titleBar.",
    "tab.",
    "list.",
    "button.",
    "badge.",
    "input.",
    "panel.",
  ]) {
    assert.ok(
      keys.some((k) => k.startsWith(surface)),
      `no ${surface}* colours -- that surface keeps VS Code's default and reads as another product`,
    );
  }
  assert.ok(colors.focusBorder !== undefined, "focusBorder carries the accent");
});

test("terminal ANSI colours are left at VS Code's defaults", () => {
  // Deliberate, and stated in D3. The sixteen ANSI slots are a contract
  // between a user's shell prompt, their `ls` colours and every TUI they run;
  // a brand palette has no business overriding them, and a green that means
  // "MemQL" would be read as "this directory is executable".
  const ansi = Object.keys(buildEditorTheme("dark").colors).filter((k) =>
    k.startsWith("terminal.ansi"),
  );
  assert.deepEqual(ansi, []);
});

test("tokenColors is a minimal set, not a syntax theme", () => {
  // Also deliberate. D3 scopes this to "a branded workbench", and a
  // half-finished syntax theme is worse than none: it overrides some scopes
  // and leaves the rest to a theme that is no longer active, so the result is
  // neither the user's colours nor ours.
  const rules = buildEditorTheme("dark").tokenColors;
  assert.ok(rules.length > 0, "there is at least a comment/string/number/keyword split");
  assert.ok(rules.length <= 8, "this is a branded workbench, not a hand-tuned syntax theme");
  for (const rule of rules) {
    assert.ok(rule.scope.length > 0, "every rule names at least one scope");
  }
});

// -----------------------------------------------------------------------------
// Legibility, mechanically
// -----------------------------------------------------------------------------

/** WCAG 2.x relative luminance of a #rrggbb value. */
function luminance(hex: string): number {
  const channel = (pair: string): number => {
    const v = parseInt(pair, 16) / 255;
    return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  };
  const r = channel(hex.slice(1, 3));
  const g = channel(hex.slice(3, 5));
  const b = channel(hex.slice(5, 7));
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

/** WCAG contrast ratio between two #rrggbb values, 1:1 to 21:1. */
function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

// Foreground/background pairs that carry TEXT a user has to read. Decorations
// (whitespace marks, indent guides) are deliberately absent: they are not text
// and holding them to a text ratio would force them to stop being subtle.
const TEXT_PAIRS: [string, string][] = [
  ["editor.foreground", "editor.background"],
  ["sideBar.foreground", "sideBar.background"],
  ["sideBarSectionHeader.foreground", "sideBarSectionHeader.background"],
  ["activityBar.foreground", "activityBar.background"],
  ["activityBar.inactiveForeground", "activityBar.background"],
  ["activityBarBadge.foreground", "activityBarBadge.background"],
  ["list.activeSelectionForeground", "list.activeSelectionBackground"],
  ["list.inactiveSelectionForeground", "list.inactiveSelectionBackground"],
  ["list.hoverForeground", "list.hoverBackground"],
  ["statusBar.foreground", "statusBar.background"],
  ["statusBar.debuggingForeground", "statusBar.debuggingBackground"],
  ["breadcrumb.foreground", "breadcrumb.background"],
  ["titleBar.activeForeground", "titleBar.activeBackground"],
  ["titleBar.inactiveForeground", "titleBar.inactiveBackground"],
  ["tab.activeForeground", "tab.activeBackground"],
  ["tab.inactiveForeground", "tab.inactiveBackground"],
  ["button.foreground", "button.background"],
  ["button.secondaryForeground", "button.secondaryBackground"],
  ["badge.foreground", "badge.background"],
  ["input.foreground", "input.background"],
  ["input.placeholderForeground", "input.background"],
  ["dropdown.foreground", "dropdown.background"],
  ["editorLineNumber.foreground", "editor.background"],
  ["panelTitle.activeForeground", "panel.background"],
  ["panelTitle.inactiveForeground", "panel.background"],
  ["quickInput.foreground", "quickInput.background"],
];

for (const variant of VARIANTS) {
  test(`${variant}: every text pair clears WCAG AA (4.5:1)`, () => {
    // This is the AC's "yields legible trees, lists, buttons and status bar"
    // turned into an assertion. Eyeballing a theme is how a 3.9:1 status bar
    // ships: it looks fine to whoever picked it, on their monitor, that day.
    const colors = buildEditorTheme(variant).colors;
    const failures: string[] = [];
    for (const [fgKey, bgKey] of TEXT_PAIRS) {
      const fg = colors[fgKey];
      const bg = colors[bgKey];
      assert.ok(fg !== undefined, `${variant}: ${fgKey} is defined`);
      assert.ok(bg !== undefined, `${variant}: ${bgKey} is defined`);
      const ratio = contrast(fg, bg);
      if (ratio < 4.5) {
        failures.push(`${fgKey} on ${bgKey}: ${ratio.toFixed(2)}:1 (${fg} on ${bg})`);
      }
    }
    assert.deepEqual(failures, [], `${variant} theme has unreadable text pairs`);
  });

  test(`${variant}: syntax tints clear WCAG AA against the editor background`, () => {
    const theme = buildEditorTheme(variant);
    const bg = theme.colors["editor.background"];
    const failures: string[] = [];
    for (const rule of theme.tokenColors) {
      const fg = rule.settings.foreground;
      if (fg === undefined) continue;
      const ratio = contrast(fg, bg);
      if (ratio < 4.5) failures.push(`${rule.scope.join(",")}: ${ratio.toFixed(2)}:1 (${fg})`);
    }
    assert.deepEqual(failures, [], `${variant} syntax tints are unreadable on the editor background`);
  });
}

test("the contrast helper reports the ratios it should", () => {
  // The reachable positive for the two tests above. A luminance function with
  // a transposed coefficient still returns plausible numbers, and every pair
  // would keep passing -- so the helper is pinned against the two ratios
  // WCAG's own worked examples give.
  assert.equal(Math.round(contrast("#ffffff", "#000000")), 21);
  assert.equal(Math.round(contrast("#ffffff", "#ffffff")), 1);
  assert.ok(contrast("#767676", "#ffffff") >= 4.5, "the canonical AA-passing grey passes");
  assert.ok(contrast("#808080", "#ffffff") < 4.5, "a slightly lighter grey does not");
});

// -----------------------------------------------------------------------------
// The manifest contribution
// -----------------------------------------------------------------------------

test("package.json contributes both themes, pointing at the committed files", () => {
  const themes = manifest.contributes.themes ?? [];
  assert.equal(themes.length, 2, "MemQL Dark and MemQL Light");

  const byLabel = new Map(themes.map((t) => [t.label, t]));
  const dark = byLabel.get(THEME_NAMES.dark);
  const light = byLabel.get(THEME_NAMES.light);
  assert.ok(dark !== undefined, `"${THEME_NAMES.dark}" is contributed`);
  assert.ok(light !== undefined, `"${THEME_NAMES.light}" is contributed`);

  // uiTheme is what VS Code uses to pick its OWN defaults for everything the
  // file does not name, and to decide which group the picker lists it under.
  // Getting it backwards gives a dark theme with light scrollbars and puts it
  // under "Light Themes".
  assert.equal(dark.uiTheme, "vs-dark");
  assert.equal(light.uiTheme, "vs");

  for (const [variant, entry] of [["dark", dark], ["light", light]] as const) {
    assert.equal(entry.path, `./${THEME_FILES[variant]}`);
    assert.ok(
      fs.existsSync(path.join(ROOT, THEME_FILES[variant])),
      `${entry.path} exists on disk -- a VSIX packs files, and a missing one is a theme that fails to load`,
    );
  }
});

test("the theme JSON's own name matches the label the manifest advertises", () => {
  // VS Code shows the manifest's `label` in the picker and the file's `name`
  // in some settings surfaces. Two spellings of one theme is a support
  // question nobody can answer.
  for (const variant of VARIANTS) {
    const onDisk = JSON.parse(
      fs.readFileSync(path.join(ROOT, THEME_FILES[variant]), "utf8"),
    ) as { name: string };
    assert.equal(onDisk.name, THEME_NAMES[variant]);
  }
});

test(".vscodeignore does not exclude the themes directory", () => {
  // The failure this prevents is invisible until someone installs the VSIX:
  // the manifest advertises two themes, the picker lists them, and choosing
  // one does nothing because the file it points at was never packed.
  const ignore = fs.readFileSync(path.join(ROOT, ".vscodeignore"), "utf8");
  const offenders = ignore
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "" && !line.startsWith("#"))
    .filter((line) => line === "themes" || line.startsWith("themes/"));
  assert.deepEqual(offenders, [], "the themes must ship inside the VSIX");
});
