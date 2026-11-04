import { useEffect, useRef } from "react";

// THE BROWSER'S BACK DOES WHAT THE TRAIL ROW'S BACK DOES.
//
// A window whose app opts in (`OsAppManifest.browserHistory`) holds one entry
// of the browser's history for every step its trail row can go back. Going
// deeper pushes an entry; going back inside the window gives entries back, so
// the browser's Back never has to be pressed twice for one step; and the
// browser's Back calls the row's own Back, so the two can never disagree about
// where back is.
//
// ===========================================================================
// ONE STACK FOR THE WHOLE TAB
// ===========================================================================
// The browser keeps one history and two windows can each hold entries in it,
// so the entries are kept here, in one stack that records whose each one is.
// A Back reaches the window that pushed the entry it took. An entry a window
// no longer needs but cannot give back -- another window's entry sits above it,
// and the browser only gives back from the top -- is marked SPENT, and a Back
// that lands on one passes straight through it.
//
// THE PAGE DECIDES; THIS ONLY FOLLOWS. Nothing here knows what a page is. A
// window reports how many steps back its trail has (`sync`), and a Back that
// the page refuses -- a code shown once, waiting for Done -- leaves the trail
// where it was, so the entry the browser took is put back: the browser's Back
// then does what the row's own Back does at that moment, which is nothing.
//
// FORWARD IS NOT OFFERED. An entry given back holds no page to reopen, so a
// Forward into one is undone.
//
// Entries carry this page load's own mark, so an entry left by an earlier load
// of the tab (a reload keeps history) is never mistaken for one of these.

const KEY = "memqlOsTrail";

/** How long a page has to answer a Back before the Back counts as refused. */
const REFUSED_AFTER_MS = 400;
/** How long a traversal of ours may take to land before it counts as lost. */
const LOST_AFTER_MS = 2000;

export interface BrowserHistoryHost {
  history: {
    readonly state: unknown;
    pushState(data: unknown, unused: string): void;
    go(delta: number): void;
  };
  addEventListener(type: "popstate", listener: (event: PopStateEvent) => void): void;
  removeEventListener(type: "popstate", listener: (event: PopStateEvent) => void): void;
  setTimeout(handler: () => void, ms: number): unknown;
  clearTimeout(handle: unknown): void;
}

/** One window's claim on the browser's history. */
export interface HistoryLane {
  /** How many steps back this window's trail has now. 0 gives every entry back. */
  sync(depth: number): void;
  /** The window is gone: everything it held is given back. */
  release(): void;
}

export interface BrowserHistory {
  lane(onBack: () => void): HistoryLane;
  /** Stops listening. Entries already pushed stay in the browser's history. */
  dispose(): void;
}

interface Entry {
  lane: number;
  spent: boolean;
}

interface LaneState {
  onBack: () => void;
  /** The depth the window last reported. */
  depth: number;
  /** Entries the window wants held: its depth, or less while a Back is being answered. */
  want: number;
  /** Backs still owed after the first, when the browser went back several steps at once. */
  owed: number;
  /** Set while a Back is being answered, so a page that refuses it can be told apart. */
  answering: unknown;
}

const isObject = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

