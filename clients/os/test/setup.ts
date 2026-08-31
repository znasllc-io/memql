(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

class MemoryStorage implements Storage {
  private store = new Map<string, string>();
  get length() {
    return this.store.size;
  }
  clear() {
    this.store.clear();
  }
  getItem(key: string) {
    return this.store.get(key) ?? null;
  }
  key(index: number) {
    return [...this.store.keys()][index] ?? null;
  }
  removeItem(key: string) {
    this.store.delete(key);
  }
  setItem(key: string, value: string) {
    this.store.set(key, value);
  }
}

Object.defineProperty(globalThis, "localStorage", {
  value: new MemoryStorage(),
  configurable: true,
});
Object.defineProperty(globalThis, "sessionStorage", {
  value: new MemoryStorage(),
  configurable: true,
});

// ---- shims the desktop chrome needs under jsdom ----

// matchMedia: layout + reduced-motion checks read it; jsdom has none.
if (!window.matchMedia) {
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    value: (query: string) => ({
      matches: false,
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      onchange: null,
      dispatchEvent: () => false,
    }),
  });
}

// ResizeObserver: the wallpaper canvas observes its parent.
if (!(globalThis as { ResizeObserver?: unknown }).ResizeObserver) {
  class ResizeObserverShim {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  (globalThis as { ResizeObserver?: unknown }).ResizeObserver = ResizeObserverShim;
}

// Element.scrollTo: the Ask log pins to its bottom; jsdom throws on it.
if (!Element.prototype.scrollTo) {
  Element.prototype.scrollTo = () => {};
}

// PointerEvent: jsdom implements none at all, so `fireEvent.pointerDown` falls
// back to a plain Event and the coordinates never reach the handler -- a pan
// computed from them comes out NaN, and the surface reads as broken when the
// only thing missing is the event type.
//
// A pointer-driven surface is not an optional thing to test here: the deploy
// map's pan and pinch ARE the map, and "works with pointer and touch" is a
// property of one code path rather than of two that have to agree. MouseEvent
// already carries clientX/clientY, so the shim is the pointer fields on top.
if (!(globalThis as { PointerEvent?: unknown }).PointerEvent) {
  class PointerEventShim extends MouseEvent {
    readonly pointerId: number;
    readonly pointerType: string;
    readonly isPrimary: boolean;
    constructor(type: string, init: PointerEventInit = {}) {
      super(type, init);
      this.pointerId = init.pointerId ?? 1;
      this.pointerType = init.pointerType ?? "mouse";
      this.isPrimary = init.isPrimary ?? true;
    }
  }
  Object.defineProperty(globalThis, "PointerEvent", {
    configurable: true,
    value: PointerEventShim,
  });
  Object.defineProperty(window, "PointerEvent", {
    configurable: true,
    value: PointerEventShim,
  });
}
