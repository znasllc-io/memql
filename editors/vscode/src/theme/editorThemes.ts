// The two MemQL editor colour themes, derived from the palette (memql#4420, D3).
//
// WHY AN EXTENSION SHIPS COLOUR THEMES AT ALL. Tree rows, the activity bar,
// tabs and the status bar are drawn by the WORKBENCH, and an extension gets
// ThemeIcon and ThemeColor and nothing else -- brandTokens.ts has recorded
// that platform constraint since memql#4196. So the panels could be perfectly
// on-brand and the chrome around them would still be whatever theme the user
// picked, which is the "several products at once" complaint that opened this
// epic. The only honest fix is for VS Code's OWN theme to be a MemQL theme.
// These are opt-in: the user picks one from the theme picker, and the
// extension never switches it for them (memql#4421 offers, once).
//
// WHY THE MAPPING IS A TABLE. `COLORS` maps a VS Code colour id to a PALETTE
// KEY, never to a hex. Two properties then hold by construction rather than by
// review: every colour in either theme is a value from that theme's palette,
// and both themes define exactly the same keys -- so no surface can wear the
// brand in one theme and VS Code's default in the other. test/themes.test.ts
// asserts both anyway, because they are claims about the OUTPUT and this
// structure is free to change.
//
// WHAT IS DELIBERATELY ABSENT:
//   * The sixteen `terminal.ansi*` slots. They are a contract between a user's
//     shell prompt, their `ls` colours and every TUI they run. A brand green
//     in that palette would be read as "this file is executable".
//   * A full syntax theme. `TOKENS` is a four-rule split -- comments, strings,
//     numbers, keywords -- because a half-finished syntax theme is worse than
//     none: it overrides some scopes and leaves the rest to a theme that is no
//     longer active, so the result is neither the user's colours nor ours.
//   * Anything needing transparency (scrollbar shadows, hover overlays). The
//     palette is opaque six-digit hexes; VS Code's own defaults for the
//     declared `uiTheme` are better than an invented alpha.
//
// LEGIBILITY IS GATED, NOT EYEBALLED. `muted` carries every piece of
// de-emphasised TEXT and `subtle` is reserved for non-text decoration
// (whitespace marks, indent guides), because test/themes.test.ts holds every
// declared text pair to WCAG AA in both themes and `subtle` does not clear it
// against these backgrounds. That is the constraint to respect when adding a
// colour here.
//
// WHY IT IS NOT UNDER src/webview/. It reads the same palette the panels do,
// so that is where it first went -- and test/surfaceGuards.test.ts rejected
// it, correctly. That gate forbids `JSON.stringify` in the webview layer
// because `<pre>${JSON.stringify(value)}</pre>` is the unreadable value
// surface memql#3751 removed, and its own comment draws the line this module
// falls on: stringifying for the WIRE OR FOR A FILE is a different thing
// entirely. Nothing here renders; it serialises two files. So it lives beside
// the other non-rendering logic instead.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go):
// this module is data, shared by the drift test and by
// scripts/generate-themes.mjs, and neither runs inside an editor.

import { DARK, LIGHT, type PaletteKey } from "../webview/palette.js";

/** Which of the two themes is being built. */
export type ThemeVariant = "dark" | "light";

/** One `tokenColors` entry, in VS Code's TextMate-rule shape. */
export interface TokenRule {
  name: string;
  scope: string[];
  settings: { foreground?: string; fontStyle?: string };
}

/** A VS Code colour theme file, as VS Code reads it. */
export interface EditorTheme {
  $schema: string;
  name: string;
  type: ThemeVariant;
  colors: Record<string, string>;
  tokenColors: TokenRule[];
}

/** The labels the theme picker shows, and the manifest advertises. */
export const THEME_NAMES: Readonly<Record<ThemeVariant, string>> = {
  dark: "MemQL Dark",
  light: "MemQL Light",
};

/** Where the generated files live, relative to the extension root. */
export const THEME_FILES: Readonly<Record<ThemeVariant, string>> = {
  dark: "themes/memql-dark-color-theme.json",
  light: "themes/memql-light-color-theme.json",
};

