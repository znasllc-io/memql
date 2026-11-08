import { StrictMode, type ReactNode } from "react";
import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Connection, ConnectionStatus, Event as GraphEvent } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
const { useLiveCollection } = await import("../../src/live/useLiveCollection");
const { disposeSessionCollections } = await import("../../src/live/sessionCollections");
const { LiveReadVisibility } = await import("../../src/live/useGraphRevision");
afterEach(() => { if (h.connection) disposeSessionCollections(h.connection as Connection); });

function connection() {
  const cycles = new Set<(cycle: number) => void>();
  const statuses = new Set<(event: { status: ConnectionStatus }) => void>();
  const deliveries = new Set<(event: GraphEvent) => void>();
  let status: ConnectionStatus = "connected";
  const conn = {
    get status() { return status; },
    subscriptions: {
      subscribeGraph: vi.fn(() => () => {}),
      onDelivery: vi.fn((handler: (event: GraphEvent) => void) => {
        deliveries.add(handler);
        return () => deliveries.delete(handler);
      }),
    },
    onConnectionCycle: (handler: (cycle: number) => void) => {
      cycles.add(handler);
      return () => cycles.delete(handler);
    },
    onStatusChange: (handler: (event: { status: ConnectionStatus }) => void) => {
      statuses.add(handler);
      handler({ status });
      return () => statuses.delete(handler);
    },
    setStatus(next: ConnectionStatus) {
      status = next;
      for (const handler of statuses) handler({ status });
    },
    reconnected() {
      conn.setStatus("connected");
      for (const handler of cycles) handler(1);
    },
    deliver(seq: number, gapBefore = false) {
      const event: GraphEvent = { subscriptionId: "unrelated", kind: "NODE_UPDATED", payload: null,
        payloadOmitted: true, timestamp: null, seq, gapBefore };
      for (const handler of deliveries) handler(event);
    },
    cycles, statuses, deliveries,
  };
  h.connection = conn as unknown as Connection;
  return conn;
}

