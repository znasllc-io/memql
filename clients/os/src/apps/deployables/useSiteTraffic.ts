import { useCallback, useEffect, useRef, useState } from "react";

import { useOsConnection } from "../../live/connection";
import { fetchTrafficSummaries, windowSpec, type TrafficSummary, type TrafficWindow } from "./traffic";

// The list's traffic figures: one read for every deployable on screen.
//
// ===========================================================================
// ONE CALL, NOT ONE PER ROW
// ===========================================================================
// The builtin's summary mode exists for exactly this: twenty rows asking for
// twenty series would pull twenty times a week of buckets to render twenty
// timestamps. The whole list is one call, summed in the database.
//
// ===========================================================================
// IT IS AN ON-DEMAND READ AND IT SAYS SO
// ===========================================================================
// v1:observability:siteTraffic is not a graph concept, so there is no
// broadcast to subscribe to and none is wanted -- a figure that moved on
// every request would be the strobe the arrival-cue rule exists to prevent.
// The list re-reads on the window's own cadence, and a deployable ABSENT from
// the answer is unmeasured rather than zero, which the row renders as nothing
// rather than as "never".
//
// ===========================================================================
// THE READ IS KEYED ON THE IDS, NOT ON THE ROWS
// ===========================================================================
// A live list's rows change identity on every folded event, so an effect
// keyed on the array would re-read on every heartbeat in the cluster. It is
// keyed on the sorted id STRING instead: the set of deployables on screen,
// which changes when one is created or removed and not otherwise.
export function useSiteTraffic(siteIds: readonly string[], window: TrafficWindow) {
  const connection = useOsConnection();
  const [figures, setFigures] = useState<Map<string, TrafficSummary>>(new Map());
  const [readAt, setReadAt] = useState("");
  const key = [...siteIds].sort().join(",");
  const asked = useRef(0);

  const read = useCallback(async () => {
    if (connection === null || key === "") return;
    const mine = ++asked.current;
    try {
      const next = await fetchTrafficSummaries(connection.query, key.split(","), window, new Date());
      if (mine !== asked.current) return;
      setFigures(next);
      setReadAt(new Date().toISOString());
    } catch {
      // A FIGURE THAT COULD NOT BE READ IS AN ABSENT FIGURE, not a broken
      // list: every row still renders everything else it knows, and the
      // deployable page beside it reports the refusal in the server's own
      // words where somebody asked for it directly.
      if (mine !== asked.current) return;
      setFigures(new Map());
    }
  }, [connection, key, window]);

  useEffect(() => {
    void read();
    const timer = setInterval(() => void read(), windowSpec(window).refreshMs);
    return () => clearInterval(timer);
  }, [read, window]);

  return { figures, readAt };
}
