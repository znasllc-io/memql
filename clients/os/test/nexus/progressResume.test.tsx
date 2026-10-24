import { act, renderHook } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection, bridgePathFor: (base: string) => base + "_memql/ws", osBridgePath: "/_memql/ws" }));
const { useDecideApproval } = await import("../../src/apps/nexus/actions");
const { approvalRow, builtinReply, fakeConnection, rowsResult, runRow } = await import("./harness");

it("reopens exact saved progress feedback and resumes without a numeric limit", async () => {
 const conn=fakeConnection(); h.connection=conn;
 Object.assign(conn.query,{
 workApprovalForOwner:vi.fn(async()=>rowsResult([approvalRow({id:"progress",runId:"run",kind:"feedback",decision:"answered",answer:{value:"retry"},subject:{progressLimit:true,resumeKind:"checkpoint",continuationId:"saved"}})])),
 workRunForOwner:vi.fn(async()=>rowsResult([runRow({id:"run",status:"waiting",waitingOn:{kind:"approval",subject:"progress"}})])),
 });
 const {result}=renderHook(()=>useDecideApproval());
 await act(async()=>{await result.current.readApprovalResume("progress")});
 expect(result.current.approvalResumes.progress?.feedbackAnswer).toEqual({value:"retry"});
 conn.query.decideApproval.mockResolvedValue(builtinReply("decideApproval",[{approvalId:"progress",decision:"answered",runId:"run",runResumed:true}]));
 await act(async()=>{await result.current.decide("progress","answered",result.current.approvalResumes.progress!.feedbackAnswer,undefined,true)});
 expect(conn.query.decideApproval).toHaveBeenCalledWith({approvalId:"progress",decision:"answered",answer:{value:"retry"}});
 expect(result.current.approvalResumes.progress).toBeUndefined();
});
it("keeps a failed continuation and never sends the same answer twice in flight", async()=>{
 const conn=fakeConnection();h.connection=conn;
 let finish!:(value:ReturnType<typeof builtinReply>)=>void;
 conn.query.decideApproval.mockImplementationOnce(()=>new Promise(resolve=>{finish=resolve}));
 const {result}=renderHook(()=>useDecideApproval());
 let first!:Promise<boolean>,second!:Promise<boolean>;
 act(()=>{first=result.current.decide("progress","answered",{value:"retry"},undefined,true);second=result.current.decide("progress","answered",{value:"retry"},undefined,true)});
 expect(conn.query.decideApproval).toHaveBeenCalledTimes(1);
 await act(async()=>{finish(builtinReply("decideApproval",[{approvalId:"progress",decision:"answered",runId:"run",runResumed:false,resumeError:"Disconnected"}]));await first});
 expect(await second).toBe(false);expect(result.current.approvalResumes.progress?.feedbackAnswer).toEqual({value:"retry"});
 expect(result.current.error).toBe("Disconnected");
});
it.each(["cancelled","moved","running","legacy"])("does not resurrect %s work", async(mode)=>{
 const conn=fakeConnection();h.connection=conn;
 Object.assign(conn.query,{
 workApprovalForOwner:vi.fn(async()=>rowsResult([approvalRow({id:"progress",runId:"run",kind:mode==="legacy"?"budget":"feedback",decision:mode==="legacy"?"approved":"answered",answer:mode==="legacy"?{newLimit:20}:{value:"retry"},subject:{progressLimit:true,resumeKind:"checkpoint",continuationId:"saved"}})])),
 workRunForOwner:vi.fn(async()=>rowsResult([runRow({id:"run",status:mode==="running"?"running":"waiting",cancelRequested:mode==="cancelled",waitingOn:{kind:"approval",subject:mode==="moved"?"other":"progress"}})])),
 });
 const {result}=renderHook(()=>useDecideApproval());await act(async()=>{await result.current.readApprovalResume("progress")});
 expect(result.current.approvalResumes.progress).toBeUndefined();expect(conn.query.decideApproval).not.toHaveBeenCalled();
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

it("restores a saved stop and clears it only after the cluster confirms stopping", async () => {
  const conn = fakeConnection(); h.connection = conn;
  Object.assign(conn.query, {
    workApprovalForOwner: vi.fn(async () => rowsResult([approvalRow({ id: "stop", runId: "run", kind: "feedback", decision: "answered", subject: { progressLimit: true }, answer: { value: "abandon" } })])),
    workRunForOwner: vi.fn(async () => rowsResult([runRow({ id: "run", status: "waiting", waitingOn: { kind: "approval", subject: "stop" } })])),
  });
  const { result } = renderHook(() => useDecideApproval());
  await act(async () => { await result.current.readApprovalResume("stop"); });
  expect(result.current.approvalResumes.stop?.feedbackAnswer).toEqual({ value: "abandon" });
  conn.query.decideApproval.mockResolvedValue(builtinReply("decideApproval", [{ approvalId: "stop", runId: "run", decision: "answered", runResumed: false, runStopped: true }]));
  await act(async () => { expect(await result.current.decide("stop", "answered", { value: "abandon" }, undefined, true)).toBe(true); });
  expect(result.current.approvalResumes.stop).toBeUndefined();
});

it.each(["ready", "cancelled", "different-answer", "different-failure", "running"])("reporting recovery restores only an unchanged abandoned answer: %s", async (mode) => {
  const conn = fakeConnection(); h.connection = conn;
  Object.assign(conn.query, {
    workApprovalForOwner: vi.fn(async () => rowsResult([approvalRow({ id: "format", runId: "run", kind: "feedback", decision: "answered", answer: { value: "retry" }, subject: { errorMessage: mode === "different-failure" ? "unrelated failure" : "work_outcome_recovery_v1: retained result" } })])),
    workRunForOwner: vi.fn(async () => rowsResult([runRow({ id: "run", status: mode === "running" ? "running" : "abandoned", errorCode: "run_abandoned", humanResumeId: mode === "different-answer" ? "new-answer" : "format", cancelRequested: mode === "cancelled" })])),
  });
  const { result } = renderHook(() => useDecideApproval());
  await act(async () => { await result.current.readApprovalResume("format"); });
  expect(conn.query.decideApproval).not.toHaveBeenCalled();
  if (mode !== "ready") {
    expect(result.current.approvalResumes.format).toBeUndefined();
    return;
  }
  expect(result.current.approvalResumes.format).toMatchObject({ outcomeRecovery: true, feedbackAnswer: { value: "retry" } });
  conn.query.decideApproval.mockResolvedValue(builtinReply("decideApproval", [{ approvalId: "format", runId: "run", decision: "answered", runResumed: true }]));
  await act(async () => { await result.current.decide("format", "answered", result.current.approvalResumes.format!.feedbackAnswer, undefined, true); });
  expect(conn.query.decideApproval).toHaveBeenCalledWith({ approvalId: "format", decision: "answered", answer: { value: "retry" } });
});
