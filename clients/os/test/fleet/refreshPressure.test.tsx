import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { useShareDirectory } from "../../src/apps/fleet/machines/useShareDirectory";
import { fakeConnection, rowsResult, shareDirectoryRow } from "./harness";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
afterEach(() => { cleanup(); vi.useRealTimers(); });
it("continuous worker updates let the directory read finish and request one fresh follow-up", async () => {
  vi.useFakeTimers();
  const pending: { signal: AbortSignal; resolve: (result: ReturnType<typeof rowsResult>) => void }[] = [];
  const connection = fakeConnection({}, { fleetShareDirectory: (_args, opts) => new Promise(resolve => pending.push({ signal: opts!.signal!, resolve })) });
  h.connection = connection;
  const { result, unmount } = renderHook(() => useShareDirectory("machine"));
  for (let i = 0; i < 30; i++) {
    await act(async () => { connection.subscriptions.emit("v1:worker:registration", { id: "machine" }); vi.advanceTimersByTime(150); });
  }
  expect(pending).toHaveLength(1);
  expect(pending[0]!.signal.aborted).toBe(false);
  await act(async () => pending[0]!.resolve(rowsResult([shareDirectoryRow()])));
  expect(result.current.read.state).toBe("ready");
  expect(pending).toHaveLength(2);
  await act(async () => pending[1]!.resolve(rowsResult([shareDirectoryRow()])));
  expect(pending).toHaveLength(2);
  expect(result.current.refreshing).toBe(false);
  unmount();
});
