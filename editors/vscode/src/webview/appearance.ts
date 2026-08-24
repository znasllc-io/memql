// The appearance resolver: the whole of decision D1, as one pure function
// (memql#4419; design record docs/superpowers/specs/2026-08-23-vscode-theme-parity-design.md).
//
// THE PROBLEM IT SOLVES. brandTokens.ts used to select its dark palette on
// `body.vscode-dark` -- the class VS Code itself stamps -- which meant the
// MemQL panels could only ever wear the theme the EDITOR wore. An operator
// running a light editor could not have dark MemQL panels, and the portal
// offers exactly that choice. `memql.appearance` is the choice; this function
// is the one place it is turned into a palette, so no panel has to reason
// about it and no two panels can disagree.
//
// ONE RESOLVER, DELIBERATELY PURE. It takes the setting and the editor's kind
// and returns a theme; it reads no configuration and knows no VS Code API, so
// the twelve-row truth table in test/appearance.test.ts is the complete
// specification and runs under bare `node --test`. The adapter that reads the
// live values is src/webview/theme.ts, and it is three lines long precisely
// because everything decidable lives here.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

/**
 * The `memql.appearance` setting, as contributed in package.json.
 *
 * `system` is the default and means FOLLOW THE EDITOR. Inside VS Code the
 * editor is the ambient theme -- it is already what tracks the OS when the
 * user asks it to -- so "system" resolved any other way would put the panels
 * out of step with the window around them.
 */
export type AppearanceSetting = "system" | "light" | "dark";

/**
 * What a panel actually renders as.
 *
 * `hc` is not a palette: it means "defer wholesale to VS Code's own
 * variables", which is what brandTokens.ts's `vscode-high-contrast` rules
 * already do. It is a third value rather than a boolean beside the palette so
 * that "which palette" and "no palette at all" cannot both be true at once.
 */
export type EffectiveTheme = "light" | "dark" | "hc";

/**
 * VS Code's `ColorThemeKind` values, copied rather than imported.
 *
 * This module may not import `vscode` (see the header), so the four numbers
 * live here. They are this module's OWN vocabulary rather than a copy that has
 * to stay in step: src/webview/theme.ts maps the editor's real enum onto them
 * with an explicit switch, so a renumbering upstream is absorbed, a member
 * removed from the enum stops type-checking, and a member added to it falls
 * through to the unknown-kind rule below. Matching VS Code's actual numbering
 * is therefore a readability choice -- a reader debugging with raw values from
 * the editor sees the same ones -- and test/appearance.test.ts pins it so it
 * stays a true statement.
 */
export const COLOR_THEME_KIND = {
  light: 1,
  dark: 2,
  highContrast: 3,
  highContrastLight: 4,
} as const;

/**
 * Resolve `memql.appearance` plus the editor's current kind into the palette a
 * panel should render with.
 *
 * Three rules, in the order they are applied, because the order IS the design:
 *
 *  1. **High contrast wins.** In either high-contrast kind the answer is `hc`
 *     whatever the setting says. A user who forced `dark` and then switched
 *     the editor into high contrast has told us two things and the
 *     accessibility one outranks the brand one -- the same stance
 *     brandTokens.ts has taken since memql#4196. The setting does not get to
 *     undo it.
 *  2. **An explicit setting wins over the editor.** That is the entire point
 *     of the setting existing.
 *  3. **Otherwise follow the editor**, and treat a kind this build does not
 *     recognise as dark. VS Code can add kinds, and the wrong guess is not
 *     symmetric: dark tokens on a light surface are unreadable, light tokens
 *     on a dark surface are merely dim.
 *
 * `setting` is typed as a plain string rather than `AppearanceSetting` on
 * purpose: it arrives from `getConfiguration().get()`, which answers whatever
 * is in a hand-edited settings.json, and `undefined` when the key is unset.
 * Both land on the documented default instead of on a fourth behaviour.
 */
export function effectiveTheme(setting: string | undefined, editorKind: number): EffectiveTheme {
  if (
    editorKind === COLOR_THEME_KIND.highContrast ||
    editorKind === COLOR_THEME_KIND.highContrastLight
  ) {
    return "hc";
  }
  if (setting === "light") return "light";
  if (setting === "dark") return "dark";
  return editorKind === COLOR_THEME_KIND.light ? "light" : "dark";
}

/**
 * The body attribute a panel stamps for a resolved theme, ready to interpolate
 * as `` `<body${bodyThemeAttr(theme)}>` ``.
 *
 * The LEADING SPACE is part of the value, so callers never have to remember
 * one: `<body` + `data-memql-theme="dark"` with no space is a tag named
 * `bodydata-memql-theme`, which is valid HTML that renders an empty page and
 * reports nothing anywhere.
 *
 * `hc` stamps NOTHING, and that is load-bearing. brandTokens.ts's
 * high-contrast rules key on the `vscode-high-contrast` / `-light` classes VS
 * Code puts on the body itself; adding a `data-memql-theme` beside them would
 * give the cascade a second opinion about a case that is already settled.
 */
export function bodyThemeAttr(theme: EffectiveTheme): string {
  return theme === "hc" ? "" : ` data-memql-theme="${theme}"`;
}
