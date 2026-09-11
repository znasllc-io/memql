import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { rowsResult } from "../cluster/harness";
import { connectionDotTone, useAskReadiness } from "../../src/ask/useAskReadiness";

const mock = vi.hoisted(() => ({
  query: vi.fn(), connected: "connected" as "connected" | "reconnecting" | "disconnected", userId: "owner", moduleState: "unconfigured",
}));
vi.mock("../../src/live/connection", () => {
  const connection = { query: { inferenceStatus: mock.query } };
  return { useOsConnection: () => connection };
});
vi.mock("../../src/chrome/connection", () => ({ useConnectionStatus: () => mock.connected }));
vi.mock("../../src/chrome/access", () => ({ useSession: () => ({ access: { userId: mock.userId }, readiness: { of: () => ({ state: mock.moduleState }) } }) }));
afterEach(() => { cleanup(); vi.useRealTimers(); });
beforeEach(() => { mock.query.mockReset(); mock.connected = "connected"; mock.userId = "owner"; mock.moduleState = "unconfigured"; });

it.each([false, undefined, "true"])("never treats aggregate eligible or catalog installation as streaming readiness (%s)", async (streamingChatEligible) => {
  mock.query.mockResolvedValue(rowsResult([{ eligible: true, fleetCatalogInstalled: true, streamingChatEligible }]));
  const view = renderHook(useAskReadiness);
  expect(view.result.current.state).toBe("checking");
  await waitFor(() => expect(view.result.current.state).toBe(streamingChatEligible === false ? "unavailable" : "error"));
});
it("accepts an authoritative streaming route on a BFF without local dispatch or module readiness", async () => {
  mock.query.mockResolvedValue(rowsResult([{ eligible: true, streamingChatEligible: true, fleetInferenceInstalled: false }]));
  const view = renderHook(useAskReadiness);
  await waitFor(() => expect(view.result.current.state).toBe("ready"));
});
it("discards a prior owner's pending response and rechecks after disconnect", async () => {
  let resolve!: (r: ReturnType<typeof rowsResult>) => void;
  mock.query.mockReturnValueOnce(new Promise((r) => { resolve = r; })).mockResolvedValue(rowsResult([{ streamingChatEligible: false }]));
  const view = renderHook(useAskReadiness);
  mock.userId = "another-owner"; view.rerender();
  await waitFor(() => expect(view.result.current.state).toBe("unavailable"));
  await act(async () => resolve(rowsResult([{ streamingChatEligible: true }])));
  expect(view.result.current.state).toBe("unavailable");
  // SDK reconnecting is not a hard lost-connection banner.
  mock.connected = "reconnecting"; view.rerender(); expect(view.result.current.state).toBe("reconnecting");
  mock.query.mockResolvedValue(rowsResult([{ streamingChatEligible: true }]));
  mock.connected = "connected"; view.rerender(); expect(view.result.current.state).toBe("checking");
  await waitFor(() => expect(view.result.current.state).toBe("ready"));
});
it("offers a fresh read after failure and checks shared route changes without heartbeat refetches", async () => {
  vi.useFakeTimers(); mock.query.mockRejectedValueOnce(new Error("offline")).mockResolvedValue(rowsResult([{ streamingChatEligible: true }]));
  const view = renderHook(useAskReadiness);
  await act(async () => {}); expect(view.result.current.state).toBe("error");
  await act(async () => view.result.current.refresh()); expect(view.result.current.state).toBe("ready");
  view.rerender(); expect(mock.query).toHaveBeenCalledTimes(2);
  mock.query.mockResolvedValue(rowsResult([{ streamingChatEligible: false }]));
  await act(async () => vi.advanceTimersByTime(30_000)); expect(view.result.current.state).toBe("unavailable");
});
it("renders a retryable readiness error when the connection rejects the query synchronously", async () => {
  mock.query.mockImplementation(() => { throw new Error("Connection is closed"); });
  const view = renderHook(useAskReadiness);
  await waitFor(() => expect(view.result.current.state).toBe("error"));
});

it("connectionDotTone reflects inference readiness, not bare WebSocket status", () => {
  expect(connectionDotTone("disconnected", { state: "ready" })).toBe("off");
  expect(connectionDotTone("reconnecting", { state: "ready" })).toBe("unreachable");
  expect(connectionDotTone("connected", { state: "ready" })).toBe("reachable");
  expect(connectionDotTone("connected", { state: "unavailable" })).toBe("unreachable");
  expect(connectionDotTone("connected", { state: "checking" })).toBe("unreachable");
  expect(connectionDotTone("connected", { state: "error" })).toBe("unreachable");
  expect(connectionDotTone("connected", { state: "reconnecting" })).toBe("unreachable");
});
