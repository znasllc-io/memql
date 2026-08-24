import { useEffect, useState } from "react";

// The stream-activity seam behind the rail mark's "streaming" state.
//
// The SDK Connection exposes no traffic hook, and adding one for a visual
// state would put UI concerns on the wire layer. Instead, the places that
// HANDLE inbound data -- the CDC subscription handlers and the row-walk's
// page settles -- ping this module, and the mark subscribes. That means
// "streaming" means "data this document actually consumed just arrived",
// which is the honest reading anyway: a stream nobody is listening to should
// not make the mark dance.
//
// Module-level rather than context: bumps come from hooks that live under
// arbitrary providers, and a singleton whose consumers are all in one
// document is exactly what a module is.

type Listener = () => void;

const listeners = new Set<Listener>();
let lastActivityAt = 0;

export function bumpActivity(): void {
  lastActivityAt = Date.now();
  for (const listener of listeners) listener();
}

// resetActivity clears the module's clock. Production never calls it -- a
// browser has one document for the life of the page, which is the whole
// premise of the module-level singleton above.
//
// TESTS need it, and the reason is worth stating because the failure it
// prevents is invisible: `lastActivityAt` survives between tests in one module
// instance, and the mount-time catch-up below reads it. So a test that renders
// the rail within `windowMs` of ANY earlier test's row-walk or feed settle
// sees "streaming" rather than "connected" -- and cannot wait it out, because
// the window is 2500ms while waitFor's default timeout is 1000ms. That makes
// the failure ORDER-DEPENDENT and, in CI, intermittent: it depends on which
// files a worker happened to run first. test/setup.ts calls this before every
// test.
export function resetActivity(): void {
  lastActivityAt = 0;
}

// True while data arrived within the last windowMs; flips false on a quiet
// timer so the mark settles to its still connected state.
export function useRecentActivity(windowMs = 2500): boolean {
  const [active, setActive] = useState(false);

  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | null = null;
    const onBump = (): void => {
      setActive(true);
      if (timer !== null) clearTimeout(timer);
      timer = setTimeout(() => setActive(false), windowMs);
    };
    listeners.add(onBump);
    // A bump that landed just before this consumer mounted still counts --
    // the rail re-mounts on route changes and must not blink.
    if (Date.now() - lastActivityAt < windowMs) onBump();
    return () => {
      listeners.delete(onBump);
      if (timer !== null) clearTimeout(timer);
    };
  }, [windowMs]);

  return active;
}
