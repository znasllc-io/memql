// The one-time offer to switch editor themes (memql#4421, D4).
//
// The decision, tested away from `vscode` -- the same split
// src/auth/passkeyOffer.ts uses, and for the same reason: what is worth
// pinning is WHEN this fires and when it must not, and none of that needs an
// editor. src/extension.ts supplies the notification and the globalState.
//
// D4's constraint is "one-time gentle offer, never a takeover", so most of
// this file is about the cases that must NOT produce an offer. Changing a
// user's editor theme unasked is hostile; asking twice is how a good prompt
// becomes one people learn to dismiss without reading.

import test from "node:test";
import assert from "node:assert/strict";

import { COLOR_THEME_KIND } from "../src/webview/appearance.js";
import { THEME_NAMES } from "../src/theme/editorThemes.js";
import {
  OFFER_DISMISS,
  OFFER_MESSAGE,
  OFFER_SWITCH,
  isMemqlTheme,
  memqlThemeFor,
  shouldOfferMemqlTheme,
} from "../src/theme/themeOffer.js";

const { light, dark, highContrast, highContrastLight } = COLOR_THEME_KIND;

/** The ordinary case: a fresh install on somebody else's theme. */
const UNANSWERED = { answered: false, activeThemeLabel: "Default Dark Modern", editorKind: dark };

test("offers once to an operator on a non-MemQL theme", () => {
  assert.equal(shouldOfferMemqlTheme(UNANSWERED), true);
  assert.equal(shouldOfferMemqlTheme({ ...UNANSWERED, editorKind: light }), true);
});

test("never offers again once the operator has answered", () => {
  // EITHER answer counts. "Not now" that comes back tomorrow is not "not now",
  // it is "every day", and D4's whole claim is that this fires at most once.
  assert.equal(shouldOfferMemqlTheme({ ...UNANSWERED, answered: true }), false);
  assert.equal(
    shouldOfferMemqlTheme({ ...UNANSWERED, answered: true, editorKind: light }),
    false,
  );
});

test("does not offer to an operator already on a MemQL theme", () => {
  for (const label of [THEME_NAMES.dark, THEME_NAMES.light]) {
    assert.equal(shouldOfferMemqlTheme({ ...UNANSWERED, activeThemeLabel: label }), false);
  }
});

test("does not offer under either high-contrast theme", () => {
  // A high-contrast theme is an ACCESSIBILITY choice, and neither MemQL theme
  // is a high-contrast theme. Inviting someone out of high contrast and into a
  // brand palette is the one version of this prompt that could do real harm,
  // and it is the same stance D1 takes for the panels: high contrast wins.
  for (const kind of [highContrast, highContrastLight]) {
    assert.equal(shouldOfferMemqlTheme({ ...UNANSWERED, editorKind: kind }), false);
  }
});

test("matches the offered theme to the editor's current kind", () => {
  // Someone on a light editor who accepts must land on MemQL Light. Offering
  // the dark one and switching them to it is a takeover wearing a button.
  assert.equal(memqlThemeFor(light), THEME_NAMES.light);
  assert.equal(memqlThemeFor(dark), THEME_NAMES.dark);
});

test("an unknown editor kind resolves to the dark theme", () => {
  // Same direction appearance.ts takes for an unrecognised kind, and for the
  // same reason: it is the safer of the two to be wrong about.
  assert.equal(memqlThemeFor(99), THEME_NAMES.dark);
});

test("isMemqlTheme matches the two labels exactly and nothing else", () => {
  // EXACT, not fuzzy. `workbench.colorTheme` holds a theme's label verbatim,
  // and a substring match would treat a third-party "MemQL Dark Pro" as ours
  // -- suppressing the offer for a theme we did not ship.
  assert.equal(isMemqlTheme(THEME_NAMES.dark), true);
  assert.equal(isMemqlTheme(THEME_NAMES.light), true);
  assert.equal(isMemqlTheme("MemQL Dark Pro"), false);
  assert.equal(isMemqlTheme("memql dark"), false);
  assert.equal(isMemqlTheme("Default Dark Modern"), false);
  assert.equal(isMemqlTheme(""), false);
});

test("the offer names both themes and both actions are distinct", () => {
  assert.match(OFFER_MESSAGE, /MemQL Dark/);
  assert.match(OFFER_MESSAGE, /MemQL Light/);
  assert.notEqual(OFFER_SWITCH, OFFER_DISMISS);
  assert.ok(OFFER_SWITCH.length > 0 && OFFER_DISMISS.length > 0);
  // The message must say what the operator GETS, because the panels are
  // already branded -- without this the prompt reads as "your theme is wrong".
  assert.match(OFFER_MESSAGE, /sidebar|side bar|tree|chrome/i);
});
