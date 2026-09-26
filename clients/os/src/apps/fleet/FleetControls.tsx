import { useLayoutEffect, useRef } from "react";
export { RefreshButton } from "../../kit/RefreshButton";

/** One scroll position per real view, restored after a drilldown or local tab. */
export function useFleetScroll(key: string) {
  const root = useRef<HTMLDivElement>(null);
  const positions = useRef(new Map<string, number>());
  useLayoutEffect(() => {
    const scroller = root.current?.closest<HTMLElement>(".fleet-page, .os-window-content");
    if (scroller) scroller.scrollTop = positions.current.get(key) ?? 0;
    return () => { if (scroller) positions.current.set(key, scroller.scrollTop); };
  }, [key]);
  return root;
}
