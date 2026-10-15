import { StrictMode } from "react";
import { act, fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection, bridgePathFor: (base: string) => base + "_memql/ws", osBridgePath: "/_memql/ws" }));
const { fakeConnection, withSession, machineRow, modelPullRow, appSessionRow, rowsResult } = await import("./harness");
const { MachinesProvider, useMachines } = await import("../../src/live/machines");
const { AppSessionsProvider, useAppSessions, useAppSessionDetail, APP_SESSION_CONCEPT } = await import("../../src/apps/fleet/apps/useAppSessions");
const { RecentWork } = await import("../../src/apps/fleet/apps/RecentWork");
const { RoutingPolicyProvider, useRoutingPolicy } = await import("../../src/apps/fleet/routing/useRoutingPolicy");
const { useModelPulls } = await import("../../src/apps/fleet/machines/useModelPulls");
const { useWorkbenches } = await import("../../src/apps/fleet/workbenches/useWorkbenches");
const session = (id: string, over: Row = {}) => appSessionRow({ id, ownerUserId: "me", workerId: "machine", runId: `run-${id}`, startedAt: `2026-10-14T10:00:${id.padStart(2, "0")}Z`, ...over });

beforeEach(() => { h.connection = null; });

describe("Fleet broadcast connections", () => {
  it("shares sessions, folds new work and status without rereads, and keeps recent work at five", async () => {
    const connection = fakeConnection({ appSessionsForUser: [1, 2, 3, 4, 5, 6].map(id => session(String(id))) });
    h.connection = connection;
    const open = vi.fn(), all = vi.fn();
    function OtherConsumer() { const feed = useAppSessions(); return <output aria-label="All sessions">{feed.sessions.length}</output>; }
    render(withSession(<StrictMode><AppSessionsProvider><RecentWork machineId="machine" onOpenSession={open} onAllSessions={all} /><OtherConsumer /></AppSessionsProvider></StrictMode>, { userId: "me" }));
    await screen.findByText("run-6");
    expect(screen.queryByText("run-1")).toBeNull();
    expect(screen.getByLabelText("All sessions").textContent).toBe("6");
    expect(connection.query.appSessionsForUser).toHaveBeenCalledTimes(1);
    const nav = screen.getByRole("button", { name: "All app sessions" });
    nav.focus();
    await act(async () => {
      connection.subscriptions.emit(APP_SESSION_CONCEPT, session("7", { status: "running" }), "NODE_CREATED");
      connection.subscriptions.emit(APP_SESSION_CONCEPT, session("8", { workerId: "elsewhere" }), "NODE_CREATED");
      connection.subscriptions.emit(APP_SESSION_CONCEPT, session("9", { ownerUserId: "someone-else" }), "NODE_CREATED");
    });
    expect(screen.getByText("run-7")).toBeTruthy();
    expect(screen.queryByText("run-2")).toBeNull();
    expect(screen.queryByText("run-8")).toBeNull();
    expect(screen.queryByText("run-9")).toBeNull();
    expect(document.activeElement).toBe(nav);
    expect(screen.getByLabelText("All sessions").textContent).toBe("8");
    await act(async () => connection.subscriptions.emit(APP_SESSION_CONCEPT, session("7", { status: "failed" })));
    const row = screen.getByRole("button", { name: /run-7/ });
    expect(within(row).getByText("failed")).toBeTruthy();
    fireEvent.click(row); expect(open).toHaveBeenCalledWith("7");
    fireEvent.click(nav); expect(all).toHaveBeenCalledOnce();
    expect(connection.query.appSessionsForUser).toHaveBeenCalledTimes(1);
  });

  it("automatically retries a failed initial session read, then stops rereading a healthy feed", async () => {
    vi.useFakeTimers();
    try {
      const connection = fakeConnection(); h.connection = connection;
      connection.query.appSessionsForUser.mockRejectedValueOnce(new Error("Stream unavailable"));
      const view = render(withSession(<RecentWork machineId="machine" onAllSessions={vi.fn()} />, { userId: "me" }));
      await act(async () => {});
      expect(screen.getByText("Recent app sessions could not be read.")).toBeTruthy();
      expect(screen.queryByText("No recent work")).toBeNull();
      connection.query.appSessionsForUser.mockResolvedValue(rowsResult([session("1")]));
      await act(async () => { await vi.advanceTimersByTimeAsync(15_000); });
      expect(screen.getByText("run-1")).toBeTruthy();
      expect(connection.query.appSessionsForUser).toHaveBeenCalledTimes(2);
      await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
      expect(connection.query.appSessionsForUser).toHaveBeenCalledTimes(2);
      view.unmount();
    } finally { vi.useRealTimers(); }
  });

  it("recovers missed machine events after reconnect and stops connection listeners on unmount", async () => {
    const connection = fakeConnection({ myWorkersWithStatus: [machineRow({ id: "one", ownerUserId: "me" })] });
    let cycle = () => {}; let status = (_: { status: string }) => {};
    const stopCycle = vi.fn(), stopStatus = vi.fn();
    h.connection = Object.assign(connection, {
      onConnectionCycle: (fn: typeof cycle) => { cycle = fn; return stopCycle; },
      onStatusChange: (fn: typeof status) => { status = fn; return stopStatus; },
    });
    const view = renderHook(useMachines, { wrapper: ({ children }) => withSession(<MachinesProvider>{children}</MachinesProvider>, { userId: "me" }) });
    await waitFor(() => expect(view.result.current.count).toBe(1));
    await act(async () => connection.subscriptions.emit("v1:worker:registration", machineRow({ id: "other", ownerUserId: "elsewhere" })));
    expect(view.result.current.count).toBe(1);
    await act(async () => status({ status: "disconnected" }));
    expect(view.result.current.feedState).toBe("disconnected");
    expect(view.result.current.count).toBe(1);
    connection.query.myWorkersWithStatus.mockResolvedValue(rowsResult([machineRow({ id: "two", ownerUserId: "me" })]));
    await act(async () => cycle());
    expect(view.result.current.feedState).toBe("live");
    expect(view.result.current.presence("two")).not.toBeNull();
    expect(view.result.current.presence("one")).toBeNull();
    expect(connection.query.myWorkersWithStatus).toHaveBeenCalledTimes(2);
    view.unmount(); expect(stopCycle).toHaveBeenCalledOnce(); expect(stopStatus).toHaveBeenCalledOnce();
  });

  it("uses one routing feed across views and filters other owners' updates", async () => {
    const connection = fakeConnection({ myRoutingPolicies: [{ id: "policy", ownerUserId: "me", active: true, strategy: "firstFit" }] });
    h.connection = connection;
    function Reader({ name }: { name: string }) { return <output aria-label={name}>{useRoutingPolicy().policy?.strategy}</output>; }
    render(withSession(<RoutingPolicyProvider><Reader name="Models" /><Reader name="Routes" /></RoutingPolicyProvider>, { userId: "me" }));
    await waitFor(() => expect(screen.getByLabelText("Models").textContent).toBe("firstFit"));
    await act(async () => connection.subscriptions.emit("v1:worker:routingPolicy", { id: "policy", ownerUserId: "me", active: true, strategy: "roundRobin" }));
    expect(screen.getByLabelText("Models").textContent).toBe("roundRobin");
    expect(screen.getByLabelText("Routes").textContent).toBe("roundRobin");
    await act(async () => connection.subscriptions.emit("v1:worker:routingPolicy", { id: "foreign", ownerUserId: "other", active: true, strategy: "labelMatch" }));
    expect(screen.getByLabelText("Routes").textContent).toBe("roundRobin");
    expect(connection.query.myRoutingPolicies).toHaveBeenCalledTimes(1);
  });

  it("folds download progress from CDC envelopes and excludes other owners and machines", async () => {
    const connection = fakeConnection(); h.connection = connection;
    const view = renderHook(() => useModelPulls("machine"), { wrapper: ({ children }) => withSession(children, { userId: "me" }) });
    await waitFor(() => expect(view.result.current.state).toBe("live"));
    await act(async () => {
      for (const [id, ownerUserId, workerId] of [["mine", "me", "machine"], ["foreign", "other", "machine"], ["elsewhere", "me", "other-machine"]]) {
        connection.subscriptions.emit("v1:worker:modelPull", { id, payload: modelPullRow({ id, ownerUserId, workerId, status: "running", completedBytes: 50, totalBytes: 100 }) });
      }
    });
    expect(view.result.current.pulls.map(pull => pull.pullId)).toEqual(["mine"]);
    expect(view.result.current.live?.completedBytes).toBe(50);
    await act(async () => connection.subscriptions.emit("v1:worker:modelPull", { id: "mine", payload: modelPullRow({ id: "mine", ownerUserId: "me", workerId: "machine", status: "completed", completedBytes: 100, totalBytes: 100 }) }));
    expect(view.result.current.live).toBeNull();
    expect(view.result.current.pulls[0]?.completedBytes).toBe(100);
    expect(connection.query.modelPullsForWorker).toHaveBeenCalledTimes(1);
  });

  it("keeps workspace event scope equal to the named read", async () => {
    const connection = fakeConnection(); h.connection = connection;
    const view = renderHook(useWorkbenches, { wrapper: ({ children }) => withSession(children, { userId: "me" }) });
    await waitFor(() => expect(view.result.current.workspaceState).toBe("live"));
    await act(async () => {
      connection.subscriptions.emit("v1:workbench:workspace", { id: "mine", ownerUserId: "me", status: "provisioned" });
      connection.subscriptions.emit("v1:workbench:workspace", { id: "foreign", ownerUserId: "other", status: "provisioned" });
    });
    expect(view.result.current.source?.snapshot.rows.map(row => row.id)).toEqual(["mine"]);
    expect(connection.query.myWorkspaces).toHaveBeenCalledTimes(1);
  });

  it("scopes a session detail, repairs an id-only event, and resets on account changes", async () => {
    const connection = fakeConnection({ appSessionById: [session("1", { status: "running" })] }); h.connection = connection;
    // A component wrapper can change the actor without changing the connection.
    function Detail() { const detail = useAppSessionDetail("1"); return <output>{detail.session?.status ?? detail.state}</output>; }
    const mounted = render(withSession(<Detail />, { userId: "me" }));
    await screen.findByText("running");
    await act(async () => connection.subscriptions.emit(APP_SESSION_CONCEPT, session("2", { status: "failed" })));
    expect(screen.queryByText("failed")).toBeNull();
    connection.query.appSessionById.mockResolvedValue(rowsResult([session("1", { status: "ended" })]));
    await act(async () => connection.subscriptions.emit(APP_SESSION_CONCEPT, { id: "1" }, "NODE_UPDATED", true));
    await screen.findByText("ended");
    connection.query.appSessionById.mockResolvedValue(rowsResult([]));
    mounted.rerender(withSession(<Detail />, { userId: "other" }));
    await screen.findByText("live");
    expect(screen.queryByText("ended")).toBeNull();
  });
});
