import { act, cleanup, fireEvent, render, renderHook, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

vi.mock("../../src/auth/context", () => ({
  useAuthSource: () => ({ bearer: async () => "test-token", refresh: async () => {} }),
}));

const { EmailApp } = await import("../../src/apps/email/EmailApp");
const { LocalEmailSettingsStore } = await import("../../src/apps/email/settings");
const { EMAIL_REFRESH_MS, useReading } = await import("../../src/apps/email/useEmail");
const { campaignRow, deliveryRow, fakeConnection, withSession } = await import("./harness");

afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); });
const settle = async () => { await act(async () => {}); };
const advance = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });

// ---------------------------------------------------------------------------
// The reading itself: it keeps its answer, and it has no clock
// ---------------------------------------------------------------------------

describe("a reading", () => {
  it("a reading keeps its last answer and aborts a stale read", async () => {
    const read = vi.fn(async (_signal: AbortSignal) => 1);
    const { result } = renderHook(() => useReading(0, read, [read]));
    await settle();
    expect(result.current).toMatchObject({ value: 1, state: "ready", error: "" });

    // A read still on its way leaves the last answer where it is...
    let stale!: AbortSignal;
    let finishStale!: (value: number) => void;
    read.mockImplementationOnce((signal) => {
      stale = signal;
      return new Promise<number>((done) => { finishStale = done; });
    });
    act(() => { void result.current.reload(); });
    expect(result.current).toMatchObject({ value: 1, state: "ready" });

    // ...and is stale the moment another one starts: aborted, and a late
    // answer from it is not taken for the current one.
    read.mockResolvedValueOnce(3);
    await act(async () => { await result.current.reload(); });
    expect(stale.aborted).toBe(true);
    expect(result.current.value).toBe(3);
    await act(async () => finishStale(99));
    expect(result.current.value).toBe(3);

    // A failure keeps the last good answer and says why; the next success clears it.
    read.mockRejectedValueOnce(new Error("Connection interrupted"));
    await act(async () => { await result.current.reload(); });
    expect(result.current).toMatchObject({ value: 3, state: "error", error: "Connection interrupted" });
    expect(result.current.readAt).not.toBe("");
    read.mockResolvedValueOnce(4);
    await act(async () => { await result.current.reload(); });
    expect(result.current).toMatchObject({ value: 4, state: "ready", error: "" });
  });

  it("aborts the old read and ignores its late answer when what is read changes", async () => {
    let finish!: (value: number) => void;
    const first = vi.fn((_signal: AbortSignal) => new Promise<number>((done) => { finish = done; }));
    const next = vi.fn(async (_signal: AbortSignal) => 7);
    const { result, rerender } = renderHook(({ read }) => useReading(0, read, [read]), { initialProps: { read: first } });
    rerender({ read: next });
    await settle();
    expect(first.mock.calls[0]![0].aborted).toBe(true);
    expect(result.current.value).toBe(7);
    await act(async () => finish(99));
    expect(result.current.value).toBe(7);
  });

  it("reads once on its own and again only when asked", async () => {
    vi.useFakeTimers();
    const read = vi.fn(async (_signal: AbortSignal) => 1);
    const { result } = renderHook(() => useReading(0, read, [read]));
    await advance(EMAIL_REFRESH_MS * 5);
    expect(read).toHaveBeenCalledTimes(1);
    await act(async () => { await result.current.reload(); });
    expect(read).toHaveBeenCalledTimes(2);
  });

  it("settles a reload when its read settles, so a refresher waits instead of piling on", async () => {
    let finish!: (value: number) => void;
    const read = vi.fn((_signal: AbortSignal) => new Promise<number>((done) => { finish = done; }));
    const { result } = renderHook(() => useReading(0, read, [read]));
    let settled = false;
    act(() => { void result.current.reload().then(() => { settled = true; }); });
    await settle();
    expect(settled).toBe(false);
    await act(async () => finish(5));
    expect(settled).toBe(true);
    expect(result.current.value).toBe(5);
  });

  it("settles a reload at once when there is nothing to read, and when the window closes", async () => {
    const idle = renderHook(() => useReading(0, null, []));
    await act(async () => { await idle.result.current.reload(); });
    expect(idle.result.current.state).toBe("idle");

    const read = vi.fn((_signal: AbortSignal) => new Promise<number>(() => {}));
    const busy = renderHook(() => useReading(0, read, [read]));
    let settled = false;
    act(() => { void busy.result.current.reload().then(() => { settled = true; }); });
    busy.unmount();
    await settle();
    expect(settled).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// The shared refresh: a page refreshes while it is seen, and only then
// ---------------------------------------------------------------------------

describe("the shared refresh", () => {
  async function openCampaign() {
    const conn = fakeConnection({
      campaigns: [campaignRow({ id: "v1:campaigns:campaign:c1", recipientCount: 10, sentCount: 10 })],
      deliveriesForCampaign: [deliveryRow({ id: "v1:campaigns:delivery:d1" })],
    });
    h.connection = conn;
    const bag = new Map<string, string>();
    const store = new LocalEmailSettingsStore({ getItem: (k: string) => bag.get(k) ?? null, setItem: (k: string, v: string) => void bag.set(k, v) });
    const app = (section: string) =>
      withSession(<EmailApp sectionId={section} navigate={() => {}} askContext={() => {}} store={store} uploads={{ upload: vi.fn() }} />);
    const view = render(app("campaigns"));
    fireEvent.click(await screen.findByText("August update"));
    // Every reading on the page has answered: a refresher waits out a first
    // read, so arming one before then would measure nothing.
    await screen.findByText("dana@acme.com");
    await screen.findByText("No breakdown yet.");
    await screen.findByText(/Latest 50 · Updates automatically/);
    const reads = () => conn.query.deliveriesForCampaign.mock.calls.length;
    return { conn, reads, section: (next: string) => view.rerender(app(next)) };
  }

  it("a visible reading refreshes every five seconds and a hidden pane stops", async () => {
    const { reads, section } = await openCampaign();
    expect(reads()).toBe(1);

    // Coming back to the window wakes the page's refreshers, which then keep
    // time on the clock this test controls.
    vi.useFakeTimers();
    await act(async () => window.dispatchEvent(new Event("focus")));
    const armed = reads();
    expect(armed).toBe(2);
    await advance(EMAIL_REFRESH_MS);
    expect(reads()).toBe(armed + 1);
    await advance(EMAIL_REFRESH_MS);
    expect(reads()).toBe(armed + 2);

    // The person goes to another tab. The campaign page stays mounted behind
    // it -- and stops reading.
    section("overview");
    const left = reads();
    await advance(EMAIL_REFRESH_MS * 3);
    expect(reads()).toBe(left);
  });

  it("an offline window stops reading and wakes once online", async () => {
    const { reads } = await openCampaign();
    const online = vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
    vi.useFakeTimers();

    await act(async () => window.dispatchEvent(new Event("focus")));
    const offline = reads();
    await advance(EMAIL_REFRESH_MS * 3);
    expect(reads()).toBe(offline);

    online.mockReturnValue(true);
    await act(async () => window.dispatchEvent(new Event("online")));
    expect(reads()).toBe(offline + 1);
  });

  it("a hidden browser tab stops reading and reads once on return", async () => {
    const { reads } = await openCampaign();
    const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    vi.useFakeTimers();

    await act(async () => document.dispatchEvent(new Event("visibilitychange")));
    const away = reads();
    await advance(EMAIL_REFRESH_MS * 3);
    expect(reads()).toBe(away);

    visibility.mockReturnValue("visible");
    await act(async () => document.dispatchEvent(new Event("visibilitychange")));
    expect(reads()).toBe(away + 1);
  });
});
