import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as any }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection, osBridgePath: "", bridgePathFor: () => "" }));
const { fakeConnection, machineRow, withSession } = await import("./harness");
const { useMachineDirectory, useMachineRecord } = await import("../../src/apps/fleet/machines/useMachineDirectory");
const { MachinesProvider, useMachinePresence } = await import("../../src/live/machines");
const rows = Array.from({ length: 25 }, (_, index) => machineRow({
  id: `machine-${String(index).padStart(2, "0")}`, displayName: `Machine ${index}`,
  registeredAt: `2026-10-${String(index + 1).padStart(2, "0")}T00:00:00Z`,
  platformInfo: { hostname: index === 0 ? "archive-host" : `host-${index}`, os: "darwin", arch: "arm64" },
  labels: Object.fromEntries(Array.from({ length: 40 }, (_, n) => [`model:fixture-${n}`, "ctx=65536,tools=1,vision=1"])),
}));
const wrapper = ({ children }: { children: React.ReactNode }) => withSession(<MachinesProvider>{children}</MachinesProvider>);
beforeEach(() => { h.connection = fakeConnection({ myWorkersWithStatus: rows }); });
afterEach(cleanup);

it("loads ten recent machines per request, keeps loaded pages after reopening, and does not fetch the full catalog", async () => {
  const first = renderHook(() => useMachineDirectory("", false), { wrapper });
  await waitFor(() => expect(first.result.current.machines).toHaveLength(10));
  expect(first.result.current.machines[0]?.id).toBe("machine-24");
  expect(h.connection.query.myMachinesBrowse).toHaveBeenCalledTimes(1);
  expect(h.connection.query.myWorkersWithStatus).not.toHaveBeenCalled();
  await act(() => first.result.current.loadMore());
  expect(first.result.current.machines).toHaveLength(20);
  first.unmount();
  const reopened = renderHook(() => useMachineDirectory("", false), { wrapper });
  expect(reopened.result.current.machines).toHaveLength(20);
  expect(h.connection.query.myMachinesBrowse).toHaveBeenCalledTimes(2);
  await act(() => reopened.result.current.loadMore());
  expect(reopened.result.current.machines).toHaveLength(25);
  expect(reopened.result.current.hasMore).toBe(false);
  expect(h.connection.query.myWorkersWithStatus).not.toHaveBeenCalled();
});

it("searches on the server beyond the first page and resolves details independently", async () => {
  const view = renderHook(({ search }) => ({ directory: useMachineDirectory(search, false), detail: useMachineRecord("machine-00") }), { initialProps: { search: "" }, wrapper });
  await waitFor(() => expect(view.result.current.directory.machines).toHaveLength(10));
  await waitFor(() => expect(view.result.current.detail.machine?.id).toBe("machine-00"));
  expect(view.result.current.directory.machines.some(row => row.id === "machine-00")).toBe(false);
  view.rerender({ search: "ARCHIVE-HOST" });
  await waitFor(() => expect(view.result.current.directory.machines).toHaveLength(1));
  expect(view.result.current.directory.machines[0]?.id).toBe("machine-00");
  expect(h.connection.query.myMachinesBrowse).toHaveBeenLastCalledWith({ search: "ARCHIVE-HOST", includeRevoked: false }, expect.objectContaining({ signal: expect.any(AbortSignal) }));
  expect(h.connection.query.myMachineById).toHaveBeenCalledTimes(1);
});

it("keeps loaded rows and retries the same cursor after a later-page refusal", async () => {
  const view = renderHook(() => useMachineDirectory("", false), { wrapper });
  await waitFor(() => expect(view.result.current.machines).toHaveLength(10));
  h.connection.query.myMachinesBrowse.mockRejectedValueOnce(new Error("Page unavailable"));
  await act(() => view.result.current.loadMore());
  expect(view.result.current.machines).toHaveLength(10);
  expect(view.result.current.loadMoreError).toContain("Page unavailable");
  await act(() => view.result.current.loadMore());
  expect(view.result.current.machines).toHaveLength(20);
  expect(h.connection.query.myMachinesBrowse.mock.calls[1][1].cursor).toBe(h.connection.query.myMachinesBrowse.mock.calls[2][1].cursor);
});

it("measures the full inventory baseline against ten directory rows plus lightweight desktop presence", async () => {
  const baseline = await h.connection.query.myWorkersWithStatus({});
  h.connection.query.myWorkersWithStatus.mockClear();
  const view = renderHook(() => ({ directory: useMachineDirectory("", false), presence: useMachinePresence() }), { wrapper });
  await waitFor(() => expect(view.result.current.directory.machines).toHaveLength(10));
  await waitFor(() => expect(view.result.current.presence.presence("machine-00")?.name).toBe("Machine 0"));
  expect(h.connection.query.myWorkersWithStatus).not.toHaveBeenCalled();
  const directory = await h.connection.query.myMachinesBrowse.mock.results[0].value;
  const presence = await h.connection.query.myMachinePresence.mock.results[0].value;
  const bytes = (value: unknown) => new TextEncoder().encode(JSON.stringify(value)).byteLength;
  const before = bytes(baseline.rows());
  const after = bytes(directory.rows()) + bytes(presence.rows());
  expect(directory.rows()).toHaveLength(10);
  expect(presence.rows()).toHaveLength(25);
  expect(presence.rows().every((row: Record<string, unknown>) => !("labels" in row) && !("hardware" in row) && !("apps" in row))).toBe(true);
  expect(after).toBeLessThan(before * 0.6);
  expect({ before, after }).toMatchInlineSnapshot(`
    {
      "after": 8242,
      "before": 56612,
    }
  `);
  console.info(JSON.stringify({ fixture: "25 machines with 40 reported models each", before: { fullInventoryRequests: 1, fullRows: 25, bytes: before }, after: { fullInventoryRequests: 0, directoryRequests: 1, directoryRows: 10, presenceRequests: 1, presenceRows: 25, bytes: after }, reductionPercent: Math.round((1 - after / before) * 100) }));
});
