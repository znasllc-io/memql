import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { useRuns } from "../../src/apps/nexus/useNexus";
import { runFromRow, runTitle } from "../../src/apps/nexus/rows";
import { fakeConnection, rowsResult, runRow } from "./harness";

describe("Nexus run navigation", () => {
  it("walks summary pages without losing older runs or their task titles", async () => {
    const conn = fakeConnection();
    conn.query.workRunSummariesForOwner
      .mockResolvedValueOnce(rowsResult([runRow({ id: "newer", workTitle: "Presidential history" })], "older-page"))
      .mockResolvedValueOnce(rowsResult([runRow({ id: "older" })]));
    h.connection = conn;
    const { result } = renderHook(() => useRuns());
    await waitFor(() => expect(result.current.snapshot.state).toBe("live"));
    expect(result.current.snapshot.rows.map((row) => row.id)).toEqual(["newer", "older"]);
    expect(runTitle(runFromRow(result.current.snapshot.rows[0]!))).toBe("Presidential history");
    expect(conn.query.workRunSummariesForOwner.mock.calls[1]?.[1]).toMatchObject({ cursor: "older-page" });
  });

  it("re-reads even full events so large execution payloads never enter the retained feed", async () => {
    const conn = fakeConnection({ runs: [runRow({ id: "run", status: "running" })] });
    h.connection = conn;
    const { result } = renderHook(() => useRuns());
    await waitFor(() => expect(result.current.snapshot.state).toBe("live"));
    const summary = runRow({ id: "run", status: "failed", errorMessage: "Connection interrupted" });
    conn.query.workRunSummaryForOwner.mockResolvedValueOnce(rowsResult([summary]));
    await act(async () => {
      conn.subscriptions.emit("v1:work:run", { ...summary, input: { document: "x".repeat(5_000_000) } });
    });
    await waitFor(() => expect(result.current.snapshot.rows[0]?.status).toBe("failed"));
    expect(result.current.snapshot.rows[0]).toEqual(summary);
    expect(conn.query.workRunSummaryForOwner).toHaveBeenCalledWith({ runId: "run" }, expect.anything());
    expect(conn.query.executeNamed).not.toHaveBeenCalled();
  });
});
