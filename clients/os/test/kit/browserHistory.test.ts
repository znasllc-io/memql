import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createBrowserHistory, type BrowserHistoryHost } from "../../src/kit/browserHistory";

// THE BROWSER'S BACK DOES WHAT THE TRAIL ROW'S BACK DOES (kit/browserHistory).
// A fake browser keeps a real history stack: pushState truncates what is
// ahead, and a traversal lands later, the way a browser's does, firing
// popstate with the state of the entry it landed on.

class FakeBrowser {
  stack: unknown[] = [{ app: "base" }];
  index = 0;
  traversals: number[] = [];
  private listeners = new Set<(event: PopStateEvent) => void>();
  get state(): unknown {
    return this.stack[this.index];
  }
  pushState = (data: unknown): void => {
    this.stack = this.stack.slice(0, this.index + 1);
    this.stack.push(structuredClone(data));
    this.index++;
  };
  go = (delta: number): void => {
    this.traversals.push(delta);
    void Promise.resolve().then(() => {
      const next = Math.min(Math.max(this.index + delta, 0), this.stack.length - 1);
      if (next === this.index) return;
      this.index = next;
      const event = { state: this.stack[next] } as PopStateEvent;
      for (const listener of [...this.listeners]) listener(event);
    });
  };
  /** The person presses the browser's Back. */
  back(steps = 1): void {
    this.go(-steps);
  }
  host(): BrowserHistoryHost {
    return {
      history: this,
      addEventListener: (_type, listener) => this.listeners.add(listener),
      removeEventListener: (_type, listener) => this.listeners.delete(listener),
      setTimeout: (handler, ms) => setTimeout(handler, ms),
      clearTimeout: (handle) => clearTimeout(handle as ReturnType<typeof setTimeout>),
    };
  }
}

const settle = async () => {
  for (let i = 0; i < 6; i++) await Promise.resolve();
};

beforeEach(() => {
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
});

describe("browser history follows the trail row", () => {
  it("holds one entry per step back, and the browser's Back calls the row's Back", async () => {
    const browser = new FakeBrowser();
    const onBack = vi.fn();
    const lane = createBrowserHistory(browser.host()).lane(onBack);
    lane.sync(1);
    lane.sync(2);
    expect(browser.index).toBe(2);

    browser.back();
    await settle();
    expect(onBack).toHaveBeenCalledOnce();
    // The page answers: its trail is one step shorter, and nothing is pushed again.
    lane.sync(1);
    await settle();
    vi.advanceTimersByTime(1000);
    await settle();
    expect(browser.index).toBe(1);
    expect(onBack).toHaveBeenCalledOnce();
  });

  it("gives an entry back when the window goes back by itself, so Back is never pressed twice", async () => {
    const browser = new FakeBrowser();
    const onBack = vi.fn();
    const lane = createBrowserHistory(browser.host()).lane(onBack);
    lane.sync(2);
    lane.sync(1);
    await settle();
    expect(browser.index).toBe(1);
    lane.sync(0);
    await settle();
    expect(browser.index).toBe(0);
    expect(onBack).not.toHaveBeenCalled();
  });

  it("gives several steps back in one traversal", async () => {
    const browser = new FakeBrowser();
    const lane = createBrowserHistory(browser.host()).lane(vi.fn());
    lane.sync(3);
    lane.sync(0);
    await settle();
    expect(browser.traversals).toEqual([-3]);
    expect(browser.index).toBe(0);
  });

  it("puts the entry back when the page refuses the Back", async () => {
    const browser = new FakeBrowser();
    const onBack = vi.fn();
    const lane = createBrowserHistory(browser.host()).lane(onBack);
    lane.sync(2);
    browser.back();
    await settle();
    expect(onBack).toHaveBeenCalledOnce();
    expect(browser.index).toBe(1);
    // A code shown once holds Back until Done: the trail never moves.
    vi.advanceTimersByTime(1000);
    await settle();
    expect(browser.index).toBe(2);
    // ...and once it does, the step is given back as usual.
    lane.sync(1);
    await settle();
    expect(browser.index).toBe(1);
  });

  it("answers several steps back one at a time, as the page takes each", async () => {
    const browser = new FakeBrowser();
    const onBack = vi.fn();
    const lane = createBrowserHistory(browser.host()).lane(onBack);
    lane.sync(3);
    browser.back(2);
    await settle();
    expect(onBack).toHaveBeenCalledTimes(1);
    lane.sync(2);
    await settle();
    expect(onBack).toHaveBeenCalledTimes(2);
    lane.sync(1);
    await settle();
    vi.advanceTimersByTime(1000);
    await settle();
    expect(onBack).toHaveBeenCalledTimes(2);
    expect(browser.index).toBe(1);
  });

  it("sends a Back to the window that pushed the entry it took", async () => {
    const browser = new FakeBrowser();
    const history = createBrowserHistory(browser.host());
    const onA = vi.fn();
    const onB = vi.fn();
    const a = history.lane(onA);
    const b = history.lane(onB);
    a.sync(1);
    b.sync(1);
    browser.back();
    await settle();
    expect(onB).toHaveBeenCalledOnce();
    expect(onA).not.toHaveBeenCalled();
  });

  it("passes through an entry its window spent under another window's", async () => {
    const browser = new FakeBrowser();
    const history = createBrowserHistory(browser.host());
    const onA = vi.fn();
    const onB = vi.fn();
    const a = history.lane(onA);
    const b = history.lane(onB);
    a.sync(1);
    b.sync(1);
    // A goes back by itself; its entry is under B's, so it cannot be given back yet.
    a.sync(0);
    await settle();
    expect(browser.index).toBe(2);
    // One Back answers B, and passes through A's spent entry on the way.
    browser.back();
    await settle();
    b.sync(0);
    await settle();
    expect(onB).toHaveBeenCalledOnce();
    expect(onA).not.toHaveBeenCalled();
    expect(browser.index).toBe(0);
  });

  it("undoes a Forward into an entry already given back", async () => {
    const browser = new FakeBrowser();
    const onBack = vi.fn();
    const lane = createBrowserHistory(browser.host()).lane(onBack);
    lane.sync(2);
    lane.sync(1);
    await settle();
    browser.go(1);
    await settle();
    await settle();
    expect(browser.index).toBe(1);
    expect(onBack).not.toHaveBeenCalled();
  });

  it("does not take an entry from an earlier load of the tab for one of its own", async () => {
    const browser = new FakeBrowser();
    browser.pushState({ memqlOsTrail: { s: "an-earlier-load", i: 1 } });
    const onBack = vi.fn();
    const lane = createBrowserHistory(browser.host()).lane(onBack);
    lane.sync(1);
    expect(browser.index).toBe(2);
    browser.back();
    await settle();
    expect(onBack).toHaveBeenCalledOnce();
    lane.sync(0);
    await settle();
    expect(browser.index).toBe(1);
  });

  it("keeps the state other code put on the entry it pushes from", () => {
    const browser = new FakeBrowser();
    browser.stack[0] = { run: "r-1" };
    createBrowserHistory(browser.host()).lane(vi.fn()).sync(1);
    expect(browser.state).toMatchObject({ run: "r-1" });
  });

  it("gives back everything a window held when it goes", async () => {
    const browser = new FakeBrowser();
    const lane = createBrowserHistory(browser.host()).lane(vi.fn());
    lane.sync(2);
    lane.release();
    await settle();
    expect(browser.index).toBe(0);
  });
});
