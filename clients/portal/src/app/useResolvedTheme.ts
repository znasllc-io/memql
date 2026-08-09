import { useEffect, useState } from "react";

// The theme currently ON SCREEN, as light or dark.
//
// src/app/theme.ts owns the CHOICE (light / dark / follow the OS) and writes
// the `data-theme` attribute. This is the other half: what that choice
// actually resolved to right now, which is a different question whenever the
// choice is "system".
//
// WHY ANYTHING NEEDS THE ANSWER. Almost nothing does -- the token layer
// resolves in CSS and every portal surface inherits it. The exception is
// view-kit's chart palette: a hue legible on white is not legible on
// near-black, so view-kit ships a validated palette per theme, selects between
// them with `prefers-color-scheme`, and offers `{ theme }` as the override for
// a host whose chrome disagrees with the OS (sdk/ts-viewkit/src/styles.ts).
// The portal is exactly that host -- an operator on a light OS who picked dark
// would otherwise get light-mode chart hues on the portal's dark surface.
//
// Both inputs are watched, because either can change without the other: the
// operator flips the ThemeToggle (attribute mutates) or changes their OS
// appearance while the tab is open (media query fires).

export type ResolvedTheme = "light" | "dark";

const DARK_QUERY = "(prefers-color-scheme: dark)";

function prefersDark(): boolean {
  // jsdom implements no matchMedia, and a hardened browser profile can throw.
  // Neither is worth failing a render over; light is the documented default.
  try {
    return globalThis.matchMedia?.(DARK_QUERY).matches === true;
  } catch {
    return false;
  }
}

export function resolveTheme(): ResolvedTheme {
  const stamped = globalThis.document?.documentElement.getAttribute("data-theme");
  // An explicit stamp wins in BOTH directions. That is the whole point of the
  // attribute: "dark on a light OS" and "light on a dark OS" are both real
  // choices an operator can make.
  if (stamped === "dark" || stamped === "light") return stamped;
  return prefersDark() ? "dark" : "light";
}

export function useResolvedTheme(): ResolvedTheme {
  const [theme, setTheme] = useState<ResolvedTheme>(() => resolveTheme());

  useEffect(() => {
    const update = (): void => setTheme(resolveTheme());
    // Re-read on mount: theme.ts applies the stored choice at module import
    // time, which can land after this component's initial state was computed.
    update();

    const root = globalThis.document?.documentElement;
    const observer =
      root && typeof MutationObserver !== "undefined"
        ? new MutationObserver(update)
        : null;
    observer?.observe(root as Element, {
      attributes: true,
      attributeFilter: ["data-theme"],
    });

    let media: MediaQueryList | null = null;
    try {
      media = globalThis.matchMedia?.(DARK_QUERY) ?? null;
    } catch {
      media = null;
    }
    media?.addEventListener("change", update);

    return () => {
      observer?.disconnect();
      media?.removeEventListener("change", update);
    };
  }, []);

  return theme;
}
