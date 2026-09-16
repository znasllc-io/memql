import { createContext, useContext, useEffect, useMemo, useRef, type RefObject } from "react";

interface SearchTarget { root: RefObject<HTMLElement | null>; open: () => void }
interface SearchHost { targets: Set<SearchTarget> }
export const WindowSearchContext = createContext<SearchHost | null>(null);

/** Search registrations belong to one window; parked panes never take focus. */
export function useWindowSearchHost() {
  const host = useMemo<SearchHost>(() => ({ targets: new Set() }), []);
  const openVisible = () => {
    const target = [...host.targets].find(item => {
      const node = item.root.current;
      return node?.isConnected && !node.closest('[hidden], [aria-hidden="true"], [inert]');
    });
    if (!target) return false;
    target.open();
    return true;
  };
  return { host, openVisible };
}

export function useWindowSearchTarget(root: RefObject<HTMLElement | null>, open: () => void) {
  const host = useContext(WindowSearchContext);
  const latest = useRef(open);
  latest.current = open;
  useEffect(() => {
    if (!host) return;
    const target = { root, open: () => latest.current() };
    host.targets.add(target);
    return () => { host.targets.delete(target); };
  }, [host, root]);
  return host !== null;
}