export function createBrowserHistory(host: BrowserHistoryHost): BrowserHistory {
  const session = `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 8)}`;
  const entries: Entry[] = [];
  const lanes = new Map<number, LaneState>();
  let nextLane = 1;
  // The index a traversal of ours will land on, while it is in flight.
  let landing: number | null = null;
  let landingTimer: unknown = null;

  const indexOf = (state: unknown): number => {
    const mark = isObject(state) ? state[KEY] : undefined;
    if (!isObject(mark) || mark.s !== session || typeof mark.i !== "number" || mark.i < 0) return 0;
    return mark.i;
  };

  const held = (id: number) => entries.reduce((n, entry) => (entry.lane === id && !entry.spent ? n + 1 : n), 0);

  function travel(delta: number, landsAt: number) {
    landing = landsAt;
    if (landingTimer !== null) host.clearTimeout(landingTimer);
    // A traversal that never lands must not hold every later step hostage.
    landingTimer = host.setTimeout(() => {
      landingTimer = null;
      landing = null;
      reconcile();
    }, LOST_AFTER_MS);
    host.history.go(delta);
  }

  function landed() {
    landing = null;
    if (landingTimer !== null) host.clearTimeout(landingTimer);
    landingTimer = null;
  }

  function reconcile() {
    if (landing !== null) return;
    // 1. Spend what each window no longer wants, newest first.
    for (const [id, lane] of lanes) {
      let excess = held(id) - lane.want;
      for (let i = entries.length - 1; i >= 0 && excess > 0; i--) {
        const entry = entries[i];
        if (entry !== undefined && entry.lane === id && !entry.spent) {
          entry.spent = true;
          excess--;
        }
      }
    }
    // 2. Give back what is spent at the top: the browser only gives back from there.
    let spent = 0;
    while (entries.at(-1)?.spent === true) {
      entries.pop();
      spent++;
    }
    if (spent > 0) {
      travel(-spent, entries.length);
      return;
    }
    // 3. Hold what each window wants and does not hold yet.
    for (const [id, lane] of lanes) {
      if (lane.owed > 0) continue;
      for (let have = held(id); have < lane.want; have++) {
        entries.push({ lane: id, spent: false });
        const prior = host.history.state;
        host.history.pushState({ ...(isObject(prior) ? prior : {}), [KEY]: { s: session, i: entries.length } }, "");
      }
    }
  }

  function answer(lane: LaneState) {
    // The page is asked to go back. If its trail has not moved by the time
    // it would have, it refused, and what the browser took is put back.
    const token = {};
    lane.answering = token;
    host.setTimeout(() => {
      if (lane.answering !== token) return;
      lane.answering = null;
      lane.owed = 0;
      lane.want = lane.depth;
      reconcile();
    }, REFUSED_AFTER_MS);
    lane.onBack();
  }

  function onPop(event: PopStateEvent) {
    const at = indexOf(event.state);
    if (landing !== null) {
      if (at === landing) {
        landed();
        reconcile();
        return;
      }
      // The person moved while ours was in flight: theirs is what happened.
      landed();
    }
    if (at > entries.length) {
      // FORWARD, into an entry already given back: there is no page there.
      travel(entries.length - at, entries.length);
      return;
    }
    const backs = new Map<number, number>();
    while (entries.length > at) {
      const entry = entries.pop() as Entry;
      if (!entry.spent) backs.set(entry.lane, (backs.get(entry.lane) ?? 0) + 1);
    }
    for (const [id, steps] of backs) {
      const lane = lanes.get(id);
      if (!lane) continue;
      lane.want = held(id);
      lane.owed = steps - 1;
      answer(lane);
    }
    reconcile();
  }

  host.addEventListener("popstate", onPop);

  return {
    lane(onBack) {
      const id = nextLane++;
      const state: LaneState = { onBack, depth: 0, want: 0, owed: 0, answering: null };
      lanes.set(id, state);
      return {
        sync(depth) {
          const next = Math.max(0, Math.floor(depth));
          const moved = next !== state.depth;
          const wentBack = next < state.depth;
          state.depth = next;
          if (state.answering !== null) {
            if (!moved) return;
            state.answering = null;
            if (wentBack && state.owed > 0) {
              state.owed--;
              answer(state);
              return;
            }
            state.owed = 0;
          }
          state.want = next;
          reconcile();
        },
        release() {
          if (!lanes.delete(id)) return;
          state.answering = null;
          for (const entry of entries) if (entry.lane === id) entry.spent = true;
          reconcile();
        },
      };
    },
    dispose() {
      host.removeEventListener("popstate", onPop);
      if (landingTimer !== null) host.clearTimeout(landingTimer);
    },
  };
}

let shared: BrowserHistory | null = null;

function sharedHistory(): BrowserHistory | null {
  if (shared !== null) return shared;
  if (typeof window === "undefined" || typeof window.history?.pushState !== "function") return null;
  shared = createBrowserHistory({
    history: window.history,
    addEventListener: (type, listener) => window.addEventListener(type, listener),
    removeEventListener: (type, listener) => window.removeEventListener(type, listener),
    setTimeout: (handler, ms) => window.setTimeout(handler, ms),
    clearTimeout: (handle) => window.clearTimeout(handle as number),
  });
  return shared;
}

/**
 * Hold `depth` browser history entries for one window while `enabled`, and
 * answer the browser's Back with `onBack`. `onBack` is read when it is called,
 * so it is never a stale closure.
 */
export function useBrowserHistory(enabled: boolean, depth: number, onBack: () => void): void {
  const back = useRef(onBack);
  back.current = onBack;
  const lane = useRef<HistoryLane | null>(null);
  useEffect(() => {
    if (!enabled) return;
    const history = sharedHistory();
    if (history === null) return;
    const claimed = history.lane(() => back.current());
    lane.current = claimed;
    return () => {
      lane.current = null;
      claimed.release();
    };
  }, [enabled]);
  useEffect(() => {
    lane.current?.sync(enabled ? depth : 0);
  }, [enabled, depth]);
}
