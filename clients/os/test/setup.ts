import { configure, getConfig } from "@testing-library/react";

import { setRoleLadder } from "../src/system/roles";
import { installFakeIntersectionObserver } from "./intersectionObserver";
import { installSeededAccess } from "./seededAccess";
import { SEEDED_LADDER } from "./seededLadder";

// THE ROLE LADDER IS CLUSTER STATE NOW (epic memql#4832, D1), and in
// production the shell reads it before it renders anything gated. jsdom has
// no cluster, so every suite would otherwise run against an EMPTY ladder --
// under which roleAdmits refuses everything gated and a launcher test would
// pass while measuring nothing.
//
// Installed here rather than per file so the default matches production. A
// test that wants the pre-load state clears it explicitly, which reads as the
// deliberate act it is.
setRoleLadder(SEEDED_LADDER);

// THE EFFECTIVE CAPABILITY SET IS CLUSTER STATE TOO (epic memql#5289, D10),
// and the shell draws nothing gated until it lands. The default here is the
// OWNER's seeded set -- the role most harnesses sign in as -- read from the
// seeds rather than written down (see seededAccess.ts). A suite that renders
// as another role installs that role's set with `installSeededAccess(role)`
// before it renders, which reads as the deliberate act it is; a suite about
// the pre-read window clears it explicitly.
installSeededAccess("owner");

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

// A WAIT HANDS BACK AFTER THE EFFECTS OF THE COMMIT IT WAITED FOR.
//
// Testing Library turns act off while a `findBy*` or `waitFor` waits, so
// content a read brings in is committed by React's own scheduler. react-dom
// asks for a paint after such a commit and the scheduler always yields to
// that, so the commit's passive effects run in a LATER task. Testing Library
// then hands the test back on a zero-delay timer of its own. React's task is
// an immediate and normally runs first. When a millisecond boundary passes
// between arming that timer and Node's next timer phase, the timer runs
// first, and the test carries on among components whose mount effects are
// still to come. Whatever it does next, a late effect can undo or use up: one
// that resets a draft overwrites what the test just typed, one that reads a
// ref takes the focus target a click just set.
//
// Measured on Node 22 with nothing but the two timers: the timer wins 1 time
// in 300 on an idle machine, 1 in 4 after a quarter-millisecond stall, and
// every time from one millisecond. So it was rare here and common on a loaded
// runner. It failed a push check on a copy control's test, and holding the
// thread at the hand-back failed three more every time
// (test/profile/profileSection.test.tsx and the two focus tests under
// test/pipelines/). None of them was wrong about the product; the order they
// resumed in was this harness's.
//
// One real `setImmediate` after the wait is satisfied removes the race rather
// than narrowing it. By the time a wait can see a commit, React has queued
// that commit's effects; immediates run in the order they were queued; and
// Testing Library arms its timer only after this callback returns. It is the
// order the suite already ran in nearly every time, so no test that passed
// sees anything new. It stops the order being luck.
//
// The timer is the REAL one, captured here before any test can fake it. It
// has to wait in the queue React's scheduler uses, and the scheduler captured
// its own when it loaded.
//
// Testing Library's wrapper is called, not replaced: it is what turns act off
// for the wait and back on afterwards. Importing it here loads it for the
// test files that never render as well, about 70 ms of processor time each.
//
// What this does not promise is that work those effects START has finished.
// An effect that sets state or begins a read schedules another render, and a
// test that needs its result still waits for it by name.
//
// test/handBack.test.tsx forces the losing order and holds this.
const realSetImmediate = globalThis.setImmediate;
const testingLibraryHandBack = getConfig().asyncWrapper;
configure({
  asyncWrapper: (wait) => testingLibraryHandBack(async () => {
    const result = await wait();
    await new Promise<void>((resolve) => { realSetImmediate(resolve); });
    return result;
  }),
});

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

// ResizeObserver: layout-aware components observe their containers.
//
// The parameters are DECLARED even though jsdom lays nothing out and so can
// never fire the callback. A shim narrower than the API it stands in for is
// not privately incomplete: CodeQL resolves the global to this class, and so
// read a correct production call `new ResizeObserver(resize)` as passing a
// superfluous argument. A gap in a test double
// surfaced as an alert against application code that was right (memql#4777).
//
// `implements ResizeObserver` is the half tsc can check -- a renamed or
// missing method fails at the edit. It deliberately does NOT cover the
// constructor, because TypeScript accepts a signature with FEWER parameters
// anywhere one with more is wanted; that half is held by this comment and by
// CodeQL.
if (!(globalThis as { ResizeObserver?: unknown }).ResizeObserver) {
  class ResizeObserverShim implements ResizeObserver {
    constructor(_callback: ResizeObserverCallback) {}
    observe(_target: Element, _options?: ResizeObserverOptions) {}
    unobserve(_target: Element) {}
    disconnect() {}
  }
  (globalThis as { ResizeObserver?: unknown }).ResizeObserver = ResizeObserverShim;
}

// IntersectionObserver: a list that loads its next page on scroll watches a
// sentinel below its rows. The fake is driven by the test that cares
// (test/intersectionObserver.ts); every other suite renders without throwing.
installFakeIntersectionObserver();

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

// Blob.text(): jsdom implements Blob and File but not the async read methods
// on them, so `await file.text()` rejects with a TypeError that surfaces as a
// file picker which silently does nothing (epic memql#4745).
//
// A SHIM RATHER THAN A CODE CHANGE. `Blob.text()` has been in every browser
// this shell supports since Safari 14, and reaching for a FileReader in
// production to satisfy a test environment is the tail wagging the dog -- the
// same call the trainer's upload and the theme installer both make would then
// be written the long way in one of them and not the other. This file is
// where the environment's gaps are filled.
if (!Blob.prototype.text) {
  Blob.prototype.text = function text(this: Blob): Promise<string> {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(String(reader.result ?? ""));
      reader.onerror = () => reject(reader.error ?? new Error("read failed"));
      reader.readAsText(this);
    });
  };
}
