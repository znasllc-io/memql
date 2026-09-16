import { useLayoutEffect, useRef } from "react";
export { RefreshButton } from "../../kit/RefreshButton";

export function FleetTabs<T extends string>({ label, value, onChange, options }: { label: string; value: T; onChange: (value: T) => void; options: readonly (readonly [T, string])[] }) {
  return <nav className="fleet-local-tabs" aria-label={label}>{options.map(([id, name]) => <button type="button" key={id} aria-current={value === id ? "page" : undefined} onClick={() => onChange(id)}>{name}</button>)}</nav>;
}

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
