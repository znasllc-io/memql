import { renderHook } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import type { LiveCollectionHandle } from "../../src/live/useLiveCollection";
const h = vi.hoisted(() => ({ rows: null as unknown, nodes: null as unknown }));
vi.mock("../../src/live/useLiveCollection", () => ({ useLiveCollection: (key: string) => key === "readiness:rows" ? h.rows : h.nodes }));
import { useReadinessFeed } from "../../src/live/readiness";

function feed(rows: Row[]): LiveCollectionHandle<Row> {
  return { snapshot: { rows, state: "live", error: "", version: 1 }, source: {} as LiveCollectionHandle<Row>["source"], reseed: vi.fn() };
}

describe("readiness context stability", () => {
  it("does not invalidate every app for parent renders or unchanged node heartbeats", () => {
    const at = new Date().toISOString();
    const reports = feed([{ id: "readiness", module: "storage", nodeId: "bff", nodeType: "bff", state: "configured", reportedAt: at, core: true }]);
    const nodes = feed([{ id: "bff", health: "healthy", lastSeen: at }]);
    h.rows = reports; h.nodes = nodes;
    const hook = renderHook(() => useReadinessFeed());
    const first = hook.result.current;
    expect(first.of("storage")?.state).toBe("configured");
    h.rows = { ...reports }; h.nodes = { ...nodes };
    hook.rerender();
    expect(hook.result.current).toBe(first);
    h.nodes = { ...nodes, snapshot: { ...nodes.snapshot, version: 2, rows: [{ ...nodes.snapshot.rows[0], lastSeen: new Date(Date.now() + 1000).toISOString() }] } };
    hook.rerender();
    expect(hook.result.current).toBe(first);
    h.rows = { ...reports, snapshot: { ...reports.snapshot, version: 2, rows: [{ ...reports.snapshot.rows[0], state: "unconfigured" }] } };
    hook.rerender();
    expect(hook.result.current).not.toBe(first);
    expect(hook.result.current.of("storage")?.state).toBe("unconfigured");
  });
});
