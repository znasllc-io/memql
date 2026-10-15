import { act, cleanup, fireEvent, render } from "@testing-library/react";
import { useLayoutEffect } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AutoRefresh, AUTO_REFRESH_MS } from "../../src/kit/AutoRefresh";

beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(new Date("2026-10-09T12:00:00Z")); });
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.useRealTimers(); });
const advance = async (ms = AUTO_REFRESH_MS) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });

describe("automatic background reads", () => {
  it.each([
    { name: "a read settling", beforeBusy: true, afterBusy: false, beforeEnabled: true, afterEnabled: true, calls: 1 },
    { name: "a read becoming busy", beforeBusy: false, afterBusy: true, beforeEnabled: true, afterEnabled: true, calls: 0 },
    { name: "refresh being enabled", beforeBusy: false, afterBusy: false, beforeEnabled: false, afterEnabled: true, calls: 1 },
    { name: "refresh being disabled", beforeBusy: false, afterBusy: false, beforeEnabled: true, afterEnabled: false, calls: 0 },
    { name: "the read callback changing", beforeBusy: false, afterBusy: false, beforeEnabled: true, afterEnabled: true, calls: 1 },
  ])("uses committed state for focus during $name", async ({ beforeBusy, afterBusy, beforeEnabled, afterEnabled, calls }) => {
    const first = vi.fn(); const next = vi.fn();
    function FocusOnCommit({ busy, enabled, onRefresh }: { busy: boolean; enabled: boolean; onRefresh: () => void }) {
      // A window can regain focus after the DOM commits but before passive
      // effects run. Observe that boundary without relying on scheduler timing.
      useLayoutEffect(() => { window.dispatchEvent(new Event("focus")); });
      return <AutoRefresh busy={busy} enabled={enabled} onRefresh={onRefresh} />;
    }
    const view = render(<FocusOnCommit busy={beforeBusy} enabled={beforeEnabled} onRefresh={first} />);
    await act(async () => { view.rerender(<FocusOnCommit busy={afterBusy} enabled={afterEnabled} onRefresh={next} />); });
    expect(first).not.toHaveBeenCalled();
    expect(next).toHaveBeenCalledTimes(calls);
  });
  it("does not duplicate the owner's initial read, follows the latest callback, and disposes all timers", async () => {
    const first = vi.fn(); const next = vi.fn();
    const view = render(<AutoRefresh onRefresh={first} />);
    expect(first).not.toHaveBeenCalled();
    await advance(); expect(first).toHaveBeenCalledOnce();
    view.rerender(<AutoRefresh onRefresh={next} />);
    await advance(); expect(next).toHaveBeenCalledOnce();
    view.unmount(); await advance(); fireEvent.focus(window);
    expect(next).toHaveBeenCalledOnce(); expect(vi.getTimerCount()).toBe(0);
  });
  it("pauses hidden browser tabs, parked panes, offline and busy reads, then wakes once on return", async () => {
    const read = vi.fn();
    const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    const online = vi.spyOn(navigator, "onLine", "get").mockReturnValue(true);
    const view = render(<div hidden><AutoRefresh onRefresh={read} /></div>);
    await advance(); expect(read).not.toHaveBeenCalled();
    visibility.mockReturnValue("visible"); await advance(); expect(read).not.toHaveBeenCalled();
    view.rerender(<div><AutoRefresh onRefresh={read} busy /></div>);
    await advance(); expect(read).not.toHaveBeenCalled();
    view.rerender(<div><AutoRefresh onRefresh={read} /></div>);
    online.mockReturnValue(false); await advance(); expect(read).not.toHaveBeenCalled();
    online.mockReturnValue(true);
    await act(async () => { fireEvent.online(window); fireEvent.focus(window); });
    expect(read).toHaveBeenCalledOnce();
  });
  it("never overlaps a slow read, retries rejection, and respects disabled terminal records", async () => {
    let reject!: (error: Error) => void;
    const read = vi.fn(() => new Promise<void>((_, fail) => { reject = fail; }));
    const view = render(<AutoRefresh onRefresh={read} />);
    await advance(); await advance(3 * AUTO_REFRESH_MS); fireEvent.focus(window);
    expect(read).toHaveBeenCalledOnce();
    await act(async () => { reject(new Error("offline")); });
    await advance(); expect(read).toHaveBeenCalledTimes(2);
    await act(async () => { reject(new Error("offline")); });
    view.rerender(<AutoRefresh onRefresh={read} enabled={false} />);
    await advance(); expect(read).toHaveBeenCalledTimes(2);
  });
});
