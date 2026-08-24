// The one place the live appearance is read (memql#4419, D2).
//
// WHAT THIS IS FOR. appearance.ts decides; this file is the three-line adapter
// that fetches the two inputs that decision needs -- `memql.appearance` and
// the editor's current theme kind -- and the subscription that tells a panel
// when either has moved. It is an ADAPTER in the sense
// cmd/memql-lsp/vscodeimportrule_test.go means: it wires VS Code's API to
// logic that lives elsewhere and carries none of its own.
//
// WHY EVERY PANEL GOES THROUGH IT. Nine panel classes render nine documents.
// If each read the setting for itself, "high contrast wins" would be nine
// separate implementations of one rule, and the panel that forgot it would be
// the one an operator using high contrast opened. One resolver, one reading of
// it, stamped once per render.
//
// THE EDITOR'S ENUM IS MAPPED EXPLICITLY, not cast. `effectiveTheme` takes a
// number so that it can stay free of `vscode`, and the temptation is to hand
// it `vscode.window.activeColorTheme.kind` directly -- which works only for as
// long as the enum's numbering never changes. The switch below removes that
// coupling entirely: it maps each REAL enum member onto appearance.ts's own
// vocabulary, so a renumbering in the editor is absorbed, a member REMOVED
// from the enum stops type-checking, and a member ADDED to it falls to
// `default`, where appearance.ts's documented unknown-kind rule takes over.

import * as vscode from "vscode";

import {
  COLOR_THEME_KIND,
  bodyThemeAttr,
  effectiveTheme,
  type EffectiveTheme,
} from "./appearance.js";

/** The configuration section and key, split as `getConfiguration` wants them. */
const SECTION = "memql";
const KEY = "appearance";

/** The dotted name, for `affectsConfiguration` and for anything that reports it. */
export const APPEARANCE_SETTING = `${SECTION}.${KEY}`;

/**
 * A kind number appearance.ts understands, from the editor's own enum.
 *
 * `default` returns a value in no palette's range on purpose rather than
 * guessing: appearance.ts treats an unrecognised kind as dark under `system`
 * and lets an explicit setting win, which is the behaviour a kind we have
 * never seen should get.
 */
function kindNumber(kind: vscode.ColorThemeKind): number {
  switch (kind) {
    case vscode.ColorThemeKind.Light:
      return COLOR_THEME_KIND.light;
    case vscode.ColorThemeKind.Dark:
      return COLOR_THEME_KIND.dark;
    case vscode.ColorThemeKind.HighContrast:
      return COLOR_THEME_KIND.highContrast;
    case vscode.ColorThemeKind.HighContrastLight:
      return COLOR_THEME_KIND.highContrastLight;
    default:
      return -1;
  }
}

/**
 * What the MemQL panels should render as right now.
 *
 * Read fresh on every call, never cached: `activeColorTheme` is replaced
 * wholesale by the editor on every theme change, and a cached kind would leave
 * a repainted panel wearing the theme it had when it opened.
 */
export function currentTheme(): EffectiveTheme {
  const setting = vscode.workspace.getConfiguration(SECTION).get<string>(KEY);
  return effectiveTheme(setting, kindNumber(vscode.window.activeColorTheme.kind));
}

/**
 * The body attribute for the current appearance, ready to interpolate as
 * `` `<body${currentBodyThemeAttr()}>` ``.
 *
 * The leading space belongs to the value; see bodyThemeAttr's doc comment for
 * what happens without it.
 */
export function currentBodyThemeAttr(): string {
  return bodyThemeAttr(currentTheme());
}

/**
 * Subscribe a panel's repaint to both inputs of the appearance decision.
 *
 * Returns the disposables rather than registering them anywhere, so they join
 * the PANEL'S own list and die with the tab. Registering them on
 * `context.subscriptions` instead would leak exactly the way conceptPanel.ts's
 * header records: that array is disposed once, at deactivation, so every
 * listener from every panel an operator ever opened would accumulate for the
 * life of the window, each holding a long-disposed panel.
 *
 * The configuration listener is GUARDED on our own setting. Without the guard
 * every panel repaints on every keystroke in settings.json -- and a repaint
 * assigns `webview.html`, which replaces the whole document and takes the
 * focused element, the caret and any selection with it.
 */
export function onAppearanceChange(repaint: () => void): vscode.Disposable[] {
  return [
    vscode.window.onDidChangeActiveColorTheme(() => repaint()),
    vscode.workspace.onDidChangeConfiguration((event) => {
      if (event.affectsConfiguration(APPEARANCE_SETTING)) repaint();
    }),
  ];
}
