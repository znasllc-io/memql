import { useEffect, useState } from "react";

// A ticking clock, for the readings that change with no event behind them.
//
// Written for the Fleet, promoted here when the Users app needed the same
// thing (memql#4734): every surface that renders elapsed time needs ONE clock
// per section, or two rows on one screen disagree about what "now" is.
//
// A machine going OFFLINE produces nothing: `lastSeenAt` simply stops being
// bumped, and the row on screen is already correct. Without a clock the dot
// would stay green until some unrelated row changed and forced a re-render --
// so the fleet would look healthy for as long as nothing else happened, which
// is exactly when an operator is most likely to be looking.
//
// One clock per section rather than one per row: every freshness string and
// every dot in a section resolves against the SAME instant, so two rows
// cannot disagree about what "now" is. The default interval is the heartbeat
// cadence (docs/public/operate/workers-runbook.md), which is the fastest rate
// at which any of these readings can actually change.
export function useNow(intervalMs = 15_000): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const timer = setInterval(() => setNow(new Date()), intervalMs);
    return () => clearInterval(timer);
  }, [intervalMs]);
  return now;
}
