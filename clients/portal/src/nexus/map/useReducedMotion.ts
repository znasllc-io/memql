import { useEffect, useState } from "react";

import { prefersReducedMotion } from "./webgl";

// The reduced-motion preference, and changes to it.
//
// Design D7: the scene reads the preference itself, because the portal's CSS
// rule cannot reach inside a canvas. Subscribing rather than sampling once
// matters more here than it usually does -- a person who turns the preference
// ON is often doing it BECAUSE of what is currently moving on their screen,
// and a scene that keeps animating until the next navigation has ignored them
// at the exact moment they asked.
export function useReducedMotion(): boolean {
  const [reduced, setReduced] = useState(prefersReducedMotion);

  useEffect(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") return;
    let query: MediaQueryList;
    try {
      query = window.matchMedia("(prefers-reduced-motion: reduce)");
    } catch {
      return;
    }
    const onChange = (): void => setReduced(query.matches);
    setReduced(query.matches);
    // addEventListener is the modern form; addListener is what older Safari
    // has. Both are guarded because jsdom's MediaQueryList has neither unless
    // the test stubs matchMedia, and a throw here would take the page down
    // over a preference.
    if (typeof query.addEventListener === "function") {
      query.addEventListener("change", onChange);
      return () => query.removeEventListener("change", onChange);
    }
    return undefined;
  }, []);

  return reduced;
}
