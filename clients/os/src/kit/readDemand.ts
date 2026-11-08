import { LiveReadVisibility } from "../live/useGraphRevision";
import { useCallback, useContext, useEffect, useState } from "react";

/** Shell-level data is shared, but is not startup work. Only a mounted
 * consumer retains a read; the session collection cache owns the last answer
 * after the final consumer leaves. A stable retain function keeps ordinary
 * provider updates from releasing and reacquiring every consumer. */
export function useReadDemand() {
  const [consumers, setConsumers] = useState(0);
  const retain = useCallback(() => {
    setConsumers(value => value + 1);
    return () => setConsumers(value => value - 1);
  }, []);
  return { requested: consumers > 0, retain };
}

export function useDemandedRead(retain: (() => () => void) | undefined, enabled = true): void {
  const visible = useContext(LiveReadVisibility);
  useEffect(() => enabled && visible ? retain?.() : undefined, [retain, enabled, visible]);
}
