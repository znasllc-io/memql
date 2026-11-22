import { useEffect, useState } from "react";

/**
 * The measure of a wait while it lasts: "0:07", counted from when `waiting`
 * became true. Empty when nothing is being waited for.
 *
 * It ticks only while waiting, so a page that is not waiting does not render
 * once a second. The text belongs in a bar's `meta` (DESIGN.md rule 12): text
 * that changes on its own stands outside what the bar's press hold watches.
 */
export function useElapsed(waiting: boolean): string {
  const [since, setSince] = useState(0);
  const [, setTick] = useState(0);
  useEffect(() => {
    if (!waiting) return;
    setSince(Date.now());
    const timer = setInterval(() => setTick((n) => n + 1), 1_000);
    return () => clearInterval(timer);
  }, [waiting]);
  if (!waiting || since === 0) return waiting ? "0:00" : "";
  const seconds = Math.max(0, Math.floor((Date.now() - since) / 1000));
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;
}
