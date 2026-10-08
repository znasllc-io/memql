import type { EditorAppearance } from "../../../brand/editorAppearance.js";
import { buildEditorTheme, THEME_NAMES } from "../../vscode/src/theme/editorThemes.js";
import { DARK, LIGHT } from "../../vscode/src/webview/palette.js";

export { readEditorAppearance } from "../../../brand/editorAppearance.js";

export function appearanceTheme(appearance: EditorAppearance) {
  const c = appearance.colors;
  const theme = buildEditorTheme(appearance.mode, {
    ...(appearance.mode === "dark" ? DARK : LIGHT),
    bg: c.ground, surface: c.plate, raised: c.raised, fg: c.ink, muted: c.muted,
    border: c.line, "border-strong": c.raised, subtle: c.muted,
    accent: c.accent, "accent-deep": c.plate, "accent-subtle": c["accent-soft"],
    "on-accent": c["accent-fg"], "on-accent-hover": c.ink, focus: c.accent,
    ok: c.accent, warn: c.warn, danger: c.error,
  });
  // Quiet chrome follows the OS surface; primary controls keep its accent.
  theme.colors["statusBar.foreground"] = c.muted;
  theme.colors["statusBar.noFolderForeground"] = c.muted;
  Object.assign(theme.colors, {
    "textLink.foreground": c.accent, "textLink.activeForeground": c.accent,
    "textCodeBlock.background": c.raised, "textBlockQuote.border": c.accent,
    "textBlockQuote.background": c.plate, "textPreformat.foreground": c.ink,
    "button.hoverBackground": c.accent, "toolbar.hoverBackground": c["accent-soft"],
    "toolbar.activeBackground": c["accent-soft"],
  });
  return theme;
}

/** Theme-scoped overrides leave other themes and unrelated customizations intact. */
export function appearanceSettings(appearance: EditorAppearance, existing: Record<string, unknown> = {}) {
  const theme = appearanceTheme(appearance);
  const name = THEME_NAMES[appearance.mode];
  const scope = `[${name}]`;
  return { name, colors: { ...existing, [scope]: {
    ...(existing[scope] && typeof existing[scope] === "object" ? existing[scope] as object : {}), ...theme.colors,
  } } };
}
