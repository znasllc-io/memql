import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ConnectionStatusContext, type ShellConnectionStatus } from "../../src/chrome/connection";
import { useGraphRevision } from "../../src/live/useGraphRevision";
import { fakeConnection } from "./harness";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
beforeEach(() => { vi.useFakeTimers(); });
afterEach(() => { cleanup(); vi.useRealTimers(); });
it("invalidates computed readings for relevant events, batches bursts, and unsubscribes on close", () => {
  const connection = fakeConnection(); h.connection = connection;
  const { result, unmount } = renderHook(() => useGraphRevision(["v1:worker:registration"]));
  act(() => {
    connection.subscriptions.emit("v1:models:modelProfile", { id: "unrelated" });
    vi.advanceTimersByTime(150);
  });
  expect(result.current).toBe(0);
  act(() => {
    connection.subscriptions.emit("v1:worker:registration", { id: "machine" });
    connection.subscriptions.emit("v1:worker:registration", { id: "machine" });
    vi.advanceTimersByTime(150);
  });
  expect(result.current).toBe(1);
  act(() => connection.subscriptions.emit("v1:worker:registration", { id: "machine" }));
  unmount();
  expect(vi.getTimerCount()).toBe(0);
});
it("recovers computed readings after reconnect and focus, and can be disabled", () => {
  h.connection = fakeConnection();
  let status: ShellConnectionStatus = "connected";
  const { result, rerender } = renderHook(({ enabled }) => useGraphRevision(["v1:worker:registration"], enabled), {
    initialProps: { enabled: true },
    wrapper: ({ children }) => <ConnectionStatusContext.Provider value={status}>{children}</ConnectionStatusContext.Provider>,
  });
  const initial = result.current;
  status = "reconnecting"; rerender({ enabled: true });
  status = "connected"; rerender({ enabled: true });
  expect(result.current).toBe(initial + 1);
  act(() => { window.dispatchEvent(new Event("focus")); vi.advanceTimersByTime(150); });
  expect(result.current).toBe(initial + 2);
  rerender({ enabled: false });
  act(() => { window.dispatchEvent(new Event("online")); vi.advanceTimersByTime(150); });
  expect(result.current).toBe(initial + 2);
});
