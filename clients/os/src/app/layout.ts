// Chrome layout is keyed off pointer/hover, never window.innerWidth
// (memql#4705). A large phone still gets phone chrome. An iPad that
// reports coarse + hover must not paint desktop hover chrome.

export type ChromeLayout = "phone" | "ipad" | "desktop";

export interface MediaQueryLike {
  matches: boolean;
}

export type MatchMedia = (query: string) => MediaQueryLike;

// layoutFromMedia is the pure split the chrome keys off.
//
//   pointer:fine + hover:hover  -> desktop (pointer frame, two reserved slots)
//   pointer:coarse + hover:hover -> iPad (touch-first, no hover chrome)
//   otherwise                   -> phone (own chrome, no slots)
export function layoutFromMedia(matchMedia: MatchMedia): ChromeLayout {
  const hover = matchMedia("(hover: hover)").matches;
  const fine = matchMedia("(pointer: fine)").matches;
  const coarse = matchMedia("(pointer: coarse)").matches;
  if (fine && hover) return "desktop";
  if (coarse && hover) return "ipad";
  return "phone";
}

export function layoutFromWindow(win: { matchMedia?: MatchMedia } | undefined): ChromeLayout {
  if (!win?.matchMedia) return "phone";
  return layoutFromMedia((q) => win.matchMedia!(q));
}
