import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AskSurface } from "../../src/ask/AskSurface";
import type { AskTransport } from "../../src/ask/askController";
import { ConversationSession, type ApprovalDiscussion, type AskTurn } from "../../src/ask/conversationSession";

const sessions: ConversationSession[] = [];
afterEach(() => { cleanup(); sessions.splice(0).forEach(session => session.dispose()); });
const work: AskTurn = { id: "work", runId: "run", prompt: "Review a change", workTitle: "Review a change", state: "queued", workStatus: "running", background: true, answer: "", startedAt: "2026-10-13T10:00:00Z", activity: [] };
const discussion: ApprovalDiscussion = { conversationId: "chat", approval: { id: "approval", runId: "run", kind: "feedback", question: "How should work continue?", status: "pending" }, canAskAdvisor: true, canSendGuidance: true };
async function setup() {
  const read = vi.fn(async (_id: string): Promise<AskTurn[]> => [work]);
  const approvalMessage = vi.fn<NonNullable<AskTransport["approvalMessage"]>>();
  const transport: AskTransport = {
    ask: vi.fn(() => ({ cancel: vi.fn() })), approvalMessage,
    discussApproval: vi.fn(async () => discussion),
    conversations: { read, list: vi.fn(async () => []), create: vi.fn(async () => ({ id: "unused", title: "Unused" })) },
  };
  const session = new ConversationSession(transport); sessions.push(session);
  await session.select("chat");
  expect(await session.discussApproval("approval")).toBe(true);
  session.setDraft("My unfinished guidance");
  render(<AskSurface conversation={session} transport={transport} variant="sheet" availability={{ state: "ready", message: "", refresh: vi.fn() }} />);
  return { session, read, approvalMessage, transport };
}

it("clears a recovered snapshot error while discussing without losing draft or scope", async () => {
  const test = await setup();
  test.read.mockRejectedValueOnce(new Error("Snapshot temporarily unavailable"));
  await act(async () => { await test.session.reload(false); });
  expect(screen.getByRole("alert").textContent).toContain("Snapshot temporarily unavailable");
  test.read.mockResolvedValueOnce([{ ...work, workStatus: "waiting", state: "waiting" }]);
  await act(async () => { await test.session.reload(false); });
  expect(screen.queryByRole("alert")).toBeNull();
  expect(screen.getByRole("textbox", { name: "Ask" })).toHaveProperty("value", "My unfinished guidance");
  expect(test.session.getSnapshot().turns[0]).toMatchObject({ id: "work", state: "waiting" });
  expect(test.session.getSnapshot().discussion).toEqual(discussion);
  expect(test.transport.ask).not.toHaveBeenCalled(); expect(test.approvalMessage).not.toHaveBeenCalled();
});

it("preserves an uncertain action error through a failed and recovered snapshot while discussing", async () => {
  const test = await setup();
  test.approvalMessage.mockRejectedValueOnce(new Error("Action acknowledgement lost"));
  await act(async () => { expect(test.session.send("Original submitted message", null)).toBe(true); });
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("Action acknowledgement lost"));
  act(() => test.session.setDraft("A newer unsent draft"));
  const receipt = test.session.getSnapshot().discussionRetry;
  expect(receipt).toMatchObject({ prompt: "Original submitted message" });
  test.read.mockRejectedValueOnce(new Error("Snapshot temporarily unavailable"));
  await act(async () => { await test.session.reload(false); });
  expect(screen.getByRole("alert").textContent).toContain("Action acknowledgement lost");
  await act(async () => { await test.session.reload(false); });
  expect(screen.getByRole("alert").textContent).toContain("Action acknowledgement lost");
  expect(screen.getByRole("textbox", { name: "Ask" })).toHaveProperty("value", "A newer unsent draft");
  expect(test.session.getSnapshot().discussionRetry).toEqual(receipt);
  expect(test.transport.ask).not.toHaveBeenCalled();
  expect(test.approvalMessage).toHaveBeenCalledOnce();
});
