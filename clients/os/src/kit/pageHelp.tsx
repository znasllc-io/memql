import { createContext, useContext, useId, useLayoutEffect, useMemo, useRef, useSyncExternalStore, type ReactNode, type RefObject } from "react";

interface HelpSection { id: string; title: string; children: ReactNode }
const EMPTY: readonly HelpSection[] = [];
const empty = () => EMPTY;
const noopSubscribe = () => () => {};

function createHelpHost() {
  const sections = new Map<HTMLElement, HelpSection>();
  const listeners = new Set<() => void>();
  let snapshot = EMPTY;
  const refresh = () => {
    const shown = [...sections].filter(([node]) => node.isConnected && !node.closest('[hidden], [aria-hidden="true"], [inert], dialog:not([open])'));
    shown.sort(([a], [b]) => a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING ? -1 : 1);
    const next = shown.map(([, section]) => section);
    if (next.length === snapshot.length && next.every((section, index) => section === snapshot[index])) return;
    snapshot = next;
    listeners.forEach(listener => listener());
  };
  return {
    refresh,
    register(node: HTMLElement) { return () => { sections.delete(node); refresh(); }; },
    publish(node: HTMLElement, section: HelpSection) {
      const previous = sections.get(node);
      if (previous?.title === section.title && previous.children === section.children) return;
      sections.set(node, section);
      refresh();
    },
    subscribe(listener: () => void) { listeners.add(listener); return () => { listeners.delete(listener); }; },
    snapshot: () => snapshot,
  };
}

const Context = createContext<ReturnType<typeof createHelpHost> | null>(null);

/** One documentation scope per window or modal. Publishers never subscribe,
 * so changing help cannot rerender its source and start a publication loop. */
export function PageHelpProvider({ root, children }: { root: RefObject<HTMLElement | null>; children: ReactNode }) {
  const host = useMemo(createHelpHost, []);
  useLayoutEffect(() => {
    host.refresh();
    const observer = new MutationObserver(host.refresh);
    if (root.current) observer.observe(root.current, { subtree: true, childList: true, attributes: true, attributeFilter: ["hidden", "aria-hidden", "inert", "open"] });
    return () => observer.disconnect();
  }, [host, root]);
  return <Context.Provider value={host}>{children}</Context.Provider>;
}

/** Publish supporting documentation to the page's single help entry. Hidden
 * retained tabs keep their content but cannot contribute to another page. */
export function PageHelp({ title, children }: { title: string; children: ReactNode }) {
  const host = useContext(Context);
  const anchor = useRef<HTMLSpanElement>(null);
  const id = useId();
  useLayoutEffect(() => {
    if (host && anchor.current) return host.register(anchor.current);
  }, [host]);
  useLayoutEffect(() => {
    if (host && anchor.current) host.publish(anchor.current, { id, title, children });
  }, [host, id, title, children]);
  // Standalone components have no window trail. Keep their documentation
  // reachable without introducing another information-icon control.
  return host ? <span ref={anchor} className="os-page-help-anchor" />
    : <details className="os-page-help-inline"><summary>{title}</summary>{children}</details>;
}

export function usePageHelp() {
  const host = useContext(Context);
  return useSyncExternalStore(host?.subscribe ?? noopSubscribe, host?.snapshot ?? empty, empty);
}
