import { useEffect, useState } from "react";

// The reduced-motion preference, read once and subscribed to.
//
// It lives in the kit rather than beside any one consumer because two
// different kinds of code need it and they need the SAME answer: CSS-driven
// components (the Constellation's assemble, Synapse's token float) that want
// to render a different tree, and the Nexus scene, whose canvas no stylesheet
// can reach inside of.
//
// SUBSCRIBING RATHER THAN SAMPLING ONCE is the part that matters. A person who
// turns the preference ON is very often doing it BECAUSE of what is moving on
// their screen right now, and a component that keeps animating until the next
// navigation has ignored them at the exact moment they asked.

// prefersReducedMotion is the plain read, for callers that are not components
// -- the scene's pure motion helpers, and the hook's own initial state.
//
// Every failure mode answers false: no window (SSR), no matchMedia (jsdom), a
// hardened profile that throws. False is the safe direction here because it
// means "render the ordinary interface"; a component that guessed `true` would
// silently ship the static variant to everyone.
export function prefersReducedMotion(): boolean {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") return false;
  try {
    return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  } catch {
    return false;
  }
}

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
    // Guarded because jsdom's MediaQueryList has neither method unless a test
    // stubs matchMedia, and a throw here would take a page down over a
    // preference.
    if (typeof query.addEventListener === "function") {
      query.addEventListener("change", onChange);
      return () => query.removeEventListener("change", onChange);
    }
    return undefined;
  }, []);

  return reduced;
}
