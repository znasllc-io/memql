import { createContext, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, type RefObject, type ReactNode } from "react";
import type { Breadcrumb } from "./Breadcrumbs";

/** What a page heading tells its window about where the person is. */
export interface PageTrail {
  title: string;
  breadcrumbs?: readonly Breadcrumb[];
  back?: { label: string; onSelect: () => void };
}

interface PageNavigation {
  trail: readonly Breadcrumb[];
  primary: HTMLElement | null;
  register: (node: HTMLElement) => () => void;
  publish: (node: HTMLElement, page: PageTrail) => void;
}

/**
 * What the window's one trail row reads. SEPARATE from `PageNavigation` on
 * purpose: a heading that consumed this would re-render every time a heading
 * published, publish again because its `breadcrumbs` array is a fresh literal
 * each render, and loop. Headings write; only the row reads.
 */
interface WindowTrail {
  trail: readonly Breadcrumb[];
  /** Read at CLICK time as well as render time, so a handler is never stale. */
  page: () => PageTrail | null;
  /** Changes only when what the row DRAWS changes -- labels, not closures. */
  signature: string;
}

const Context = createContext<PageNavigation | null>(null);
const TrailContext = createContext<WindowTrail | null>(null);

const visible = (node: HTMLElement) => node.isConnected && !node.closest('[hidden], [aria-hidden="true"], [inert]');

function signatureOf(page: PageTrail | null): string {
  if (!page) return "";
  const crumbs = (page.breadcrumbs ?? []).map(item => `${item.label}${item.onSelect ? "*" : ""}`).join("\u001f");
  return `${page.title}\u001e${crumbs}\u001e${page.back ? page.back.label : ""}`;
}

/**
 * One return path per window.
 *
 * THE ROW IS THE SHELL'S, NOT THE PAGE'S. A heading used to draw its own
 * breadcrumbs above its title and its own back arrow beside it, and "one
 * trail" was a convention every app had to honour -- which is how Fleet came
 * to draw two: a heading handed an explicit `breadcrumbs` prop drew them
 * whether or not it was the window's primary heading. Now a heading PUBLISHES
 * where it is and `TrailRow`, rendered once by the window frame, draws it. Any
 * number of headings can mount; the window still has exactly one row, because
 * there is exactly one place that draws one.
 *
 * The primary heading is still the first VISIBLE one in document order, so
 * retained panes and secondary headings cannot claim the trail.
 */
export function PageNavigationProvider({ trail, root, children }: { trail: readonly Breadcrumb[]; root: RefObject<HTMLElement | null>; children: ReactNode }) {
  const [state, setState] = useState<{ primary: HTMLElement | null; signature: string }>({ primary: null, signature: "" });
  const host = useMemo(() => {
    const nodes = new Set<HTMLElement>();
    const pages = new Map<HTMLElement, PageTrail>();
    let primary: HTMLElement | null = null;
    const update = () => {
      const shown = [...nodes].filter(visible);
      shown.sort((a, b) => a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING ? -1 : 1);
      primary = shown[0] ?? null;
      const signature = signatureOf(primary ? pages.get(primary) ?? null : null);
      // Bail out on no change: this runs on every heading render, and a new
      // object here would re-render the row for nothing.
      setState(prev => prev.primary === primary && prev.signature === signature ? prev : { primary, signature });
    };
    return {
      update,
      register(node: HTMLElement) { nodes.add(node); update(); return () => { nodes.delete(node); pages.delete(node); update(); }; },
      publish(node: HTMLElement, page: PageTrail) { pages.set(node, page); update(); },
      page: () => (primary ? pages.get(primary) ?? null : null),
    };
  }, []);
  useEffect(() => {
    host.update();
    const observer = new MutationObserver(host.update);
    if (root.current) observer.observe(root.current, { subtree: true, childList: true, attributes: true, attributeFilter: ["hidden", "aria-hidden", "inert"] });
    return () => observer.disconnect();
  }, [host, root]);
  const navigation = useMemo(() => ({ trail, primary: state.primary, register: host.register, publish: host.publish }), [trail, state.primary, host]);
  const windowTrail = useMemo(() => ({ trail, page: host.page, signature: state.signature }), [trail, host, state.signature]);
  return <Context.Provider value={navigation}><TrailContext.Provider value={windowTrail}>{children}</TrailContext.Provider></Context.Provider>;
}

/**
 * A heading's side of the contract.
 *
 * `hosted` says a window frame is drawing the trail row, so the heading must
 * not draw navigation of its own. Outside a window -- a section rendered on
 * its own, which is how most of the suite renders one -- there is no row, and
 * the heading keeps its inline breadcrumbs and back arrow so it still works.
 */
export function usePageNavigation(enabled = true, page?: PageTrail) {
  const context = useContext(Context);
  const root = useRef<HTMLDivElement>(null);
  const register = context?.register;
  const publish = context?.publish;
  useLayoutEffect(() => {
    if (enabled && root.current && register) return register(root.current);
  }, [enabled, register]);
  // No dependency list, deliberately: `breadcrumbs` and `back` are fresh
  // literals on every render, so any list that named them would fire every
  // time anyway. The provider compares by SIGNATURE and drops a no-op.
  useLayoutEffect(() => {
    if (enabled && root.current && publish && page) publish(root.current, page);
  });
  return {
    root,
    hosted: context !== null,
    trail: enabled && root.current !== null && context?.primary === root.current ? context?.trail ?? [] : [],
  };
}

/** The trail row's side: the window's origin trail and the primary heading. */
export function useWindowTrail(): WindowTrail | null {
  return useContext(TrailContext);
}
