import { createContext, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, type RefObject, type ReactNode } from "react";
import type { Breadcrumb } from "./Breadcrumbs";

interface PageNavigation {
  trail: readonly Breadcrumb[];
  primary: HTMLElement | null;
  register: (node: HTMLElement) => () => void;
}
const Context = createContext<PageNavigation | null>(null);

/** One return path per window, owned by the visible page header. Retained
 * panes and secondary headings cannot add another shell navigation row. */
export function PageNavigationProvider({ trail, root, children }: { trail: readonly Breadcrumb[]; root: RefObject<HTMLElement | null>; children: ReactNode }) {
  const [primary, setPrimary] = useState<HTMLElement | null>(null);
  const host = useMemo(() => {
    const nodes = new Set<HTMLElement>();
    const update = () => {
      const visible = [...nodes].filter(node => node.isConnected && !node.closest('[hidden], [aria-hidden="true"], [inert]'));
      visible.sort((a, b) => a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING ? -1 : 1);
      setPrimary(visible[0] ?? null);
    };
    return { update, register(node: HTMLElement) { nodes.add(node); update(); return () => { nodes.delete(node); update(); }; } };
  }, []);
  useEffect(() => {
    host.update();
    const observer = new MutationObserver(host.update);
    if (root.current) observer.observe(root.current, { subtree: true, childList: true, attributes: true, attributeFilter: ["hidden", "aria-hidden", "inert"] });
    return () => observer.disconnect();
  }, [host, root]);
  return <Context.Provider value={{ trail, primary, register: host.register }}>{children}</Context.Provider>;
}

export function usePageNavigation(enabled = true) {
  const context = useContext(Context);
  const root = useRef<HTMLDivElement>(null);
  const register = context?.register;
  useLayoutEffect(() => {
    if (enabled && root.current && register) return register(root.current);
  }, [enabled, register]);
  return { root, trail: enabled && root.current !== null && context?.primary === root.current ? context?.trail ?? [] : [] };
}
