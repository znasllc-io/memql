import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { QueryClient, Result, type Connection, type Row } from "@znasllc-io/memql-sdk-core/client";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { useSiteTraffic } from "../../src/apps/deployables/useSiteTraffic";
import { LiveReadVisibility } from "../../src/live/useGraphRevision";
import { disposeSessionCollections } from "../../src/live/sessionCollections";
import { parseCall } from "../callArgs";

afterEach(() => { vi.useRealTimers(); if (h.connection) disposeSessionCollections(h.connection as Connection); });

function connection() {
  const pending: Array<{ ids: string[]; complete: (rows: Row[]) => void; signal?: AbortSignal }> = [];
  const query = Object.assign(Object.create(QueryClient.prototype), {
    executeNamed: vi.fn((_name: string, call: string, options?: { signal?: AbortSignal }) => new Promise(resolve => {
      pending.push({ ids: parseCall(call).args.siteIds as string[], signal: options?.signal, complete: rows => resolve(new Result({ data: rows } as never)) });
    })),
  }) as QueryClient;
  const conn = { query, subscriptions: { subscribeGraph: vi.fn(() => () => {}) } };
  h.connection = conn;
  return { conn, pending };
}

describe("traffic summaries", () => {
  it("shares requests, restores cached figures, and ignores a late answer after the visible ids change", async () => {
    const { pending } = connection();
    const hook = renderHook(({ ids }) => useSiteTraffic(ids, "hour"), { initialProps: { ids: ["a"] } });
    await waitFor(() => expect(pending).toHaveLength(1));
    await act(async () => pending[0]!.complete([{ siteId: "a", requestCount: 7 }]));
    expect(hook.result.current.figures.get("a")?.requests).toBe(7);
    const duplicate = renderHook(() => useSiteTraffic(["a"], "hour"));
    expect(duplicate.result.current.figures.get("a")?.requests).toBe(7);
    expect(pending).toHaveLength(1);
    hook.rerender({ ids: ["b"] });
    await waitFor(() => expect(pending).toHaveLength(2));
    hook.rerender({ ids: [] });
    await act(async () => pending[1]!.complete([{ siteId: "b", requestCount: 99 }]));
    expect(hook.result.current.figures.size).toBe(0);
    hook.rerender({ ids: ["a"] });
    expect(hook.result.current.figures.get("a")?.requests).toBe(7);
    expect(pending).toHaveLength(2);
  });

  it("does no work while the pane is hidden and stops its refresh cadence when parked", async () => {
    const { pending, conn } = connection();
    let visible = false;
    const hook = renderHook(() => useSiteTraffic(["a"], "hour"), { wrapper: ({ children }) => <LiveReadVisibility.Provider value={visible}>{children}</LiveReadVisibility.Provider> });
    await act(async () => {});
    expect(pending).toHaveLength(0);
    visible = true; hook.rerender();
    await waitFor(() => expect(pending).toHaveLength(1));
    await act(async () => pending[0]!.complete([{ siteId: "a", requestCount: 7 }]));
    expect(conn.subscriptions.subscribeGraph).not.toHaveBeenCalled();
    vi.useFakeTimers();
    visible = false; hook.rerender();
    await act(async () => { await vi.advanceTimersByTimeAsync(120_000); });
    expect(pending).toHaveLength(1);
  });
});
