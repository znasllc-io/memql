/** Scroll memory belongs to a window, not to an app's mounted page components.
 * Record before navigation mutates the DOM: layout-effect cleanup is too late
 * when replacing a long page with a short one has already clamped its scroll. */
type Position = { top: number; left: number };
const hidden = '[hidden], [inert], [aria-hidden="true"]';
const excluded = 'dialog, [role="dialog"], [popover], [data-os-scroll-ignore]';
const position = (node: HTMLElement): Position => ({ top: node.scrollTop, left: node.scrollLeft });
const same = (a: Position, b: Position) => Math.abs(a.top - b.top) < 1 && Math.abs(a.left - b.left) < 1;

export function createPageScroll(root: HTMLElement, page: () => string | null) {
  // The desktop window itself has role="dialog". Exclude overlays and scroll
  // owners inside this page, not its enclosing window. Hidden ancestors still
  // matter: a retained page in a hidden window must not overwrite its memory.
  const shown = (node: HTMLElement) => node.isConnected && !node.closest(hidden)
    && !root.contains(node.closest(excluded));
  const history = new Map<string, Map<string, Position>>();
  let current: string | null = null;
  let containers = new Map<HTMLElement, string>();
  let pending = new Map<string, Position>();
  const applied = new WeakMap<HTMLElement, Position>();
  let stopped = false, scheduled = false, dirty = true;

  // A stable DOM address survives an unmounted page. Pages with multiple
  // independently reordered panes can name them with data-os-scroll-id.
  function address(node: HTMLElement): string {
    if (node === root) return "root";
    if (node.dataset.osScrollId) return `id:${node.dataset.osScrollId}`;
    const parts: string[] = [];
    for (let at: HTMLElement | null = node; at && at !== root; at = at.parentElement) {
      const siblings = Array.from(at.parentElement?.children ?? []);
      parts.push(`${at.tagName}:${siblings.indexOf(at)}`);
    }
    return parts.reverse().join("/");
  }
  function findContainers() {
    const found = new Map<HTMLElement, string>();
    for (const node of [root, ...root.querySelectorAll<HTMLElement>("*")]) {
      if (!shown(node) || node.matches("input, textarea, select")) continue;
      const style = getComputedStyle(node);
      if (node === root || /auto|scroll/.test(`${style.overflow} ${style.overflowX} ${style.overflowY}`)) found.set(node, address(node));
    }
    return found;
  }
  function remember(node: HTMLElement, key: string) {
    if (current && shown(node)) history.get(current)?.set(key, position(node));
  }
  function capture() {
    if (page() !== current) return; // A queued event from a page already left.
    for (const [node, key] of containers) if (!pending.has(key)) remember(node, key);
  }
  function restore() {
    for (const [node, key] of containers) {
      const wanted = pending.get(key);
      if (!wanted) continue;
      // Assignment is immediate, including with reduced motion or a themed
      // smooth-scroll rule. Keep the target if loading content is still short.
      node.scrollTo({ top: wanted.top, left: wanted.left, behavior: "instant" });
      const actual = position(node);
      applied.set(node, actual);
      if (same(actual, wanted)) pending.delete(key);
    }
  }
  const resize = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(() => restore());
  function update() {
    scheduled = false;
    if (stopped) return;
    const next = page();
    if (!next || !shown(root)) { current = null; pending.clear(); resize?.disconnect(); return; }
    const changed = next !== current;
    current = next;
    if (!history.has(next)) history.set(next, new Map());
    // Bounded, per-window memory. Closing the window drops it altogether.
    const saved = history.get(next)!;
    history.delete(next); history.set(next, saved);
    if (history.size > 100) history.delete(history.keys().next().value!);
    if (!changed && !dirty) { restore(); return; }
    dirty = false;
    const previous = containers;
    containers = findContainers();
    if (changed) pending = new Map(saved);
    for (const [node, key] of containers) {
      if (changed || !previous.has(node)) {
        const target = saved.get(key) ?? { top: 0, left: 0 };
        saved.set(key, target);
        pending.set(key, target);
      }
    }
    restore();
    resize?.disconnect();
    for (const node of containers.keys()) {
      resize?.observe(node);
      // A scroller's own size need not change when its contents finish loading.
      for (const child of node.children) resize?.observe(child);
    }
  }
  function schedule() {
    if (stopped || scheduled) return;
    scheduled = true;
    // Let all headings publish and retained panes settle in this React commit.
    // A transient missing heading must not become another navigation entry.
    queueMicrotask(update);
  }
  function onScroll(event: Event) {
    const node = event.target;
    if (!(node instanceof HTMLElement) || page() !== current || !shown(node)) return;
    const key = containers.get(node);
    if (key === undefined) return;
    const last = applied.get(node);
    applied.delete(node);
    if (last && same(last, position(node))) return;
    pending.delete(key);
    remember(node, key);
  }
  function interrupt() {
    // Once the person takes over, late content must not pull them back.
    pending.clear();
    capture();
  }
  function onKey(event: KeyboardEvent) {
    if (["ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight", "PageUp", "PageDown", "Home", "End", " ", "Tab"].includes(event.key)) interrupt();
    else capture();
  }
  // The frame includes its tabs and trail, which sit outside the scroller.
  const frame = root.parentElement ?? root;
  frame.addEventListener("click", capture, true);
  frame.addEventListener("pointerdown", capture, true);
  root.addEventListener("scroll", onScroll, true);
  root.addEventListener("wheel", interrupt, { passive: true, capture: true });
  root.addEventListener("touchstart", interrupt, { passive: true, capture: true });
  frame.addEventListener("keydown", onKey, true);
  const mutations = new MutationObserver(records => {
    if (records.some(record => record.type !== "characterData")) dirty = true;
    schedule();
  });
  mutations.observe(root, { subtree: true, childList: true, characterData: true, attributes: true, attributeFilter: ["hidden", "aria-hidden", "inert", "data-os-scroll-page"] });
  schedule();
  return { schedule, dispose() {
    stopped = true;
    mutations.disconnect(); resize?.disconnect();
    frame.removeEventListener("click", capture, true);
    frame.removeEventListener("pointerdown", capture, true);
    frame.removeEventListener("keydown", onKey, true);
    root.removeEventListener("scroll", onScroll, true);
    root.removeEventListener("wheel", interrupt, true);
    root.removeEventListener("touchstart", interrupt, true);
    history.clear();
  } };
}
