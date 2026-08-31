// The theme registry (spec G). A THEME is a named `--os-*` token pack: every
// token, the wallpaper parameters, and nothing else. It is applied as
// `data-os-theme="<id>"` on the document root, and mode (light / dark /
// system, `data-theme`) stays the orthogonal mechanism beside it -- a theme
// defines BOTH looks.
//
// The foundation shipped `themePack` as a bare string with one hard-coded
// button; this is the type spec G named, so the marketplace epic (#4745) has
// a contract to sell packs into rather than a string to guess at.

/** A named token pack. */
export interface OsThemePack {
  /** Stable id; the value of `data-os-theme` on the document root. */
  id: string;
  /** Human label for the picker. */
  name: string;
  /**
   * Where the pack's token stylesheet lives. ABSENT for a built-in, whose
   * tokens are already in the bundle -- which is the load-bearing half of
   * the distinction, not a convenience: a pack with no href needs no fetch,
   * so the built-in renders on the first frame offline. The marketplace
   * epic fills this in for installed packs.
   */
  tokensHref?: string;
}

/**
 * Graphite is the unqualified `:root` token block in `styles/tokens.css` --
 * there is deliberately no `[data-os-theme="graphite"]` selector. The
 * built-in IS the default, so making it a selector would mean the shell
 * rendered unstyled until the attribute landed. The attribute is still set
 * for it, because the wallpaper watches that attribute and a pack that
 * never announces itself cannot be swapped AWAY from either.
 */
export const BUILT_IN_THEME_ID = "graphite";

export const BUILT_IN_THEMES: readonly OsThemePack[] = [
  { id: BUILT_IN_THEME_ID, name: "Graphite" },
];

/** The packs installable today. One, and the marketplace epic is #4745. */
export function themePacks(): readonly OsThemePack[] {
  return BUILT_IN_THEMES;
}

export function themePackById(id: string): OsThemePack | undefined {
  return themePacks().find((t) => t.id === id);
}

/**
 * The pack to RENDER for a stored id. An id naming nothing installed falls
 * back to the built-in rather than leaving the picker with no selection: a
 * stored pack can outlive its installation (uninstalled, or a bundle rolled
 * back), and the desktop document is not the place to discover that.
 */
export function resolveThemePack(id: string): OsThemePack {
  return themePackById(id) ?? BUILT_IN_THEMES[0]!;
}