/**
 * Every workbench colour this theme sets, as a VS Code colour id -> palette key.
 *
 * Grouped by surface, in the order D3 lists them. A colour id NOT here keeps
 * VS Code's own default for the declared `uiTheme`, which is the right answer
 * for anything needing transparency.
 */
const COLORS: Readonly<Record<string, PaletteKey>> = {
  // Base
  focusBorder: "accent",
  foreground: "fg",
  descriptionForeground: "muted",
  errorForeground: "danger",
  "widget.border": "border",
  "sash.hoverBorder": "accent",

  // Editor
  "editor.background": "bg",
  "editor.foreground": "fg",
  "editor.selectionBackground": "border-strong",
  "editor.selectionHighlightBackground": "border",
  "editor.lineHighlightBackground": "raised",
  "editor.findMatchBackground": "border-strong",
  "editor.findMatchHighlightBackground": "border",
  "editorCursor.foreground": "accent",
  "editorLineNumber.foreground": "muted",
  "editorLineNumber.activeForeground": "accent",
  "editorWhitespace.foreground": "subtle",
  "editorIndentGuide.background1": "border",
  "editorIndentGuide.activeBackground1": "border-strong",
  "editorWidget.background": "surface",
  "editorWidget.border": "border",
  "editorHoverWidget.background": "surface",
  "editorHoverWidget.border": "border",
  "editorSuggestWidget.background": "surface",
  "editorSuggestWidget.border": "border",
  "editorSuggestWidget.selectedBackground": "border-strong",
  "editorSuggestWidget.highlightForeground": "accent",
  "editorGroup.border": "border",
  "editorGroupHeader.tabsBackground": "surface",
  "editorError.foreground": "danger",
  "editorWarning.foreground": "data-string",
  "editorInfo.foreground": "accent",

  // Side bar
  "sideBar.background": "surface",
  "sideBar.foreground": "fg",
  "sideBar.border": "border",
  "sideBarTitle.foreground": "muted",
  "sideBarSectionHeader.background": "raised",
  "sideBarSectionHeader.foreground": "fg",
  "sideBarSectionHeader.border": "border",

  // Activity bar
  "activityBar.background": "surface",
  "activityBar.foreground": "accent",
  "activityBar.inactiveForeground": "muted",
  "activityBar.border": "border",
  "activityBar.activeBorder": "accent",
  "activityBarBadge.background": "accent",
  "activityBarBadge.foreground": "on-accent",

  // Lists and trees -- the surface this whole theme exists for
  "list.activeSelectionBackground": "border-strong",
  "list.activeSelectionForeground": "fg",
  "list.inactiveSelectionBackground": "border",
  "list.inactiveSelectionForeground": "fg",
  "list.hoverBackground": "raised",
  "list.hoverForeground": "fg",
  "list.focusBackground": "border-strong",
  "list.focusForeground": "fg",
  "list.focusOutline": "accent",
  "list.highlightForeground": "accent",
  "list.errorForeground": "danger",
  "list.warningForeground": "data-string",
  "tree.indentGuidesStroke": "border",

  // Status bar. `accent-deep` is the SAME hex in both palettes, so the status
  // bar is a brand constant across the pair rather than two different greens.
  "statusBar.background": "accent-deep",
  "statusBar.foreground": "on-accent-hover",
  "statusBar.border": "accent-deep",
  "statusBar.noFolderBackground": "accent-deep",
  "statusBar.noFolderForeground": "on-accent-hover",
  "statusBar.debuggingBackground": "data-string",
  "statusBar.debuggingForeground": "bg",
  "statusBarItem.remoteBackground": "accent",
  "statusBarItem.remoteForeground": "on-accent",

  // Title bar
  "titleBar.activeBackground": "surface",
  "titleBar.activeForeground": "fg",
  "titleBar.inactiveBackground": "surface",
  "titleBar.inactiveForeground": "muted",
  "titleBar.border": "border",

  // Tabs
  "tab.activeBackground": "bg",
  "tab.activeForeground": "fg",
  "tab.inactiveBackground": "surface",
  "tab.inactiveForeground": "muted",
  "tab.border": "border",
  "tab.activeBorderTop": "accent",
  "tab.unfocusedActiveBorderTop": "border-strong",
  "tab.hoverBackground": "raised",

  // Buttons
  "button.background": "accent",
  "button.foreground": "on-accent",
  "button.hoverBackground": "accent-deep",
  "button.secondaryBackground": "raised",
  "button.secondaryForeground": "fg",
  "button.secondaryHoverBackground": "border",

  // Badges
  "badge.background": "accent",
  "badge.foreground": "on-accent",

  // Inputs
  "input.background": "surface",
  "input.foreground": "fg",
  "input.border": "border-strong",
  "input.placeholderForeground": "muted",
  "inputOption.activeBorder": "accent",
  "inputValidation.errorBackground": "surface",
  "inputValidation.errorForeground": "fg",
  "inputValidation.errorBorder": "danger",
  "dropdown.background": "surface",
  "dropdown.foreground": "fg",
  "dropdown.border": "border-strong",

  // Panel and terminal chrome (ANSI slots deliberately untouched)
  "panel.background": "bg",
  "panel.border": "border",
  "panelTitle.activeForeground": "fg",
  "panelTitle.inactiveForeground": "muted",
  "panelTitle.activeBorder": "accent",
  "terminal.background": "bg",
  "terminal.foreground": "fg",

  // Quick input, menus, notifications
  "quickInput.background": "surface",
  "quickInput.foreground": "fg",
  "quickInputList.focusBackground": "border-strong",
  "quickInputList.focusForeground": "fg",
  "menu.background": "surface",
  "menu.foreground": "fg",
  "menu.border": "border",
  "menu.selectionBackground": "border-strong",
  "menu.selectionForeground": "fg",
  "notifications.background": "surface",
  "notifications.foreground": "fg",
  "notifications.border": "border",
  "notificationLink.foreground": "accent",

  // Breadcrumbs
  "breadcrumb.background": "bg",
  "breadcrumb.foreground": "muted",
  "breadcrumb.focusForeground": "fg",
};

