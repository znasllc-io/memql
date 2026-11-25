import { useCallback } from "react";

import { useShareDirectory } from "../apps/files/useSharing";
import { useDesktopAccessAnswer } from "../chrome/desktopAccessContext";
import { useAppReach } from "../kit/ReadinessStates";
import { LENDER_FALLBACK } from "./accessWords";
import type { AskAccess } from "./askFailure";

/**
 * What Ask needs to word a refusal for THIS person: whether they can open Fleet
 * (the InferenceStop pattern: a machines section and a window to open it in),
 * the sources the gate's access check listed, and a way to name a lender.
 *
 * The sources come from the answer CoreGate already holds; an unknown answer
 * (a failed check) has none, so a person who cannot open Fleet gets the plain
 * "ask your administrator" sentence. Lenders are named from the share
 * directory, read only while there is something lent that is not ready and the
 * person cannot open Fleet. That directory is open only to people who may
 * share files, and omits people outside their organization; either leaves the
 * fallback LENDER_FALLBACK, never a wrong name.
 */
export function useAskAccess(): AskAccess {
  const answer = useDesktopAccessAnswer();
  const fleet = useAppReach("fleet");
  const canOpenFleet = fleet.sections.includes("machines") && fleet.canOpenWindows;
  const sources = answer?.state === "admitted" ? answer.sources : [];
  const lentOffline = !canOpenFleet && sources.some((s) => !s.own && s.lenderUserId !== undefined && s.state !== "ready");
  const { people } = useShareDirectory(lentOffline);
  const nameOf = useCallback(
    (id: string) => people?.find((p) => p.userId === id)?.displayName || LENDER_FALLBACK,
    [people],
  );
  return { canOpenFleet, sources, nameOf };
}
