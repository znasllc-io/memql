import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CAMPAIGN_REFRESH_MS, useReading } from "../../src/apps/campaigns/useCampaigns";

beforeEach(() => vi.useFakeTimers());
afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); });
const settle = async () => { await act(async () => {}); };
const tick = async () => { await act(async () => { await vi.advanceTimersByTimeAsync(CAMPAIGN_REFRESH_MS); }); };

describe("automatic campaign readings", () => {
  it("updates without a click, retains data through background reads, and retries a failure", async () => {
    const read = vi.fn(async (_signal: AbortSignal) => 1);
    const { result } = renderHook(() => useReading(0, read, [read], CAMPAIGN_REFRESH_MS));
    await settle();
    let resolve!: (value: number) => void;
    read.mockImplementationOnce(() => new Promise<number>(done => { resolve = done; }));
    await tick();
    expect(result.current.value).toBe(1);
    expect(result.current.state).toBe("ready");
    await tick();
    expect(read).toHaveBeenCalledTimes(2); // no overlapping reads
    await act(async () => resolve(2));
    read.mockRejectedValueOnce(new Error("Connection interrupted"));
    await tick();
    expect(result.current.value).toBe(2);
    expect(result.current.error).toBe("Connection interrupted");
    read.mockResolvedValue(3);
    await tick();
    expect(result.current.value).toBe(3);
    expect(result.current.error).toBe("");
  });

  it("pauses in hidden tabs, reads on return and stops on unmount", async () => {
    const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    const read = vi.fn(async (_signal: AbortSignal) => 1);
    const { unmount } = renderHook(() => useReading(0, read, [read], CAMPAIGN_REFRESH_MS));
    await settle();
    visibility.mockReturnValue("hidden");
    act(() => document.dispatchEvent(new Event("visibilitychange")));
    await tick();
    expect(read).toHaveBeenCalledTimes(1);
    visibility.mockReturnValue("visible");
    await act(async () => document.dispatchEvent(new Event("visibilitychange")));
    expect(read).toHaveBeenCalledTimes(2);
    unmount();
    expect(read.mock.calls[1]![0].aborted).toBe(true);
    await tick();
    expect(read).toHaveBeenCalledTimes(2);
  });

  it("aborts the old campaign read and ignores its late result after navigation", async () => {
    let resolve!: (value: number) => void;
    const first = vi.fn((_signal: AbortSignal) => new Promise<number>(done => { resolve = done; }));
    const next = vi.fn(async (_signal: AbortSignal) => 7);
    const { result, rerender } = renderHook(({ read }) => useReading(0, read, [read], CAMPAIGN_REFRESH_MS), { initialProps: { read: first } });
    rerender({ read: next });
    await settle();
    expect(first.mock.calls[0]![0].aborted).toBe(true);
    expect(result.current.value).toBe(7);
    await act(async () => resolve(99));
    expect(result.current.value).toBe(7);
  });
});
