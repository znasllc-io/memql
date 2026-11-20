import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  ask,
  holdOf,
  NO_OPENS,
  READY_MS,
  settle,
  useDeskFileStates,
  type Hold,
  type Wait,
} from "../../src/items/deskFileState";

// No dot on a desk file (José, Nov 19 call; Jesús picked the states Nov 20
// 2026): uploading or offline is dim and does not open; opening an uploading
// file waits and it opens by itself once it lands; opening an offline
// computer's file is refused and never remembered (Jesús, Nov 20); a failed upload drops the wait; a finished
// upload nobody waited on says "Ready" for about three seconds.

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
  it("opens a file opened while uploading once the upload lands, with no Ready", () => {
    const d = desk({ a: "uploading" });
    d.ask("a", "open");
    expect(d.release).not.toHaveBeenCalled();
    expect(d.line("a")).toBe("opening");

    d.holds({ a: null });
    expect(d.release).toHaveBeenCalledExactlyOnceWith("a", "open");
    expect(d.line("a")).toBeNull();
  });

  it("drops the waiting open when the upload fails, and a retry that lands just says Ready", () => {
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

  it("says Ready for three seconds when nobody tried to open it, then nothing", () => {
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

  it("drops a waiting open if the file's computer goes offline before it can open", () => {
    const d = desk({ a: "uploading" });
    d.ask("a", "open");
    d.holds({ a: "offline" });
    d.holds({ a: null });
    expect(d.release).not.toHaveBeenCalled();
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

  it("forgets a file that left the desk while it waited", () => {
    const d = desk({ a: "uploading" });
    d.ask("a", "open");
    d.holds({});
    d.holds({ a: null });
    expect(d.release).not.toHaveBeenCalled();
  });
});

describe("settle and ask, without React", () => {
  it("leave the state untouched when nothing moved, so the desk does not redraw", () => {
    const h = holds({ a: "uploading" });
    expect(settle(NO_OPENS, h, h).opens).toBe(NO_OPENS);
    expect(ask(NO_OPENS, "a", "failed", "open").opens).toBe(NO_OPENS);
    expect(ask(NO_OPENS, "a", "offline", "open")).toEqual({ opens: NO_OPENS, now: false });
  });
});
