import { useCallback, useEffect, useState } from "react";

import { flatten } from "../kit/rows";
import { useOsConnection } from "../live/connection";
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
 * "ask your administrator" sentence.
 *
 * Lenders are named with `userDisplayById`, the @public read that answers a
 * display name and nothing else for any signed-in person, so it needs no
 * files part and reaches people outside the organization. It is asked once
 * per lender id a not-ready lent source names, and each answer is remembered
 * (a failed or empty one too, so a bad id is not asked again). An id that
 * answers nothing is named LENDER_FALLBACK, never a wrong name.
 */
export function useAskAccess(): AskAccess {
  const answer = useDesktopAccessAnswer();
  const connection = useOsConnection();
  const fleet = useAppReach("fleet");
  const canOpenFleet = fleet.sections.includes("machines") && fleet.canOpenWindows;
  const sources = answer?.state === "admitted" ? answer.sources : [];
  const [names, setNames] = useState<Record<string, string>>({});
  const wanted = canOpenFleet
    ? []
    : Array.from(new Set(sources.filter((s) => !s.own && s.state !== "ready").map((s) => s.lenderUserId ?? "").filter((id) => id !== "")));
  const key = wanted.join("|");
  useEffect(() => {
    if (!connection) return;
    const abort = new AbortController();
    for (const id of wanted) {
      if (id in names) continue;
      // Reserve the id first, so a re-render while the read is out does not ask twice.
      setNames((have) => (id in have ? have : { ...have, [id]: "" }));
      void Promise.resolve()
        .then(() => connection.query.userDisplayById({ userId: id }, { signal: abort.signal, priority: "background" }))
        .then((result) => {
          const row = result.rows()[0];
          const name = row ? String(flatten(row)["displayName"] ?? "").trim() : "";
          if (name !== "" && !abort.signal.aborted) setNames((have) => ({ ...have, [id]: name }));
        })
        .catch(() => {});
    }
    return () => abort.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [connection, key]);
  const nameOf = useCallback((id: string) => names[id] || LENDER_FALLBACK, [names]);
  return { canOpenFleet, sources, nameOf };
}
