import { act, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { readTheBar } from "../readTheBar";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { NexusApp } = await import("../../src/apps/nexus/NexusApp");
const { useDecideApproval } = await import("../../src/apps/nexus/actions");
const { approvalRow, builtinReply, fakeConnection, rowsResult, runRow, withSession } = await import("./harness");

const budget = () => approvalRow({ id: "budget", runId: "run", kind: "budget", question: "More model calls needed", subject: {
  ceiling: "modelCalls", actual: "12", limit: "12", resumeKind: "checkpoint", continuationId: "saved-checkpoint",
} });
const parked = () => runRow({ id: "run", workTitle: "Update the requested record", status: "waiting", waitingOn: { kind: "approval", subject: "budget" } });
const approved = () => ({ ...budget(), decision: "approved", answer: { newLimit: 20, feedback: "Use the completed lookup." } });
const receipt = (runResumed: boolean, extra: Record<string, unknown> = {}) => builtinReply("decideApproval", [{ approvalId: "budget", runId: "run", decision: "approved", runResumed, ...extra }]);

function connection(pending = true) {
  const conn = fakeConnection({ approvals: pending ? [budget()] : [], runs: [parked()] });
  const readApproval = vi.fn(async (_args: Record<string, unknown>, _opts?: unknown) => rowsResult([approved()]));
  const readRun = vi.fn(async (_args: Record<string, unknown>, _opts?: unknown) => rowsResult([parked()]));
  Object.assign(conn.query, { workApprovalForOwner: readApproval, workRunForOwner: readRun });
  return { conn, readApproval, readRun };
}

function mount(conn: ReturnType<typeof fakeConnection>) {
  h.connection = conn;
  return render(withSession(<NexusApp sectionId="approvals" navigate={() => {}} askContext={() => {}} />));
}

async function raiseLimit() {
  fireEvent.click(await screen.findByText("More model calls needed"));
  fireEvent.change(screen.getByLabelText("New total model calls"), { target: { value: "20" } });
  await readTheBar();
  fireEvent.click(screen.getByRole("button", { name: "Raise limit" }));
}

describe("an approved budget whose work did not resume", () => {
  it("keeps an exact retry after the pending row disappears, including a failed retry", async () => {
    const { conn } = connection();
    conn.query.decideApproval.mockResolvedValueOnce(receipt(false, { resumeError: "The run write was unavailable." }))
      .mockRejectedValueOnce(new Error("Connection interrupted"))
      .mockResolvedValueOnce(receipt(true));
    mount(conn);
    await raiseLimit();
    expect(await screen.findByRole("button", { name: "Resume work" })).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toBe("The run write was unavailable.");
    await act(async () => { conn.subscriptions.emit("v1:work:approval", budget(), "NODE_DELETED"); });
    expect(screen.getByRole("button", { name: "Resume work" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Raise limit" })).toBeNull();
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Resume work" }));
    expect(await screen.findByText("Connection interrupted")).toBeTruthy();
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Resume work" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Resume work" })).toBeNull());
    expect(conn.query.decideApproval.mock.calls.map(call => call[0])).toEqual(Array(3).fill({
      approvalId: "budget", decision: "approved", answer: { newLimit: 20 },
    }));
  });

  it("does not turn a newer draft into the retry's approved limit", async () => {
    const { conn } = connection();
    let finish!: (value: ReturnType<typeof receipt>) => void;
    conn.query.decideApproval.mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
    conn.query.decideApproval.mockResolvedValueOnce(receipt(true));
    mount(conn);
    await raiseLimit();
    fireEvent.change(screen.getByLabelText("New total model calls"), { target: { value: "35" } });
    await act(async () => { finish(receipt(false)); });
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Resume work" }));
    await waitFor(() => expect(conn.query.decideApproval).toHaveBeenCalledTimes(2));
    expect(conn.query.decideApproval.mock.calls[1]?.[0]).toEqual({ approvalId: "budget", decision: "approved", answer: { newLimit: 20 } });
  });

  it("recovers a lost decision response when the live feed removed the row first", async () => {
    const { conn, readApproval } = connection();
    let fail!: (reason: Error) => void;
    conn.query.decideApproval.mockImplementationOnce(() => new Promise((_resolve, reject) => { fail = reject; }));
    mount(conn);
    await raiseLimit();
    await act(async () => { conn.subscriptions.emit("v1:work:approval", budget(), "NODE_DELETED"); });
    expect(readApproval).not.toHaveBeenCalled();
    await act(async () => { fail(new Error("Response lost")); });
    expect(await screen.findByRole("button", { name: "Resume work" })).toBeTruthy();
    expect(readApproval).toHaveBeenCalled();
    expect(conn.query.decideApproval).toHaveBeenCalledTimes(1);
  });

  it("restores the exact approved receipt from the still-waiting run after reopening", async () => {
    const { conn, readApproval, readRun } = connection(false);
    conn.query.decideApproval.mockResolvedValue(receipt(true));
    mount(conn);
    fireEvent.click(await screen.findByText("Update the requested record"));
    expect(screen.queryByRole("button", { name: "Raise limit" })).toBeNull();
    fireEvent.click(await screen.findByRole("button", { name: "Resume work" }));
    await waitFor(() => expect(conn.query.decideApproval).toHaveBeenCalledWith({ approvalId: "budget", decision: "approved", answer: { newLimit: 20 } }));
    expect(readApproval.mock.calls[0]?.[0]).toEqual({ approvalId: "budget" });
    expect(readRun.mock.calls[0]?.[0]).toEqual({ runId: "run" });
  });

  it.each(["rejected", "legacy", "moved", "cancelled"])("does not offer recovery for a %s receipt or run", async scenario => {
    const { conn, readApproval, readRun } = connection(false);
    if (scenario === "rejected") readApproval.mockResolvedValue(rowsResult([{ ...approved(), decision: "rejected" }]));
    if (scenario === "legacy") readApproval.mockResolvedValue(rowsResult([{ ...approved(), subject: { ceiling: "modelCalls" } }]));
    if (scenario === "moved") readRun.mockResolvedValue(rowsResult([{ ...parked(), waitingOn: { kind: "approval", subject: "other" } }]));
    if (scenario === "cancelled") readRun.mockResolvedValue(rowsResult([{ ...parked(), cancelRequested: true }]));
    mount(conn);
    await waitFor(() => expect(readApproval).toHaveBeenCalled());
    await act(async () => {});
    expect(screen.queryByRole("button", { name: "Resume work" })).toBeNull();
    expect(screen.queryByLabelText("Work ready to resume")).toBeNull();
    expect(conn.query.decideApproval).not.toHaveBeenCalled();
  });

  it.each(["resumed", "declined", "missing"])("does not offer resume after a %s approval reply", async scenario => {
    const { conn } = connection();
    conn.query.decideApproval.mockResolvedValue(scenario === "missing" ? rowsResult([]) : receipt(scenario === "resumed", scenario === "declined" ? { decision: "rejected" } : {}));
    mount(conn);
    await raiseLimit();
    await waitFor(() => expect(conn.query.decideApproval).toHaveBeenCalled());
    await act(async () => {});
    expect(screen.queryByRole("button", { name: "Resume work" })).toBeNull();
    if (scenario !== "resumed") expect(screen.getByRole("alert").textContent).toContain("did not confirm this limit increase");
  });

  it.each(["finished", "cancelled", "moved"])("removes a held retry when the live run is %s", async scenario => {
    const { conn } = connection();
    conn.query.decideApproval.mockResolvedValue(receipt(false));
    mount(conn);
    await raiseLimit();
    expect(await screen.findByRole("button", { name: "Resume work" })).toBeTruthy();
    const changed = { ...parked(), ...(scenario === "finished" ? { status: "succeeded" }
      : scenario === "cancelled" ? { cancelRequested: true } : { waitingOn: { kind: "approval", subject: "other" } }) };
    await act(async () => { conn.subscriptions.emit("v1:work:run", changed, "NODE_UPDATED"); });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Resume work" })).toBeNull());
    expect(screen.queryByRole("button", { name: "Raise limit" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Back to Approvals" }));
    expect(screen.queryByLabelText("Work ready to resume")).toBeNull();
  });

  it("bounds reload recovery to one owner-read chain at a time", async () => {
    const conn = fakeConnection({ approvals: [], runs: [parked(), { ...parked(), id: "run2", waitingOn: { kind: "approval", subject: "budget2" } }] });
    let finish!: (value: ReturnType<typeof rowsResult>) => void;
    const readApproval = vi.fn((_args: Record<string, unknown>) => new Promise<ReturnType<typeof rowsResult>>(resolve => { finish = resolve; }));
    Object.assign(conn.query, { workApprovalForOwner: readApproval });
    const mounted = mount(conn);
    await waitFor(() => expect(readApproval).toHaveBeenCalledTimes(1));
    await act(async () => { finish(rowsResult([])); });
    await waitFor(() => expect(readApproval).toHaveBeenCalledTimes(2));
    mounted.unmount();
    await act(async () => { finish(rowsResult([])); });
    expect(readApproval).toHaveBeenCalledTimes(2);
  });

  it("does not dispatch a second decision while the same exact retry is in flight", async () => {
    const { conn } = connection();
    h.connection = conn;
    let finish!: (value: ReturnType<typeof receipt>) => void;
    conn.query.decideApproval.mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
    const { result } = renderHook(() => useDecideApproval());
    let first!: Promise<boolean>, second!: Promise<boolean>;
    act(() => {
      first = result.current.decide("budget", "approved", { newLimit: 20 }, undefined, true);
      second = result.current.decide("budget", "approved", { newLimit: 20 }, undefined, true);
    });
    expect(conn.query.decideApproval).toHaveBeenCalledTimes(1);
    await act(async () => { finish(receipt(false)); await first; });
    expect(await second).toBe(false);
    expect(result.current.approvalResumes.budget?.newLimit).toBe(20);
  });
});

describe("computer permission recovery", () => {
  const permission = () => approvalRow({ id: "permission", runId: "run", kind: "scopeElevation", decision: "approved", artifactHash: "reviewed", subject: { computerPermission: { version: 1 } }, answer: { permissionMode: "always" } });
  it("recovers saved directory permission after reopening without selecting a new mode", async () => {
    const conn = fakeConnection();
    Object.assign(conn.query, {
      workApprovalForOwner: vi.fn(async () => rowsResult([permission()])),
      workRunForOwner: vi.fn(async () => rowsResult([runRow({ id: "run", status: "waiting", waitingOn: { kind: "approval", subject: "permission" } })])),
    });
    h.connection = conn;
    const { result } = renderHook(() => useDecideApproval());
    await act(async () => { await result.current.readApprovalResume("permission"); });
    expect(result.current.approvalResumes.permission?.permissionAnswer).toEqual({ permissionMode: "always", permissionHash: "reviewed" });
    conn.query.decideApproval.mockResolvedValue(builtinReply("decideApproval", [{ approvalId: "permission", decision: "approved", runId: "run", runResumed: true }]));
    await act(async () => { await result.current.decide("permission", "approved", result.current.approvalResumes.permission!.permissionAnswer); });
    expect(result.current.approvalResumes.permission).toBeUndefined();
  });
  it("keeps a failed continuation available and distinguishes a review from approval", async () => {
    const conn = fakeConnection(); h.connection = conn;
    conn.query.decideApproval.mockResolvedValueOnce(builtinReply("decideApproval", [{ approvalId: "permission", permissionState: "review", message: "Review again", runResumed: false }]))
      .mockResolvedValueOnce(builtinReply("decideApproval", [{ approvalId: "permission", decision: "approved", runId: "run", runResumed: false, resumeError: "Reconnect to continue" }]));
    const { result } = renderHook(() => useDecideApproval());
    await act(async () => { expect(await result.current.decide("permission", "approved", { permissionMode: "once" })).toBe(false); });
    expect(result.current.permissionReviews.permission).toBe("review");
    expect(result.current.approvalResumes.permission).toBeUndefined();
    await act(async () => { await result.current.refreshPermission("permission"); });
    expect(conn.query.decideApproval).toHaveBeenCalledTimes(1);
    expect(result.current.permissionReviews.permission).toBeUndefined();
    await act(async () => { expect(await result.current.decide("permission", "approved", { permissionMode: "once" })).toBe(false); });
    expect(result.current.approvalResumes.permission?.permissionAnswer).toEqual({ permissionMode: "once" });
    expect(result.current.error).toBe("Reconnect to continue");
  });
});
