import { StrictMode, type ReactNode } from "react";
import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Connection, ConnectionStatus } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
const { useLiveCollection } = await import("../../src/live/useLiveCollection");

function connection() {
  const cycles = new Set<(cycle: number) => void>();
  const statuses = new Set<(event: { status: ConnectionStatus }) => void>();
  let status: ConnectionStatus = "connected";
  const conn = {
    get status() { return status; },
    subscriptions: { subscribeGraph: vi.fn(() => () => {}) },
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
    cycles, statuses,
  };
  h.connection = conn as unknown as Connection;
  return conn;
}

describe("live collection reconnects", () => {
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
    expect(conn.cycles.size).toBe(0);
    expect(conn.statuses.size).toBe(0);
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
