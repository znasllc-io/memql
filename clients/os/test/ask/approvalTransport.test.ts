import { expect, it, vi } from "vitest";
import type { Dispatcher } from "@znasllc-io/memql-sdk-core/client";
import { SdkAskTransport } from "../../src/ask/sdkTransport";

function setup(payload: Record<string, unknown>) {
  // Builtins return an id-keyed node envelope, not a flat query row.
  const sendAndWait = vi.fn(async (_request: unknown) => ({ queryResult: { result: { data: [{ receipt: { id: "receipt", concept: "integration:work:result", type: "object", createdAt: "2026-10-12T20:00:00Z", payload } }] } } }));
  const stream = vi.fn();
  return { sendAndWait, stream, transport: new SdkAskTransport(() => ({ sendAndWait } as unknown as Dispatcher), stream) };
}

it("opens the owned approval through the generated builtin and preserves its recovery receipt", async () => {
  const context = { conversationId: "original", approval: { id: "approval", runId: "parent", kind: "feedback", question: "How should this continue?", status: "answered" }, canSendGuidance: false, canAskAdvisor: true, guidanceToResume: "Read the smaller directory first." };
  const { transport, sendAndWait, stream } = setup(context);
  expect(await transport.discussApproval("approval")).toMatchObject(context);
  expect(sendAndWait).toHaveBeenCalledWith(expect.objectContaining({ executeQuery: expect.objectContaining({ query: 'builtin askApprovalDiscussion(approvalId: "approval")' }) }), undefined);
  expect(stream).not.toHaveBeenCalled();
});

it("sends an explicitly marked investigation without the executable Ask stream", async () => {
  const receipt = { conversationId: "original", turnId: "turn", runId: "child", status: "queued" };
  const { transport, sendAndWait, stream } = setup(receipt);
  expect(await transport.approvalMessage({ approvalId: "approval", conversationId: "original", requestId: "same-request", prompt: "Why did this fail?", advisor: true })).toMatchObject(receipt);
  expect(sendAndWait).toHaveBeenCalledWith(expect.objectContaining({ executeQuery: expect.objectContaining({ query: 'builtin askApprovalMessage(approvalId: "approval", conversationId: "original", requestId: "same-request", prompt: "Why did this fail?", advisor: true)' }) }), undefined);
  expect(stream).not.toHaveBeenCalled();
});

it("replays only the exact guidance answer and treats resume false as an incomplete continuation", async () => {
  const { transport, sendAndWait } = setup({ runResumed: false, resumeError: "Planner unavailable" });
  expect(await transport.approvalGuidance("approval", 'Use "read only".')).toEqual({ runResumed: false, resumeError: "Planner unavailable" });
  const request = sendAndWait.mock.calls[0]?.[0] as { executeQuery: { query: string } };
  expect(request.executeQuery.query).toBe('builtin decideApproval(approvalId: "approval", decision: "answered", answer: {feedback: "Use \\"read only\\"."})');
  expect(request.executeQuery.query).not.toContain("permissionMode");
});