describe("live collection reconnects", () => {
  it("repairs noncached readers on stream gaps without duplicating reconnect or resume recovery", async () => {
    const conn = connection();
    const seed = vi.fn().mockResolvedValue({ rows: [{ id: "initial" }], nextCursor: "" });
    const wrapper = ({ children }: { children: ReactNode }) => <StrictMode>{children}</StrictMode>;
    const { result, unmount } = renderHook(() => useLiveCollection("composed", () => ({ concept: "c", seed }), { cacheReleased: false }), { wrapper });
    await waitFor(() => expect(seed).toHaveBeenCalledTimes(1));
    seed.mockResolvedValue({ rows: [{ id: "repaired" }], nextCursor: "" });
    act(() => { conn.deliver(1); conn.deliver(3); });
    await waitFor(() => expect(result.current.snapshot.rows).toEqual([{ id: "repaired" }]));
    expect(seed).toHaveBeenCalledTimes(2);
    act(() => conn.deliver(4, true));
    await waitFor(() => expect(seed).toHaveBeenCalledTimes(3));
    expect(conn.subscriptions.onDelivery).toHaveBeenCalledTimes(1);
    expect(conn.cycles.size).toBe(1);

    const markDisconnected = vi.spyOn(result.current.source as unknown as { markDisconnected(): void }, "markDisconnected");
    act(() => conn.setStatus("reconnecting"));
    expect(markDisconnected).toHaveBeenCalledTimes(1);
    expect(result.current.snapshot.state).toBe("disconnected");
    act(() => conn.reconnected());
    await waitFor(() => expect(seed).toHaveBeenCalledTimes(4));
    await act(async () => {
      window.dispatchEvent(new Event("focus"));
      window.dispatchEvent(new Event("online"));
      window.dispatchEvent(new Event("pageshow"));
    });
    expect(seed).toHaveBeenCalledTimes(4);
    unmount();
    act(() => { conn.deliver(6, true); conn.reconnected(); });
    expect(seed).toHaveBeenCalledTimes(4);
    disposeSessionCollections(conn as unknown as Connection);
    expect(conn.cycles.size).toBe(0);
    expect(conn.statuses.size).toBe(0);
    expect(conn.deliveries.size).toBe(0);
  });

  it("classifies a transport-interrupted seed as disconnected rather than a failed read", async () => {
    const conn = connection();
    const seed = vi.fn().mockRejectedValueOnce(Object.assign(new Error("stream closed"), {reason:"transport"})).mockResolvedValueOnce({rows:[{id:"recovered"}],nextCursor:""});
    const {result} = renderHook(() => useLiveCollection("recovery", () => ({concept:"approval",seed})));
    await waitFor(() => expect(result.current.snapshot.state).toBe("disconnected"));
    expect(result.current.snapshot.error).toBe("");
    act(() => conn.reconnected());
    await waitFor(() => expect(result.current.snapshot.rows).toEqual([{id:"recovered"}]));
    expect(result.current.snapshot.state).toBe("live");
  });
  it("repairs a stream-closed seed and clears its stale error after subscription replay", async () => {
    const conn = connection();
    const seed = vi.fn()
      .mockRejectedValueOnce(new Error("stream closed"))
      .mockResolvedValueOnce({ rows: [{ id: "fresh", decision: "approved" }], nextCursor: "" });
    const { result, unmount } = renderHook(() => useLiveCollection("approvals", () => ({ concept: "approval", seed })));
    await waitFor(() => expect(result.current.snapshot.error).toBe("stream closed"));
    act(() => conn.setStatus("reconnecting"));
    expect(result.current.snapshot.state).toBe("disconnected");
    // Connected transport alone is too early: subscriptions must be replayed.
    act(() => conn.setStatus("connected"));
    expect(seed).toHaveBeenCalledTimes(1);
    act(() => conn.reconnected());
    await waitFor(() => expect(result.current.snapshot.rows).toEqual([{ id: "fresh", decision: "approved" }]));
    expect(result.current.snapshot.state).toBe("live");
    expect(result.current.snapshot.error).toBe("");
    expect(seed).toHaveBeenCalledTimes(2);
    unmount();
    disposeSessionCollections(conn as unknown as Connection);
    expect(conn.cycles.size).toBe(0);
    expect(conn.statuses.size).toBe(0);
    act(() => conn.reconnected());
    expect(seed).toHaveBeenCalledTimes(2);
  });

  it("keeps last-known rows and replaces them with changes missed while disconnected", async () => {
    const conn = connection();
    const seed = vi.fn()
      .mockResolvedValueOnce({ rows: [{ id: "old" }], nextCursor: "" })
      .mockResolvedValueOnce({ rows: [{ id: "new" }], nextCursor: "" });
    const { result } = renderHook(() => useLiveCollection("approvals", () => ({ concept: "approval", seed })));
    await waitFor(() => expect(result.current.snapshot.rows).toEqual([{ id: "old" }]));
    act(() => conn.setStatus("reconnecting"));
    expect(result.current.snapshot.rows).toEqual([{ id: "old" }]);
    expect(result.current.snapshot.state).toBe("disconnected");
    act(() => conn.reconnected());
    await waitFor(() => expect(result.current.snapshot.rows).toEqual([{ id: "new" }]));
  });

  it("does not double-read or leak reconnect listeners under StrictMode", async () => {
    const conn = connection();
    const seed = vi.fn().mockResolvedValue({ rows: [{ id: "row" }], nextCursor: "" });
    const wrapper = ({ children }: { children: ReactNode }) => <StrictMode>{children}</StrictMode>;
    const { result, unmount } = renderHook(() => useLiveCollection("approvals", () => ({ concept: "approval", seed })), { wrapper });
    await waitFor(() => expect(result.current.snapshot.state).toBe("live"));
    expect(seed).toHaveBeenCalledTimes(1);
    expect(conn.cycles.size).toBe(1);
    expect(conn.statuses.size).toBe(1);
    act(() => conn.reconnected());
    await waitFor(() => expect(seed).toHaveBeenCalledTimes(2));
    unmount();
    disposeSessionCollections(conn as unknown as Connection);
    expect(conn.cycles.size).toBe(0);
    expect(conn.statuses.size).toBe(0);
  });

  it("shares a resource read and subscription across windows and reopening", async () => {
    const conn = connection();
    const seed = vi.fn().mockResolvedValue({ rows: [{ id: "retained" }], nextCursor: "" });
    const first = renderHook(() => useLiveCollection("shared", () => ({ concept: "c", seed })));
    const second = renderHook(() => useLiveCollection("shared", () => ({ concept: "c", seed })));
    await waitFor(() => expect(first.result.current.snapshot.rows).toHaveLength(1));
    expect(first.result.current.source).toBe(second.result.current.source);
    expect(seed).toHaveBeenCalledTimes(1);
    expect(conn.subscriptions.subscribeGraph).toHaveBeenCalledTimes(1);
    first.unmount(); second.unmount();
    const reopened = renderHook(() => useLiveCollection("shared", () => ({ concept: "c", seed })));
    expect(reopened.result.current.snapshot.rows).toEqual([{ id: "retained" }]);
    expect(seed).toHaveBeenCalledTimes(1);
    reopened.unmount();
  });

  it("does no reads for parked content until it becomes visible", async () => {
    connection();
    const seed = vi.fn().mockResolvedValue({ rows: [{ id: "visible" }], nextCursor: "" });
    const hidden = renderHook(() => useLiveCollection("parked", () => ({ concept: "c", seed })), {
      wrapper: ({ children }: { children: ReactNode }) => <LiveReadVisibility.Provider value={false}>{children}</LiveReadVisibility.Provider>,
    });
    expect(seed).not.toHaveBeenCalled();
    hidden.unmount();
    const shown = renderHook(() => useLiveCollection("parked", () => ({ concept: "c", seed })));
    await waitFor(() => expect(shown.result.current.snapshot.rows).toHaveLength(1));
    expect(seed).toHaveBeenCalledTimes(1);
  });

  it("discards a late disconnected read and finishes the queued fresh read", async () => {
    const conn = connection();
    type Page = { rows: { id: string }[]; nextCursor: string };
    let finishOld!: (page: Page) => void;
    let oldSignal!: AbortSignal;
    const seed = vi.fn<(_cursor: string, signal: AbortSignal) => Promise<Page>>().mockImplementationOnce((_cursor, signal) => {
      oldSignal = signal;
      return new Promise<Page>((resolve) => { finishOld = resolve; });
    }).mockResolvedValueOnce({ rows: [{ id: "fresh" }], nextCursor: "" });
    const { result } = renderHook(() => useLiveCollection("approvals", () => ({ concept: "approval", seed })));
    act(() => conn.setStatus("reconnecting"));
    expect(oldSignal.aborted).toBe(true);
    act(() => conn.reconnected());
    expect(seed).toHaveBeenCalledTimes(1);
    await act(async () => finishOld({ rows: [{ id: "stale" }], nextCursor: "" }));
    await waitFor(() => expect(result.current.snapshot.rows).toEqual([{ id: "fresh" }]));
    expect(seed).toHaveBeenCalledTimes(2);
    expect(result.current.snapshot.state).toBe("live");
    expect(result.current.snapshot.error).toBe("");
  });
});
