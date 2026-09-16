import { StrictMode, type ReactNode } from "react";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ chat: vi.fn(), connection: { dispatcher: {} } }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("@znasllc-io/memql-sdk-core/ai", () => ({ aiChat: (...args: unknown[]) => h.chat(...args) }));
const { useModelResponse, RESPONSE_TIMEOUT_MS } = await import("../../src/apps/fleet/addMachine/useModelResponse");

beforeEach(() => { h.chat.mockReset(); h.chat.mockResolvedValue({ message: { content: "hello" } }); });
afterEach(() => { cleanup(); vi.useRealTimers(); });

it("automatically verifies once under StrictMode, pinned to the selected machine and model", async () => {
  const view = renderHook(() => useModelResponse("machine-1", "text-model", true), {
    wrapper: ({ children }: { children: ReactNode }) => <StrictMode>{children}</StrictMode>,
  });
  await waitFor(() => expect(view.result.current.response.state).toBe("passed"));
  view.rerender();
  expect(h.chat).toHaveBeenCalledTimes(1);
  expect(h.chat.mock.calls[0]?.[2]).toMatchObject({ provider: "fleet:text-model", fleetRegistrationId: "machine-1" });
});

it.each(["", "  \n "])("an empty response cannot complete the check, and retry can recover", async content => {
  h.chat.mockResolvedValueOnce({ message: { content } });
  const view = renderHook(() => useModelResponse("machine-1", "text-model", true));
  await waitFor(() => expect(view.result.current.response.state).toBe("failed"));
  expect(view.result.current.response.error).toContain("empty response");
  act(() => view.result.current.retry());
  await waitFor(() => expect(view.result.current.response.state).toBe("passed"));
  expect(h.chat).toHaveBeenCalledTimes(2);
});

it("keeps a refusal failed until an explicit retry", async () => {
  h.chat.mockRejectedValueOnce(new Error("Machine is unavailable"));
  const view = renderHook(() => useModelResponse("machine-1", "text-model", true));
  await waitFor(() => expect(view.result.current.response.state).toBe("failed"));
  view.rerender();
  expect(view.result.current.response.error).toBe("Machine is unavailable");
  expect(h.chat).toHaveBeenCalledTimes(1);
});

it("does not send a check before models are available", async () => {
  const view = renderHook(({ enabled }) => useModelResponse("machine-1", "text-model", enabled), { initialProps: { enabled: false } });
  expect(h.chat).not.toHaveBeenCalled();
  expect(view.result.current.response.state).toBe("waiting");
  view.rerender({ enabled: true });
  await waitFor(() => expect(view.result.current.response.state).toBe("passed"));
});

it("times out, aborts the request, and ignores a late response", async () => {
  vi.useFakeTimers();
  let answer!: (value: unknown) => void;
  h.chat.mockImplementation(() => new Promise(resolve => { answer = resolve; }));
  const view = renderHook(() => useModelResponse("machine-1", "text-model", true));
  await act(async () => vi.advanceTimersByTimeAsync(1));
  expect(view.result.current.response.state).toBe("running");
  await act(async () => vi.advanceTimersByTimeAsync(RESPONSE_TIMEOUT_MS));
  expect(view.result.current.response.state).toBe("failed");
  expect(h.chat.mock.calls[0]?.[2].signal.aborted).toBe(true);
  await act(async () => answer({ message: { content: "late hello" } }));
  expect(view.result.current.response.state).toBe("failed");
});

it("a different machine cannot inherit a pending or successful response", async () => {
  let oldAnswer!: (value: unknown) => void;
  h.chat.mockImplementationOnce(() => new Promise(resolve => { oldAnswer = resolve; }));
  h.chat.mockRejectedValueOnce(new Error("New machine unavailable"));
  const view = renderHook(({ id }) => useModelResponse(id, "text-model", true), { initialProps: { id: "machine-1" } });
  await waitFor(() => expect(h.chat).toHaveBeenCalledTimes(1));
  view.rerender({ id: "machine-2" });
  expect(h.chat.mock.calls[0]?.[2].signal.aborted).toBe(true);
  await waitFor(() => expect(view.result.current.response.state).toBe("failed"));
  await act(async () => oldAnswer({ message: { content: "old hello" } }));
  expect(view.result.current.response).toEqual({ state: "failed", error: "New machine unavailable" });
});
