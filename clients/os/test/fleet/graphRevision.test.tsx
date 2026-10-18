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
it("bounds continuous invalidation and pauses a parked window until it becomes visible", async () => {
  const { LiveReadVisibility } = await import("../../src/live/useGraphRevision");
  const connection = fakeConnection(); h.connection = connection;
  let visible = true;
  const { result, rerender } = renderHook(() => useGraphRevision(["v1:worker:registration"]), {
    wrapper: ({ children }) => <ConnectionStatusContext.Provider value="connected"><LiveReadVisibility.Provider value={visible}>{children}</LiveReadVisibility.Provider></ConnectionStatusContext.Provider>,
  });
  expect(result.current).toBe(0); // mounting already performs the initial read
  for (let i = 0; i < 100; i++) act(() => {
    connection.subscriptions.emit("v1:worker:registration", { id: "machine" });
    vi.advanceTimersByTime(10);
  });
  expect(result.current).toBe(1);
  visible = false; rerender();
  act(() => {
    connection.subscriptions.emit("v1:worker:registration", { id: "machine" });
    vi.advanceTimersByTime(10_000);
  });
  expect(result.current).toBe(1);
  visible = true; rerender();
  expect(result.current).toBe(2); // one catch-up read, regardless of missed updates
});
it("pauses hidden browser tabs and catches up when visible", () => {
  const connection = fakeConnection(); h.connection = connection;
  let visibility = "visible";
  const descriptor = Object.getOwnPropertyDescriptor(document, "visibilityState");
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => visibility });
  try {
    const { result } = renderHook(() => useGraphRevision(["v1:worker:registration"]), {
      wrapper: ({ children }) => <ConnectionStatusContext.Provider value="connected">{children}</ConnectionStatusContext.Provider>,
    });
    act(() => { visibility = "hidden"; document.dispatchEvent(new Event("visibilitychange")); });
    act(() => { connection.subscriptions.emit("v1:worker:registration", { id: "machine" }); vi.advanceTimersByTime(10_000); });
    expect(result.current).toBe(0);
    act(() => { visibility = "visible"; document.dispatchEvent(new Event("visibilitychange")); });
    expect(result.current).toBe(1);
  } finally {
    if (descriptor) Object.defineProperty(document, "visibilityState", descriptor);
    else Reflect.deleteProperty(document, "visibilityState");
  }
});
