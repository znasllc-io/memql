import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  ask,
  holdOf,
  NO_OPENS,
  GLOW_MS,
  READY_MS,
  settle,
  useArrivalGlow,
  useDeskFileStates,
  type Hold,
  type Wait,
} from "../../src/items/deskFileState";

// No dot on a desk file (José, Nov 19 call; Jesús picked the states Nov 20
// 2026): uploading or offline is dim and does not open. Opening it does
// nothing and is never remembered, so nothing opens by itself later (Jesús,
// Nov 20 evening: a later open is outside the click and Safari blocks it). A
// file dropped into a folder while uploading still goes in once it lands; a
// failed upload drops that move; an upload that lands on the desk says
// "Ready" for about three seconds.

const holds = (entries: Record<string, Hold>) => new Map(Object.entries(entries));

function desk(initial: Record<string, Hold>) {
  const release = vi.fn<(id: string, wait: Wait) => void>();
  const hook = renderHook(({ h }) => useDeskFileStates(h, release), { initialProps: { h: holds(initial) } });
  return {
    release,
    line: (id: string) => hook.result.current.line(id),
    ask: (id: string, wait: Wait) => act(() => hook.result.current.ask(id, wait)),
    holds: (next: Record<string, Hold>) => hook.rerender({ h: holds(next) }),
  };
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("holdOf -- what stops a desk file opening", () => {
  const made = { source: "computer_use", producedByWorkerId: "w-1" };

  it("is the upload while there is one", () => {
    expect(holdOf({ source: "uploaded", uploadState: "uploading" }, null)).toBe("uploading");
    expect(holdOf({ source: "uploaded", uploadState: "failed" }, null)).toBe("failed");
  });

  it("is the computer when the machine that made it is known and offline", () => {
    expect(holdOf(made, { name: "Studio Mac", online: false })).toBe("offline");
    expect(holdOf(made, { name: "Studio Mac", online: true })).toBeNull();
  });

  it("holds nothing back for a machine nobody has heard of yet, or a file made here", () => {
    expect(holdOf(made, null)).toBeNull();
    expect(holdOf({ source: "uploaded" }, null)).toBeNull();
  });
});

describe("the desk's file states", () => {
  it("does nothing when a file is opened while uploading, and nothing opens when it lands", () => {
    const d = desk({ a: "uploading" });
    d.ask("a", "open");
    d.ask("a", "open");
    expect(d.release).not.toHaveBeenCalled();
    expect(d.line("a")).toBeNull();

    d.holds({ a: null });
    expect(d.release).not.toHaveBeenCalled();
    // Ready says it can be opened now, and opening it now opens it.
    expect(d.line("a")).toBe("ready");
    d.ask("a", "open");
    expect(d.release).toHaveBeenCalledExactlyOnceWith("a", "open");
  });

  it("offers the retry line when the upload fails, and a retry that lands says Ready", () => {
    const d = desk({ a: "uploading" });
    d.ask("a", "open");
    d.holds({ a: "failed" });
    expect(d.line("a")).toBe("failed");

    d.holds({ a: "uploading" });
    expect(d.line("a")).toBeNull();
    d.holds({ a: null });
    expect(d.release).not.toHaveBeenCalled();
    expect(d.line("a")).toBe("ready");
  });

  it("says Ready for three seconds after an upload lands, then nothing", () => {
    const d = desk({ a: "uploading", b: null });
    d.holds({ a: null, b: null });
    expect(d.line("a")).toBe("ready");
    // A file that was never uploading here never says Ready.
    expect(d.line("b")).toBeNull();

    act(() => vi.advanceTimersByTime(READY_MS - 1));
    expect(d.line("a")).toBe("ready");
    act(() => vi.advanceTimersByTime(1));
    expect(d.line("a")).toBeNull();
  });

  it("never opens a file opened while its computer was offline, even once the computer is back", () => {
    const d = desk({ a: "offline" });
    expect(d.line("a")).toBe("offline");
    d.ask("a", "open");
    d.ask("a", "open");
    expect(d.release).not.toHaveBeenCalled();
    expect(d.line("a")).toBe("offline");

    d.holds({ a: null });
    act(() => vi.advanceTimersByTime(READY_MS * 10));
    expect(d.release).not.toHaveBeenCalled();
    expect(d.line("a")).toBeNull();
    // Opening it now that the computer is back works as usual.
    d.ask("a", "open");
    expect(d.release).toHaveBeenCalledExactlyOnceWith("a", "open");
  });

  it("opens at once when nothing holds it, and never for a failed upload", () => {
    const d = desk({ a: null, f: "failed" });
    d.ask("a", "open");
    d.ask("f", "open");
    expect(d.release).toHaveBeenCalledExactlyOnceWith("a", "open");
  });

  it("files an uploading file into a folder once it lands; offline does not hold a move", () => {
    const d = desk({ a: "uploading", m: "offline" });
    d.ask("m", { folderId: "f-2" });
    expect(d.release).toHaveBeenLastCalledWith("m", { folderId: "f-2" });

    d.ask("a", { folderId: "f-1" });
    expect(d.release).toHaveBeenCalledTimes(1);
    d.holds({ a: null, m: "offline" });
    expect(d.release).toHaveBeenLastCalledWith("a", { folderId: "f-1" });
    // Nothing says Ready for a file on its way into a folder.
    expect(d.line("a")).toBeNull();
  });

  it("drops a folder move when the upload fails or the file leaves the desk", () => {
    const d = desk({ a: "uploading", b: "uploading" });
    d.ask("a", { folderId: "f-1" });
    d.ask("b", { folderId: "f-1" });
    // a fails; b leaves the desk.
    d.holds({ a: "failed" });
    d.holds({ a: null, b: null });
    expect(d.release).not.toHaveBeenCalled();
  });
});

describe("settle and ask, without React", () => {
  it("leave the state untouched when nothing moved, so the desk does not redraw", () => {
    const h = holds({ a: "uploading" });
    expect(settle(NO_OPENS, h, h).opens).toBe(NO_OPENS);
    expect(ask(NO_OPENS, "a", "failed", "open").opens).toBe(NO_OPENS);
    expect(ask(NO_OPENS, "a", "offline", "open")).toEqual({ opens: NO_OPENS, now: false });
    expect(ask(NO_OPENS, "a", "uploading", "open")).toEqual({ opens: NO_OPENS, now: false });
  });
});

describe("a new arrival glows (Jesus's pick 6B, Nov 22 2026)", () => {
  function arrivals(initial: Record<string, Hold>) {
    const hook = renderHook(({ h }) => useArrivalGlow(h), { initialProps: { h: holds(initial) } });
    return {
      glowing: (id: string) => hook.result.current.glowing(id),
      clear: (id: string) => act(() => hook.result.current.clear(id)),
      holds: (next: Record<string, Hold>) => hook.rerender({ h: holds(next) }),
    };
  }

  it("glows a file that lands, not the icons already there", () => {
    const d = arrivals({ old: null });
    expect(d.glowing("old")).toBe(false);
    d.holds({ old: null, fresh: null });
    expect(d.glowing("fresh")).toBe(true);
    expect(d.glowing("old")).toBe(false);
  });

  it("files on the hydrate document are the baseline, a later file glows", () => {
    const hook = renderHook(({ h, r }) => useArrivalGlow(h, r), { initialProps: { h: holds({}), r: false } });
    hook.rerender({ h: holds({ old: null }), r: true });
    expect(hook.result.current.glowing("old")).toBe(false);
    hook.rerender({ h: holds({ old: null, fresh: null }), r: true });
    expect(hook.result.current.glowing("fresh")).toBe(true);
    expect(hook.result.current.glowing("old")).toBe(false);
  });

  it("a second hydrate (local store swapped for the graph one) is a baseline too", () => {
    const hook = renderHook(({ h, n }) => useArrivalGlow(h, true, n), { initialProps: { h: holds({ local: null }), n: 0 } });
    hook.rerender({ h: holds({ local: null, synced: null }), n: 1 });
    expect(hook.result.current.glowing("synced")).toBe(false);
    hook.rerender({ h: holds({ local: null, synced: null, fresh: null }), n: 1 });
    expect(hook.result.current.glowing("fresh")).toBe(true);
  });

  it("an upload glows once it has landed, not while dim", () => {
    const d = arrivals({});
    d.holds({ up: "uploading" });
    expect(d.glowing("up")).toBe(false);
    d.holds({ up: null });
    expect(d.glowing("up")).toBe(true);
  });

  it("opening it clears the glow", () => {
    const d = arrivals({});
    d.holds({ a: null });
    expect(d.glowing("a")).toBe(true);
    d.clear("a");
    expect(d.glowing("a")).toBe(false);
  });

  it("fades out after GLOW_MS", () => {
    const d = arrivals({});
    d.holds({ a: null });
    act(() => void vi.advanceTimersByTime(GLOW_MS - 1));
    expect(d.glowing("a")).toBe(true);
    act(() => void vi.advanceTimersByTime(1));
    expect(d.glowing("a")).toBe(false);
  });
});
