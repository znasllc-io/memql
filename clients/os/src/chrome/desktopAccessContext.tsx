import { createContext, useContext } from "react";

import type { DesktopAccessAnswer } from "./useDesktopAccess";

// The ONE desktop access answer, shared. CoreGate asks (useDesktopAccess) and
// provides it here, so Ask can say what is offline from the same read the gate
// holds and never makes a second one (design record 2026-11-24-desktop-access,
// section 5). Null outside the gate (tests, setup): no sources are known.
export const DesktopAccessContext = createContext<DesktopAccessAnswer | null>(null);

export function useDesktopAccessAnswer(): DesktopAccessAnswer | null {
  return useContext(DesktopAccessContext);
}
