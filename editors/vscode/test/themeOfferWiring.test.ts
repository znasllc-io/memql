// The theme offer, as activation actually fires it (memql#4421, D4).
//
// themeOffer.test.ts pins the DECISION away from `vscode`. This file pins the
// wiring, which is a separate claim and the one that has failed before in this
// package: a correct decision function nobody calls, or one called with the
// wrong inputs, passes every unit test and does nothing in the editor.
//
// ITS OWN FILE, because activation happens ONCE per process --
// registerRuntimeSurface guards on module state and refuses a second
// registration, which is why activation.test.ts and activationGates.test.ts
// are already split. node:test runs each file as its own process, so a case
// needing a different pre-activation world gets one by being a new file.
//
// THIS FILE'S WORLD: a fresh operator on VS Code's own dark theme who has
// never answered. That is the case the offer exists for, and the only one
// where an activation-driven test can observe the whole path -- prompt shown,
// answer recorded, theme written.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import type { ExtensionContext } from "vscode";

import { activate } from "../src/extension.js";
import { THEME_NAMES } from "../src/theme/editorThemes.js";
import {
  OFFER_DISMISS,
  OFFER_MESSAGE,
  OFFER_SWITCH,
  THEME_OFFER_ANSWERED_KEY,
} from "../src/theme/themeOffer.js";
import {
  ColorThemeKind,
  recorded,
  setActiveColorTheme,
  setNextInformationMessageChoice,
  settings,
  workspace,
  writtenSettings,
} from "./support/vscodeStub.js";

// The same isolation activation.test.ts takes, and for the same reasons: the
// runtime surface mkdirs ~/.memql, and an empty PATH is the only way to make
// resolveOnPath report "memql-lsp is not installed".
const home = fs.mkdtempSync(path.join(os.tmpdir(), "memql-themeoffer-"));
process.env.HOME = home;
process.env.PATH = "";

const globalState = {
  store: new Map<string, unknown>(),
  get<T>(key: string): T | undefined {
    return globalState.store.get(key) as T | undefined;
  },
  update(key: string, value: unknown): Promise<void> {
    if (value === undefined) globalState.store.delete(key);
    else globalState.store.set(key, value);
    return Promise.resolve();
  },
};

const context = {
  subscriptions: [] as { dispose(): unknown }[],
  asAbsolutePath: (relative: string) => path.join(home, "extension", relative),
  globalState,
} as unknown as ExtensionContext;

// The world, set BEFORE the single activate() below: a dark editor on VS
// Code's own theme, and no recorded answer.
settings.clear();
writtenSettings.clear();
workspace.isTrusted = true;
setActiveColorTheme(ColorThemeKind.Dark);
writtenSettings.set("workbench.colorTheme", "Default Dark Modern");
setNextInformationMessageChoice(OFFER_SWITCH);

activate(context);

test("activation offers the theme, with both actions", () => {
  // recorded.infos alone would be satisfied by a notification whose buttons
  // were dropped -- which looks identical in a test and is a dead end on
  // screen. The ACTIONS are the assertion.
  const at = recorded.infos.indexOf(OFFER_MESSAGE);
  assert.ok(at >= 0, `the offer was not shown; saw: ${JSON.stringify(recorded.infos)}`);
  assert.deepEqual(recorded.infoActions[at], [OFFER_SWITCH, OFFER_DISMISS]);
});

test("accepting writes the matching theme to the user's own settings", async () => {
  // The offer resolves on a promise, so give the continuation a turn. Awaiting
  // a resolved promise is enough -- there is no timer in the path, and a fixed
  // sleep here would pass for the wrong reason on a slow machine.
  await Promise.resolve();
  await Promise.resolve();

  assert.equal(
    writtenSettings.get("workbench.colorTheme"),
    THEME_NAMES.dark,
    "a dark editor must be offered and given MemQL Dark, not MemQL Light",
  );
  const write = writtenSettings
    .writes()
    .find((w) => w.sectionKey === "workbench.colorTheme");
  assert.ok(write !== undefined, "the theme was written, not just read back");
});

test("answering is recorded so the offer never fires again", () => {
  assert.equal(globalState.get(THEME_OFFER_ANSWERED_KEY), true);
});

test("the offer fires exactly once per activation", () => {
  // A second copy of the message would mean the offer is wired into two places
  // -- the shape that turns a one-time prompt into a pair of them.
  const count = recorded.infos.filter((m) => m === OFFER_MESSAGE).length;
  assert.equal(count, 1);
});
