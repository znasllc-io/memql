// The installed set, and how a pack's CSS reaches the document.
//
// The built-ins plus whatever this desktop has installed. `themePacks()` was
// a function on the foundation's registry rather than a constant precisely so
// this epic could replace it without touching its callers.

import { BUILT_IN_PACKS, BUILT_IN_THEME_ID, GRAPHITE } from "./builtins";
import { themePackCss, type OsThemePack } from "./pack";

export { BUILT_IN_PACKS, BUILT_IN_THEME_ID, GRAPHITE, isBuiltInId } from "./builtins";
export * from "./pack";

/** The style element every installed pack's CSS lives in. One, rewritten. */
export const THEME_STYLE_ID = "os-theme-packs";

export function themePacks(installed: readonly OsThemePack[] = []): readonly OsThemePack[] {
  return [...BUILT_IN_PACKS, ...installed];
}

export function themePackById(
  id: string,
  installed: readonly OsThemePack[] = [],
): OsThemePack | undefined {
  return themePacks(installed).find((p) => p.id === id);
}

/**
 * The pack to RENDER for a stored id.
 *
 * An id naming nothing installed falls back to the built-in rather than
 * leaving the shell with no theme: a stored pack outlives its installation
 * (uninstalled here, or installed on another machine and roamed to this one
 * before the desktop document caught up), and the wallpaper still has to know
 * what to paint.
 *
 * The ATTRIBUTE is not resolved this way -- see chrome/state.tsx. The stored
 * id is stamped verbatim, so a pack that arrives later takes effect without a
 * reload, and a roamed desktop naming a pack this bundle does not have keeps
 * naming it rather than silently rewriting the person's choice to graphite.
 */
export function resolveThemePack(
  id: string,
  installed: readonly OsThemePack[] = [],
): OsThemePack {
  return themePackById(id, installed) ?? GRAPHITE;
}

/**
 * Put every pack's CSS in the document, in one style element.
 *
 * GRAPHITE IS SKIPPED, and that is the same fact spec G recorded: it is the
 * unqualified `:root` block in tokens.css, so it needs no selector and has
 * none. Emitting `[data-os-theme="graphite"]` rules for it would work and
 * would also mean the shell rendered unstyled for one frame on any path where
 * this module had not run yet.
 *
 * One element, rewritten wholesale. Appending per pack would leave a stale
 * rule behind after an uninstall, and stale token rules are invisible until
 * somebody re-installs a pack under an id they once had.
 */
export function applyPackStyles(
  installed: readonly OsThemePack[],
  doc: Document | undefined = globalThis.document,
): void {
  if (!doc?.head) return;
  const css = themePacks(installed)
    .filter((pack) => pack.id !== BUILT_IN_THEME_ID)
    .map(themePackCss)
    .join("\n");

  let style = doc.getElementById(THEME_STYLE_ID) as HTMLStyleElement | null;
  if (!style) {
    style = doc.createElement("style");
    style.id = THEME_STYLE_ID;
    doc.head.appendChild(style);
  }
  if (style.textContent !== css) style.textContent = css;
}
