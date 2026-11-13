import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { rowsResult } from "../cluster/harness";
import { clearEffectiveCapabilities, setEffectiveCapabilities } from "../../src/system/roles";
import { installSeededAccess } from "../seededAccess";
import {
  ASK_READINESS_MAX_PROBES,
  connectionDotTone,
  connectionDotTooltip,
  probeDelayMs,
  useAskReadiness,
} from "../../src/ask/useAskReadiness";

const mock = vi.hoisted(() => ({
  query: vi.fn(), connected: "connected" as "connected" | "reconnecting" | "disconnected", userId: "owner", moduleState: "unconfigured",
}));
vi.mock("../../src/live/connection", () => {
  const connection = { query: { inferenceStatus: mock.query } };
  return { useOsConnection: () => connection };
});
vi.mock("../../src/chrome/connection", () => ({ useConnectionStatus: () => mock.connected }));
vi.mock("../../src/chrome/access", () => ({ useSession: () => ({ access: { userId: mock.userId }, readiness: { of: () => ({ state: mock.moduleState }) } }) }));
afterEach(() => { cleanup(); vi.useRealTimers(); installSeededAccess("owner"); });
beforeEach(() => { installSeededAccess("owner"); mock.query.mockReset(); mock.connected = "connected"; mock.userId = "owner"; mock.moduleState = "unconfigured"; });

it.each(["not granted", "denied", "not loaded"])("does not probe inference when Ask is %s, even on refresh or reconnect", async (state) => {
  if (state === "not loaded") clearEffectiveCapabilities();
  else setEffectiveCapabilities(state === "denied" ? [{ verb: "read", resource: "app:ask", effect: "deny", source: "user" }] : []);
  const view = renderHook(useAskReadiness);
  expect(view.result.current.state).toBe("disconnected");
  await act(async () => { view.result.current.refresh(); });
  mock.connected = "reconnecting"; view.rerender();
  mock.connected = "connected"; view.rerender();
  await act(async () => {});
  expect(mock.query).not.toHaveBeenCalled();
});

it("aborts a pending probe on revocation and ignores its answer until a fresh allowed probe", async () => {
  let resolve!: (r: ReturnType<typeof rowsResult>) => void;
  mock.query.mockReturnValueOnce(new Promise((r) => { resolve = r; }));
  const view = renderHook(useAskReadiness);
  await waitFor(() => expect(mock.query).toHaveBeenCalledOnce());
  const signal = mock.query.mock.calls[0]![1].signal as AbortSignal;
  setEffectiveCapabilities([]); view.rerender();
  expect(signal.aborted).toBe(true);
  await act(async () => resolve(rowsResult([{ streamingChatEligible: true }])));
  expect(view.result.current.state).toBe("disconnected");
  expect(mock.query).toHaveBeenCalledOnce();
  mock.query.mockResolvedValue(rowsResult([{ streamingChatEligible: true }]));
  installSeededAccess("owner"); view.rerender();
  expect(view.result.current.state).toBe("checking");
  await waitFor(() => expect(view.result.current.state).toBe("ready"));
  expect(mock.query).toHaveBeenCalledTimes(2);
});

it.each([false, undefined, "true"])("never treats aggregate eligible or catalog installation as streaming readiness (%s)", async (streamingChatEligible) => {
  mock.query.mockResolvedValue(rowsResult([{ eligible: true, fleetCatalogInstalled: true, streamingChatEligible }]));
  const view = renderHook(useAskReadiness);
  expect(view.result.current.state).toBe("checking");
  await waitFor(() => expect(view.result.current.state).toBe("checking"));
  expect(view.result.current.message).toBe("");
});
it("accepts an authoritative streaming route on a BFF without local dispatch or module readiness", async () => {
  mock.query.mockResolvedValue(rowsResult([{ eligible: true, streamingChatEligible: true, fleetInferenceInstalled: false }]));
  const view = renderHook(useAskReadiness);
  await waitFor(() => expect(view.result.current.state).toBe("ready"));
  expect(view.result.current.message).toBe("");
});
it("discards a prior owner's pending response and rechecks after disconnect", async () => {
  let resolve!: (r: ReturnType<typeof rowsResult>) => void;
  mock.query.mockReturnValueOnce(new Promise((r) => { resolve = r; })).mockResolvedValue(rowsResult([{ streamingChatEligible: false }]));
  const view = renderHook(useAskReadiness);
  await waitFor(() => expect(mock.query).toHaveBeenCalledOnce());
  mock.userId = "another-owner"; view.rerender();
  await waitFor(() => expect(view.result.current.state).toBe("checking"));
  await act(async () => resolve(rowsResult([{ streamingChatEligible: true }])));
  expect(view.result.current.state).not.toBe("ready");
  mock.connected = "reconnecting"; view.rerender();
  expect(view.result.current.message).toBe("");
  mock.query.mockResolvedValue(rowsResult([{ streamingChatEligible: true }]));
  mock.connected = "connected"; view.rerender(); expect(view.result.current.state).toBe("checking");
  await waitFor(() => expect(view.result.current.state).toBe("ready"));
});
it("stays ready across brief BFF reconnecting so GoingAway does not yellow-flap Ask", async () => {
  mock.query.mockResolvedValue(rowsResult([{ streamingChatEligible: true }]));
  const view = renderHook(useAskReadiness);
  await waitFor(() => expect(view.result.current.state).toBe("ready"));
  mock.connected = "reconnecting"; view.rerender();
  expect(view.result.current.state).toBe("ready");
  expect(connectionDotTone("reconnecting", view.result.current)).toBe("reachable");
});
it("goes red and stops probing after the bounded budget with increasing backoff", async () => {
  vi.useFakeTimers();
  mock.query.mockResolvedValue(rowsResult([{ streamingChatEligible: false }]));
  const view = renderHook(useAskReadiness);
  for (let i = 0; i < ASK_READINESS_MAX_PROBES; i++) {
    await act(async () => { await vi.advanceTimersByTimeAsync(probeDelayMs(i + 1)); });
  }
  expect(view.result.current.state).toBe("unavailable");
  expect(view.result.current.message).toBe("");
  const calls = mock.query.mock.calls.length;
  await act(async () => { await vi.advanceTimersByTimeAsync(probeDelayMs(ASK_READINESS_MAX_PROBES) * 3); });
  expect(mock.query.mock.calls.length).toBe(calls);
  expect(connectionDotTone("connected", view.result.current)).toBe("failed");
});
it("probeDelayMs increases then caps", () => {
  expect(probeDelayMs(1)).toBeLessThan(probeDelayMs(3));
  expect(probeDelayMs(10)).toBe(probeDelayMs(8));
});
it("connectionDotTone and tooltip reflect yellow/blue/red without banners", () => {
  expect(connectionDotTone("disconnected", { state: "ready" })).toBe("off");
  expect(connectionDotTone("reconnecting", { state: "ready" })).toBe("reachable");
  expect(connectionDotTone("reconnecting", { state: "checking" })).toBe("unreachable");
  expect(connectionDotTone("connected", { state: "ready" })).toBe("reachable");
  expect(connectionDotTone("connected", { state: "unavailable" })).toBe("failed");
  expect(connectionDotTone("connected", { state: "checking" })).toBe("unreachable");
  expect(connectionDotTooltip("reachable")).toMatch(/ready/i);
  expect(connectionDotTooltip("unreachable")).toMatch(/Connecting/i);
  expect(connectionDotTooltip("failed")).toMatch(/Fleet/i);
  expect(connectionDotTooltip("off")).toMatch(/Not connected/i);
});
