import { createContext, useContext, type ReactNode } from "react";

import { useDesktopAccess } from "./useDesktopAccess";

// The ONE read of the signed-in person's own model access, shared (design
// record 2026-11-25-who-gets-into-memql-os). It decides whether they get Ask,
// never whether they enter: the cluster's readiness decides that (CoreGate).
//
// MOUNTED ABOVE ASK (Shell.tsx), because Ask's provider sits above the gate and
// is where "may this person open Ask" is answered once for the dock, the phone
// bar, the sheet and every "Ask about this". The gate reads it too, to draw the
// desk only once it is known whether Ask belongs on it, and Ask words its
// refusals from the same sources (design record 2026-11-24-desktop-access,
// section 5). Nobody makes a second read. Null outside the scope (tests,
// setup): nothing is known, and nothing is taken away.
type DesktopAccess = ReturnType<typeof useDesktopAccess>;

export const DesktopAccessContext = createContext<DesktopAccess | null>(null);

export function DesktopAccessScope({ children }: { children: ReactNode }) {
  return <DesktopAccessContext.Provider value={useDesktopAccess(true)}>{children}</DesktopAccessContext.Provider>;
}

export function useDesktopAccessAnswer(): DesktopAccess | null {
  return useContext(DesktopAccessContext);
}

/**
 * Whether the engine answered that this person has NO model access set up: no
 * machine, app, key or cluster account of any kind, theirs or lent. Such a
 * person has no Ask ("if you have no inference access, then you don't even get
 * the Ask app").
 *
 * False for everything else, on purpose: a cluster owner or developer (the
 * engine exempts them), access that is set up and asleep or out of credit, and
 * a check that failed or has not answered. A failed check is no evidence that
 * a person lacks access, and the server refuses their model calls either way.
 *
 * The engine's wire field is still `admitted`, and this client's word for
 * `admitted: false` is still "held", from when that answer kept a person out
 * of MemQL OS. It no longer does.
 */
export function useHasNoModelAccess(): boolean {
  return useContext(DesktopAccessContext)?.state === "held";
}
