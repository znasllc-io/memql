import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AskSurface } from "../../src/ask/AskSurface";
import type { AskTransport } from "../../src/ask/askController";
import { ConversationSession, type ApprovalDiscussion, type AskTurn } from "../../src/ask/conversationSession";

const sessions: ConversationSession[] = [];
afterEach(() => { cleanup(); sessions.splice(0).forEach(session => session.dispose()); });
const work: AskTurn = { id: "work", runId: "run", prompt: "Review a change", workTitle: "Review a change", canGuideWork: true, guidanceEpoch: "epoch", state: "queued", workStatus: "running", background: true, answer: "", startedAt: "2026-10-13T10:00:00Z", activity: [] };
const discussion: ApprovalDiscussion = { conversationId: "chat", approval: { id: "approval", runId: "run", kind: "feedback", question: "How should work continue?", status: "pending" }, canAskAdvisor: true, canSendGuidance: true };
async function setup(mode: "guiding" | "discussing") {
  const read = vi.fn(async (_id: string): Promise<AskTurn[]> => [work]);
  const guideWork = vi.fn<NonNullable<AskTransport["guideWork"]>>();
  const approvalMessage = vi.fn<NonNullable<AskTransport["approvalMessage"]>>();
  const transport: AskTransport = {
    ask: vi.fn(() => ({ cancel: vi.fn() })), guideWork, approvalMessage,
    discussApproval: vi.fn(async () => discussion),
    conversations: { read, list: vi.fn(async () => []), create: vi.fn(async () => ({ id: "unused", title: "Unused" })) },
  };
  const session = new ConversationSession(transport); sessions.push(session);
  await session.select("chat");
  if (mode === "guiding") expect(session.beginWorkGuidance("work")).toBe(true);
  else expect(await session.discussApproval("approval")).toBe(true);
  session.setDraft("My unfinished guidance");
  render(<AskSurface conversation={session} transport={transport} variant="sheet" availability={{ state: "ready", message: "", refresh: vi.fn() }} />);
  return { session, read, guideWork, approvalMessage, transport };
}

it.each(["guiding", "discussing"] as const)("clears a recovered snapshot error while %s without losing draft or scope", async mode => {
  const test = await setup(mode);
  test.read.mockRejectedValueOnce(new Error("Snapshot temporarily unavailable"));
  await act(async () => { await test.session.reload(false); });
  expect(screen.getByRole("alert").textContent).toContain("Snapshot temporarily unavailable");
  test.read.mockResolvedValueOnce([{ ...work, workStatus: "waiting", state: "waiting" }]);
  await act(async () => { await test.session.reload(false); });
  expect(screen.queryByRole("alert")).toBeNull();
  expect(screen.getByRole("textbox", { name: "Ask" })).toHaveProperty("value", "My unfinished guidance");
  expect(test.session.getSnapshot().turns[0]).toMatchObject({ id: "work", state: "waiting" });
  if (mode === "guiding") expect(test.session.getSnapshot().workGuidance).toMatchObject({ runId: "run", targetEpoch: "epoch" });
  else expect(test.session.getSnapshot().discussion).toEqual(discussion);
  expect(test.transport.ask).not.toHaveBeenCalled(); expect(test.guideWork).not.toHaveBeenCalled(); expect(test.approvalMessage).not.toHaveBeenCalled();
});

it.each(["guiding", "discussing"] as const)("preserves an uncertain action error through a failed and recovered snapshot while %s", async mode => {
  const test = await setup(mode);
  test.guideWork.mockRejectedValueOnce(new Error("Action acknowledgement lost"));
  test.approvalMessage.mockRejectedValueOnce(new Error("Action acknowledgement lost"));
  await act(async () => { expect(test.session.send("Original submitted message", null)).toBe(true); });
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("Action acknowledgement lost"));
  act(() => test.session.setDraft("A newer unsent draft"));
  const receipt = mode === "guiding" ? test.session.getSnapshot().workGuidanceRetry : test.session.getSnapshot().discussionRetry;
  expect(receipt).toMatchObject({ prompt: "Original submitted message" });
  test.read.mockRejectedValueOnce(new Error("Snapshot temporarily unavailable"));
  await act(async () => { await test.session.reload(false); });
  expect(screen.getByRole("alert").textContent).toContain("Action acknowledgement lost");
  await act(async () => { await test.session.reload(false); });
  expect(screen.getByRole("alert").textContent).toContain("Action acknowledgement lost");
  expect(screen.getByRole("textbox", { name: "Ask" })).toHaveProperty("value", "A newer unsent draft");
  expect(mode === "guiding" ? test.session.getSnapshot().workGuidanceRetry : test.session.getSnapshot().discussionRetry).toEqual(receipt);
  expect(test.transport.ask).not.toHaveBeenCalled();
  expect(mode === "guiding" ? test.guideWork : test.approvalMessage).toHaveBeenCalledOnce();
});
