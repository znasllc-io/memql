import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import type { ReactNode } from "react";
import { ConnectionStatusContext, type ShellConnectionStatus } from "../../src/chrome/connection";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { LiveReadVisibility, useGraphRevision } from "../../src/live/useGraphRevision";

afterEach(() => { cleanup(); vi.useRealTimers(); h.connection = null; });

it("uses compact events, coalesces bursts, throttles continuous changes and releases hidden reads", () => {
  vi.useFakeTimers();
  const handlers = new Set<() => void>();
  const stop = vi.fn();
  const subscribeGraph = vi.fn((handler: () => void) => {
    handlers.add(handler);
    return () => { handlers.delete(handler); stop(); };
  });
  h.connection = { subscriptions: { subscribeGraph } };
  let visible = true;
  const wrapper = ({ children }: { children: ReactNode }) => <ConnectionStatusContext.Provider value="connected"><LiveReadVisibility.Provider value={visible}>{children}</LiveReadVisibility.Provider></ConnectionStatusContext.Provider>;
  const { result, rerender, unmount } = renderHook(() => useGraphRevision(["v1:test:item"]), { wrapper });
  expect(subscribeGraph).toHaveBeenCalledWith(expect.any(Function), { concept: "v1:test:item", idOnly: true });
  const burst = () => { for (let n = 0; n < 1000; n++) for (const handler of handlers) handler(); };
  act(() => { burst(); vi.advanceTimersByTime(99); });
  expect(result.current).toBe(0);
  act(() => vi.advanceTimersByTime(1));
  expect(result.current).toBe(1);
  act(() => { burst(); vi.advanceTimersByTime(999); });
  expect(result.current).toBe(1);
  act(() => vi.advanceTimersByTime(1));
  expect(result.current).toBe(2);
  act(() => burst());
  visible = false; rerender();
  expect(handlers.size).toBe(0);
  expect(stop).toHaveBeenCalledTimes(1);
  act(() => vi.advanceTimersByTime(1000));
  expect(result.current).toBe(2);
  unmount();
});

it("leaves browser and connection recovery to session feeds while preserving input events and parked-pane recovery", () => {
  vi.useFakeTimers();
  const handlers = new Set<() => void>();
  h.connection = { subscriptions: { subscribeGraph: (handler: () => void) => {
    handlers.add(handler);
    return () => handlers.delete(handler);
  } } };
  let visible = true;
  let status: ShellConnectionStatus = "connected";
  let tabVisibility = "visible";
  const descriptor = Object.getOwnPropertyDescriptor(document, "visibilityState");
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => tabVisibility });
  try {
    const wrapper = ({ children }: { children: ReactNode }) => <ConnectionStatusContext.Provider value={status}><LiveReadVisibility.Provider value={visible}>{children}</LiveReadVisibility.Provider></ConnectionStatusContext.Provider>;
    const { result, rerender } = renderHook(() => useGraphRevision(["v1:test:item"], true, { externalRecovery: true }), { wrapper });
    act(() => {
      window.dispatchEvent(new Event("focus"));
      window.dispatchEvent(new Event("online"));
      window.dispatchEvent(new Event("pageshow"));
      vi.advanceTimersByTime(1500);
    });
    expect(result.current).toBe(0);
    status = "reconnecting"; rerender();
    status = "connected"; rerender();
    expect(result.current).toBe(0);
    act(() => { tabVisibility = "hidden"; document.dispatchEvent(new Event("visibilitychange")); });
    expect(handlers.size).toBe(0);
    act(() => { tabVisibility = "visible"; document.dispatchEvent(new Event("visibilitychange")); vi.advanceTimersByTime(1500); });
    expect(result.current).toBe(0);
    act(() => { for (const handler of handlers) handler(); vi.advanceTimersByTime(100); });
    expect(result.current).toBe(1);
    visible = false; rerender();
    expect(handlers.size).toBe(0);
    visible = true; rerender();
    expect(result.current).toBe(2);
  } finally {
    if (descriptor) Object.defineProperty(document, "visibilityState", descriptor);
    else Reflect.deleteProperty(document, "visibilityState");
  }
});
