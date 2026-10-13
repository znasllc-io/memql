import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";
import { StorageHealthFeed, StorageHealthProvider, StoragePanel, StorageWarning } from "../../src/cluster/storage/StorageHealth";
import { readStorageReport, reportFresh } from "../../src/cluster/storage/model";
import { withSession } from "./harness";

const seam = vi.hoisted(() => ({ connection: null as unknown, status: "connected" }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => seam.connection }));
vi.mock("../../src/chrome/connection", () => ({ useConnectionStatus: () => seam.status }));
const settle = async () => act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
const reply = (row: Row) => new Result({ data: [row] } as never);
function measurement(over: Row = {}): Row { return { available: true, observedAt: Date.now() / 1000, dataBytes: 85 * 1024 ** 3, dataCapacityBytes: 100 * 1024 ** 3, walBytes: 2 * 1024 ** 3, walCapacityBytes: 10 * 1024 ** 3, issues: ["data-volume-over-80-percent"], ...over }; }
function surface(query: { clusterStorageStatus: ReturnType<typeof vi.fn> }, connected = true) { return <StorageHealthFeed query={query} connected={connected}><StorageWarning /><StoragePanel /></StorageHealthFeed>; }
beforeEach(() => { vi.useFakeTimers({ toFake: ["Date"] }); seam.status = "connected"; vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible"); vi.spyOn(navigator, "onLine", "get").mockReturnValue(true); });
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); seam.connection = null; });

it("does not read or show owner storage for other roles", async () => {
  const query = { clusterStorageStatus: vi.fn().mockResolvedValue(reply(measurement())) };
  seam.connection = { query };
  const view = render(withSession(<StorageHealthProvider><StorageWarning /><StoragePanel /></StorageHealthProvider>, { role: "admin" }));
  expect(query.clusterStorageStatus).not.toHaveBeenCalled(); expect(screen.queryByText("Storage")).toBeNull();
  view.rerender(withSession(<StorageHealthProvider><StorageWarning /><StoragePanel /></StorageHealthProvider>));
  await screen.findByRole("status"); expect(query.clusterStorageStatus).toHaveBeenCalledTimes(1);
  view.rerender(withSession(<StorageHealthProvider><StorageWarning /><StoragePanel /></StorageHealthProvider>, { role: "reader" }));
  expect(screen.queryByRole("status")).toBeNull();
});

it("keeps a capacity warning across failed and stale reads, clearing only on fresh recovery", async () => {
  const query = { clusterStorageStatus: vi.fn().mockResolvedValue(reply(measurement())) };
  render(surface(query)); await screen.findByRole("status");
  expect(screen.getByRole("status").textContent).toContain("85%");
  query.clusterStorageStatus.mockRejectedValue(new Error("Monitor unavailable"));
  await settle(); vi.setSystemTime(Date.now() + 2000); fireEvent(window, new Event("focus"));
  await screen.findByText("Monitor unavailable"); expect(screen.getByRole("status").textContent).toContain("current reading is unavailable");
  query.clusterStorageStatus.mockResolvedValue(reply(measurement({ observedAt: (Date.now() - 20 * 60_000) / 1000, issues: [] })));
  await settle(); vi.setSystemTime(Date.now() + 2000); fireEvent(window, new Event("focus"));
  await screen.findByText("The storage report is out of date."); expect(screen.getByRole("status")).toBeTruthy();
  query.clusterStorageStatus.mockResolvedValue(reply(measurement({ dataBytes: 50 * 1024 ** 3, issues: [] })));
  await settle(); vi.setSystemTime(Date.now() + 2000); fireEvent(window, new Event("focus"));
  await waitFor(() => expect(screen.queryByRole("status")).toBeNull());
  expect(screen.getByText(/50%/)).toBeTruthy();
});

it("pauses while hidden or disconnected and aborts obsolete reads", async () => {
  const query = { clusterStorageStatus: vi.fn().mockResolvedValue(reply(measurement())) };
  const view = render(surface(query, false)); expect(query.clusterStorageStatus).not.toHaveBeenCalled();
  view.rerender(surface(query)); await screen.findByRole("status");
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  await settle(); vi.setSystemTime(Date.now() + 2000); fireEvent(window, new Event("focus"));
  expect(query.clusterStorageStatus).toHaveBeenCalledTimes(1);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  let finish!: (value: Result) => void;
  query.clusterStorageStatus.mockImplementation(() => new Promise<Result>((resolve) => { finish = resolve; }));
  await settle(); fireEvent(document, new Event("visibilitychange")); await waitFor(() => expect(query.clusterStorageStatus).toHaveBeenCalledTimes(2));
  const signal = query.clusterStorageStatus.mock.calls[1]![1].signal as AbortSignal;
  view.rerender(surface(query, false)); expect(signal.aborted).toBe(true);
  await act(async () => finish(reply(measurement({ issues: [] }))));
  expect(screen.getByRole("status")).toBeTruthy();
});

it("rejects missing values and future or stale timestamps instead of inventing health", () => {
  expect(() => readStorageReport(measurement({ walCapacityBytes: 0 }))).toThrow();
  expect(() => readStorageReport({ available: false, reason: "No monitor" })).toThrow("No monitor");
  expect(reportFresh(readStorageReport(measurement({ observedAt: Date.now() / 1000 + 120 })), Date.now())).toBe(false);
  expect(reportFresh(readStorageReport(measurement({ observedAt: Date.now() / 1000 - 901 })), Date.now())).toBe(false);
});