/**
 * The minimal syntax split, from the data tints the site already uses.
 *
 * Four rules on purpose (see the header). Everything else inherits
 * `editor.foreground`, which is a deliberate answer rather than an omission:
 * an unstyled identifier in the editor's own foreground reads correctly, while
 * a half-guessed one does not.
 */
const TOKENS: readonly { name: string; scope: string[]; token: PaletteKey; fontStyle?: string }[] = [
  {
    name: "Comment",
    scope: ["comment", "punctuation.definition.comment"],
    token: "muted",
    fontStyle: "italic",
  },
  {
    name: "String",
    scope: ["string", "string.quoted", "constant.character"],
    token: "data-string",
  },
  {
    name: "Number and language constant",
    scope: ["constant.numeric", "constant.language", "constant.other"],
    token: "data-number",
  },
  {
    name: "Keyword, storage and annotation",
    scope: [
      "keyword",
      "keyword.control",
      "keyword.operator",
      "storage",
      "storage.modifier",
      "entity.name.tag",
    ],
    token: "accent",
  },
];

/** Build one theme from its palette. Pure: same variant in, same bytes out. */
export function buildEditorTheme(variant: ThemeVariant): EditorTheme {
  const palette = variant === "dark" ? DARK : LIGHT;

  const colors: Record<string, string> = {};
  for (const [id, token] of Object.entries(COLORS)) colors[id] = palette[token];

  const tokenColors: TokenRule[] = TOKENS.map((rule) => ({
    name: rule.name,
    scope: [...rule.scope],
    settings:
      rule.fontStyle === undefined
        ? { foreground: palette[rule.token] }
        : { foreground: palette[rule.token], fontStyle: rule.fontStyle },
  }));

  return {
    $schema: "vscode://schemas/color-theme",
    name: THEME_NAMES[variant],
    type: variant,
    colors,
    tokenColors,
  };
}

/**
 * The exact bytes `scripts/generate-themes.mjs` writes, and the exact bytes
 * test/themes.test.ts compares the committed file against.
 *
 * Shared rather than duplicated: if the generator and the gate serialised
 * separately, the gate would be checking its own formatting rather than the
 * generator's output -- and a formatting change in one of them would read as a
 * palette drift in the other.
 */
export function serializeTheme(theme: EditorTheme): string {
  return `${JSON.stringify(theme, null, 2)}\n`;
}
