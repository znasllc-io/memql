import { createContext, useContext, useEffect, useMemo, useRef, useSyncExternalStore, type RefObject } from "react";

interface SearchTarget { root: RefObject<HTMLElement | null>; open: () => void }
interface SearchHost { targets: Set<SearchTarget>; loading: number; version: number; listeners: Set<() => void> }
function notify(host: SearchHost) { host.version++; for (const listener of host.listeners) listener(); }
export const WindowSearchContext = createContext<SearchHost | null>(null);

/** Search registrations belong to one window; parked panes never take focus. */
export function useWindowSearchHost() {
  const host = useMemo<SearchHost>(() => ({ targets: new Set(), loading: 0, version: 0, listeners: new Set() }), []);
  const revision = useSyncExternalStore(useMemo(() => (listener: () => void) => { host.listeners.add(listener); return () => { host.listeners.delete(listener); }; }, [host]), () => host.version);
  const openVisible = () => {
    const target = [...host.targets].find(item => {
      const node = item.root.current;
      return node?.isConnected && !node.closest('[hidden], [aria-hidden="true"], [inert]');
    });
    if (!target) return false;
    target.open();
    return true;
  };
  return { host, openVisible, revision };
}

export function useWindowSearchTarget(root: RefObject<HTMLElement | null>, open: () => void) {
  const host = useContext(WindowSearchContext);
  const latest = useRef(open);
  latest.current = open;
  useEffect(() => {
    if (!host) return;
    const target = { root, open: () => latest.current() };
    host.targets.add(target); notify(host);
    return () => { host.targets.delete(target); notify(host); };
  }, [host, root]);
  return host !== null;
}

/** Keep an early window-search request pending while its app chunk opens. */
export function useWindowSearchLoading(loading: boolean) {
  const host = useContext(WindowSearchContext);
  useEffect(() => {
    if (!host || !loading) return;
    host.loading++; notify(host);
    return () => { host.loading--; notify(host); };
  }, [host, loading]);
}
