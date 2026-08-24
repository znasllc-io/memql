// The appearance resolver: one function, one truth table (memql#4419, D1/D2).
//
// WHY A TRUTH TABLE AND NOT A HANDFUL OF CASES. `effectiveTheme` is the whole
// of decision D1, and its inputs are two small closed sets -- three settings
// and four editor kinds. Twelve rows is the ENTIRE behaviour, so enumerating
// them costs less than choosing which subset to trust, and every future edit
// has to face all twelve rather than the three someone happened to write.
//
// The rows that carry the design, stated so a later reader does not "simplify"
// them away:
//
//   * High contrast wins over the setting, in all three settings. A user who
//     forced `dark` and then switched the editor into high contrast is telling
//     us two things, and the accessibility one outranks the brand one. This is
//     the same stance brandTokens.ts has always taken for its `vscode-high-
//     contrast` classes; the setting does not get to undo it.
//   * `system` means FOLLOW THE EDITOR, not follow the OS. Inside VS Code the
//     editor IS the ambient theme -- it is the thing that already tracks the
//     OS when the user asks it to -- so resolving `system` any other way would
//     put the panels out of step with the window around them.
//   * An UNKNOWN editor kind resolves to `dark` under `system`, because VS Code
//     can add kinds and a wrong guess is not symmetric: dark tokens on a light
//     surface is unreadable, light tokens on a dark surface is merely dim.
//     Under a FORCED setting the setting still wins -- an unknown kind is not
//     known to be high contrast, and treating it as if it were would silently
//     drop a preference the user set explicitly.
//
// Deliberately free of `vscode` imports, like the module it tests
// (cmd/memql-lsp/vscodeimportrule_test.go).

import test from "node:test";
import assert from "node:assert/strict";

import {
  COLOR_THEME_KIND,
  bodyThemeAttr,
  effectiveTheme,
  type AppearanceSetting,
  type EffectiveTheme,
} from "../src/webview/appearance.js";

const { light, dark, highContrast, highContrastLight } = COLOR_THEME_KIND;

// setting x editor kind -> effective theme. All twelve rows, in setting order.
const TRUTH_TABLE: [AppearanceSetting, number, EffectiveTheme][] = [
  ["system", light, "light"],
  ["system", dark, "dark"],
  ["system", highContrast, "hc"],
  ["system", highContrastLight, "hc"],

  ["light", light, "light"],
  ["light", dark, "light"],
  ["light", highContrast, "hc"],
  ["light", highContrastLight, "hc"],

  ["dark", light, "dark"],
  ["dark", dark, "dark"],
  ["dark", highContrast, "hc"],
  ["dark", highContrastLight, "hc"],
];

test("effectiveTheme resolves all twelve setting x editor-kind combinations", () => {
  for (const [setting, kind, expected] of TRUTH_TABLE) {
    assert.equal(
      effectiveTheme(setting, kind),
      expected,
      `memql.appearance=${setting} with editor kind ${kind}`,
    );
  }
});

test("high contrast wins over a forced setting, in both high-contrast kinds", () => {
  // Called out separately from the table because it is the ONE row a reader is
  // most likely to think is a bug: the user asked for dark and did not get it.
  for (const kind of [highContrast, highContrastLight]) {
    assert.equal(effectiveTheme("dark", kind), "hc");
    assert.equal(effectiveTheme("light", kind), "hc");
  }
});

test("an unknown editor kind resolves to dark under system", () => {
  // VS Code may add kinds. 99 stands for "a kind this build has never heard
  // of"; dark is the safer contrast direction, and pinning it here is what
  // stops a future kind from resolving to whatever `undefined` happens to
  // compare as.
  assert.equal(effectiveTheme("system", 99), "dark");
});

test("a forced setting still wins over an unknown editor kind", () => {
  // An unknown kind is not KNOWN to be high contrast, so it must not be
  // treated as one -- that would discard a preference the user set by hand.
  assert.equal(effectiveTheme("light", 99), "light");
  assert.equal(effectiveTheme("dark", 99), "dark");
});

test("an absent or unrecognised setting is treated as system", () => {
  // `getConfiguration().get()` answers undefined when a key has never been
  // written and the manifest default has not been consulted, and settings.json
  // is hand-editable, so a typo reaches this function as a plain string. Both
  // must land on the documented default rather than on a fourth behaviour.
  assert.equal(effectiveTheme(undefined, dark), "dark");
  assert.equal(effectiveTheme(undefined, light), "light");
  assert.equal(effectiveTheme("", light), "light");
  assert.equal(effectiveTheme("Dark", light), "light", "the enum is lower-case; 'Dark' is not it");
  assert.equal(effectiveTheme("solarized", dark), "dark");
});

test("bodyThemeAttr stamps light and dark, and stamps nothing for high contrast", () => {
  // The `hc` case stamping NOTHING is load-bearing: brandTokens.ts's
  // high-contrast rules key on the `vscode-high-contrast*` classes VS Code
  // itself puts on the body, and a `data-memql-theme` alongside them would
  // give the cascade a second opinion about a case that is settled.
  assert.equal(bodyThemeAttr("light"), ' data-memql-theme="light"');
  assert.equal(bodyThemeAttr("dark"), ' data-memql-theme="dark"');
  assert.equal(bodyThemeAttr("hc"), "");
});

test("bodyThemeAttr's output is a complete attribute, leading space included", () => {
  // Panels interpolate it as `<body${bodyThemeAttr(theme)}>`. Returning
  // `data-memql-theme="dark"` without the leading space would emit
  // `<bodydata-memql-theme="dark">`, which is a tag named `bodydata-memql-theme`
  // -- valid HTML that renders an empty page, with no error anywhere.
  for (const theme of ["light", "dark", "hc"] as const) {
    const tag = `<body${bodyThemeAttr(theme)}>`;
    assert.match(tag, /^<body[ >]/, `"${theme}" must not fuse onto the tag name`);
  }
});

test("COLOR_THEME_KIND carries VS Code's own numbering", () => {
  // These four numbers match vscode.ColorThemeKind. Correctness does not
  // DEPEND on that -- src/webview/theme.ts maps the editor's real enum onto
  // this vocabulary with an explicit switch -- but the match is what lets a
  // reader compare a raw kind seen in the debugger against this table, so it
  // is worth keeping true rather than letting it quietly rot.
  assert.deepEqual(COLOR_THEME_KIND, {
    light: 1,
    dark: 2,
    highContrast: 3,
    highContrastLight: 4,
  });
});
