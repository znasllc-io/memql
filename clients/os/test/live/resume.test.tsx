import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { onAppResume } from "../../src/live/resume";
import { ConnectionNotice } from "../../src/kit/ConnectionNotice";
import { ConnectionRecoveryContext, ConnectionStatusContext, type ShellConnectionStatus } from "../../src/chrome/connection";

afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); });

it("coalesces browser return, ignores hidden/offline pages, and releases listeners", async () => {
  vi.useFakeTimers();
  let visible = "visible", online = true;
  vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visible as DocumentVisibilityState);
  vi.spyOn(navigator, "onLine", "get").mockImplementation(() => online);
  const first = vi.fn(), second = vi.fn();
  const stopFirst = onAppResume(first), stopSecond = onAppResume(second);
  visible = "hidden";
  document.dispatchEvent(new Event("visibilitychange")); window.dispatchEvent(new Event("pageshow"));
  await vi.advanceTimersByTimeAsync(1000); expect(first).not.toHaveBeenCalled();
  visible = "visible"; online = false;
  window.dispatchEvent(new Event("focus"));
  await vi.advanceTimersByTimeAsync(1000); expect(first).not.toHaveBeenCalled();
  online = true;
  for (const event of ["focus", "pageshow", "online"]) window.dispatchEvent(new Event(event));
  document.dispatchEvent(new Event("visibilitychange"));
  await vi.advanceTimersByTimeAsync(250);
  expect(first).toHaveBeenCalledOnce(); expect(second).toHaveBeenCalledOnce();
  stopFirst(); window.dispatchEvent(new Event("pageshow")); await vi.advanceTimersByTimeAsync(250);
  expect(first).toHaveBeenCalledOnce(); expect(second).toHaveBeenCalledTimes(2);
  window.dispatchEvent(new Event("focus")); stopSecond();
  expect(vi.getTimerCount()).toBe(0);
});

it("keeps brief interruptions quiet, preserves content, and offers connection retry after a sustained outage", async () => {
  vi.useFakeTimers();
  const retry = vi.fn();
  const view = (status: ShellConnectionStatus) => <ConnectionRecoveryContext.Provider value={{ managed: true, retry }}><ConnectionStatusContext.Provider value={status}><textarea aria-label="Draft" defaultValue="Keep this" /><ConnectionNotice /></ConnectionStatusContext.Provider></ConnectionRecoveryContext.Provider>;
  const rendered = render(view("connected"));
  const draft = screen.getByRole("textbox"); draft.focus();
  rendered.rerender(view("reconnecting"));
  await act(() => vi.advanceTimersByTimeAsync(1500));
  expect(screen.queryByRole("status")).toBeNull();
  rendered.rerender(view("connected"));
  await act(() => vi.advanceTimersByTimeAsync(1000));
  expect(screen.queryByRole("status")).toBeNull();
  rendered.rerender(view("reconnecting"));
  await act(() => vi.advanceTimersByTimeAsync(2000));
  expect(screen.getByRole("status").textContent).toBe("Reconnecting…");
  // Initial-dial retries alternate states; that must not reset the outage age.
  rendered.rerender(view("disconnected"));
  await act(() => vi.advanceTimersByTimeAsync(28000));
  expect(screen.getByRole("status").textContent).toBe("Can’t reconnect");
  fireEvent.click(screen.getByRole("button", { name: "Retry connection" })); expect(retry).toHaveBeenCalledOnce();
  expect(screen.getByRole("textbox")).toBe(draft); expect(draft).toHaveProperty("value", "Keep this");
  expect(document.activeElement).toBe(draft);
  rendered.rerender(view("connected")); expect(screen.queryByRole("status")).toBeNull();
});

it("starts a fresh visible grace period when returning after a long suspension", async () => {
  vi.useFakeTimers(); let visible = "hidden";
  vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visible as DocumentVisibilityState);
  render(<ConnectionRecoveryContext.Provider value={{managed:true,retry:vi.fn()}}><ConnectionStatusContext.Provider value="reconnecting"><ConnectionNotice /></ConnectionStatusContext.Provider></ConnectionRecoveryContext.Provider>);
  await act(() => vi.advanceTimersByTimeAsync(60000)); expect(screen.queryByRole("status")).toBeNull();
  visible = "visible"; fireEvent(document, new Event("visibilitychange"));
  await act(() => vi.advanceTimersByTimeAsync(1999)); expect(screen.queryByRole("status")).toBeNull();
  await act(() => vi.advanceTimersByTimeAsync(1)); expect(screen.getByRole("status").textContent).toBe("Reconnecting…");
});
