// IntersectionObserver for jsdom, which has none and lays nothing out.
//
// A FAKE THE TEST DRIVES rather than a shim that never fires: jsdom cannot say
// whether anything is in view, so a test says it, with `intersect(target)`,
// and every live observer watching that target hears it as the browser would
// tell it. Installed once by test/setup.ts, so a component that observes
// renders in every suite without throwing; a suite that never calls
// `intersect` simply never sees anything come into view.
//
// The constructor declares both parameters for the reason the ResizeObserver
// shim beside it records: a double narrower than the API it stands in for
// reads, to a scanner, as a production call passing a superfluous argument.

export class FakeIntersectionObserver implements IntersectionObserver {
  static readonly live = new Set<FakeIntersectionObserver>();

  readonly root: Element | Document | null;
  readonly rootMargin: string;
  readonly scrollMargin: string;
  readonly thresholds: readonly number[];
  private readonly targets = new Set<Element>();

  constructor(private readonly callback: IntersectionObserverCallback, options?: IntersectionObserverInit) {
    this.root = options?.root ?? null;
    this.rootMargin = options?.rootMargin ?? "0px";
    this.scrollMargin = "0px";
    const threshold = options?.threshold ?? 0;
    this.thresholds = Array.isArray(threshold) ? threshold : [threshold as number];
    FakeIntersectionObserver.live.add(this);
  }

  observe(target: Element) {
    this.targets.add(target);
  }

  unobserve(target: Element) {
    this.targets.delete(target);
  }

  disconnect() {
    this.targets.clear();
    FakeIntersectionObserver.live.delete(this);
  }

  takeRecords(): IntersectionObserverEntry[] {
    return [];
  }

  /** Whether this observer is watching `target`. */
  watches(target: Element): boolean {
    return this.targets.has(target);
  }

  /** Tell this observer that `target` came into view, or left it. */
  report(target: Element, isIntersecting: boolean) {
    if (!this.targets.has(target)) return;
    const rect = target.getBoundingClientRect();
    const entry = {
      target,
      isIntersecting,
      intersectionRatio: isIntersecting ? 1 : 0,
      boundingClientRect: rect,
      intersectionRect: rect,
      rootBounds: null,
      time: performance.now(),
    } as IntersectionObserverEntry;
    this.callback([entry], this);
  }
}

/** Every live observer watching `target` hears that it came into view (or left). */
export function intersect(target: Element, isIntersecting = true) {
  for (const observer of [...FakeIntersectionObserver.live]) observer.report(target, isIntersecting);
}

/** Whether any live observer is watching `target`. */
export function observed(target: Element): boolean {
  return [...FakeIntersectionObserver.live].some((observer) => observer.watches(target));
}

export function installFakeIntersectionObserver() {
  if ((globalThis as { IntersectionObserver?: unknown }).IntersectionObserver) return;
  Object.defineProperty(globalThis, "IntersectionObserver", { configurable: true, writable: true, value: FakeIntersectionObserver });
}
