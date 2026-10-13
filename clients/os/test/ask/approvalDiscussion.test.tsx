import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ConversationSession, type ApprovalDiscussion, type AskTurn } from "../../src/ask/conversationSession";
import { AskSurface } from "../../src/ask/AskSurface";

afterEach(cleanup);
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(done => { resolve = done; }); return { promise, resolve }; }
const context: ApprovalDiscussion = { conversationId: "original", approval: { id: "approval", runId: "parent", kind: "planReview", question: "How should this work continue?", status: "pending" }, canAskAdvisor: true, canSendGuidance: true };
function setup() {
  let turns: AskTurn[] = [];
  const transport = {
    ask: vi.fn(() => ({ cancel: vi.fn() })),
    discussApproval: vi.fn(async (_id: string): Promise<ApprovalDiscussion> => context),
    approvalMessage: vi.fn(async (request: { approvalId: string; conversationId: string; requestId: string; prompt: string; advisor?: boolean }) => {
      turns = [...turns, { id: request.requestId, prompt: request.prompt, answer: "", runId: "advice", approvalId: request.approvalId, advisor: request.advisor, background: true, state: "queued", startedAt: "2026-10-12T20:00:00Z", activity: [] }];
      return { conversationId: request.conversationId, turnId: request.requestId, runId: "advice", status: "queued" };
    }),
    approvalGuidance: vi.fn(async (_id: string, _feedback: string) => {
      transport.discussApproval.mockResolvedValue({ ...context, canSendGuidance: false });
      return { runResumed: true, resumeError: "" };
    }),
    conversations: { list: vi.fn(async () => []), create: vi.fn(async () => ({ id: "unused", title: "Unused" })), read: vi.fn(async (_id: string) => turns) },
  };
  const session = new ConversationSession(transport);
  return { transport, session, setTurns: (value: AskTurn[]) => { turns = value; } };
}
function mount(session: ConversationSession, transport: ReturnType<typeof setup>["transport"]) {
  return render(<AskSurface transport={transport} conversation={session} variant="sheet" availability={{ state: "ready", message: "", refresh: vi.fn() }} />);
}

it("opens the original conversation, preserves its draft and issues no execution", async () => {
  const { transport, session } = setup();
  await session.select("original"); session.setDraft("Keep my unfinished note");
  await session.discussApproval("approval");
  expect(session.getSnapshot()).toMatchObject({ selectedId: "original", draft: "Keep my unfinished note", discussion: context });
  expect(transport.discussApproval).toHaveBeenCalledWith("approval");
  expect(transport.conversations.create).not.toHaveBeenCalled(); expect(transport.ask).not.toHaveBeenCalled(); session.dispose();
});

it("ignores a late opening after New conversation and blocks execution during lookup", async () => {
  const { transport, session } = setup(); const pending = deferred<ApprovalDiscussion>();
  transport.discussApproval.mockReturnValue(pending.promise);
  const opening = session.discussApproval("approval");
  expect(session.send("Do it", null)).toBe(false);
  session.newConversation(); pending.resolve(context); await opening;
  expect(session.getSnapshot().selectedId).toBeNull(); expect(session.getSnapshot().discussion).toBeNull();
  expect(transport.ask).not.toHaveBeenCalled(); session.dispose();
});

it("sends discussion through the read-only endpoint and preserves typing as advice arrives", async () => {
  const { transport, session, setTurns } = setup(); await session.discussApproval("approval");
  expect(session.send("What caused this?", null)).toBe(true); session.setDraft("Try a smaller directory next.");
  await waitFor(() => expect(session.getSnapshot().discussionSending).toBe(false));
  expect(transport.approvalMessage).toHaveBeenCalledWith(expect.objectContaining({ approvalId: "approval", conversationId: "original", prompt: "What caused this?", advisor: false }));
  expect(transport.ask).not.toHaveBeenCalled();
  const turn = session.getSnapshot().turns[0]!;
  setTurns([{ ...turn, answer: "The old checkpoint was too small.", state: "done", endedAt: "2026-10-12T20:01:00Z" }]);
  await session.reload();
  expect(session.getSnapshot().draft).toBe("Try a smaller directory next.");
  expect(session.getSnapshot().turns[0]?.answer).toBe("The old checkpoint was too small."); session.dispose();
});

it("reuses the request id after a lost response without replacing a newer draft", async () => {
  const { transport, session } = setup(); await session.discussApproval("approval");
  transport.approvalMessage.mockRejectedValueOnce(new Error("Response lost"));
  session.sendDiscussion("Investigate the failure", true); session.setDraft("My own plan");
  await waitFor(() => expect(session.getSnapshot().discussionSending).toBe(false));
  expect(session.getSnapshot().draft).toBe("My own plan");
  session.sendDiscussion("Investigate the failure", true);
  await waitFor(() => expect(transport.approvalMessage).toHaveBeenCalledTimes(2));
  expect(transport.approvalMessage.mock.calls[0]?.[0].requestId).toBe(transport.approvalMessage.mock.calls[1]?.[0].requestId);
  expect(transport.approvalGuidance).not.toHaveBeenCalled(); session.dispose();
});

it("does not inject a late advice receipt into another conversation", async () => {
  const { transport, session } = setup(); await session.discussApproval("approval");
  const receipt = deferred<{ conversationId: string; turnId: string; runId: string; status: string }>();
  transport.approvalMessage.mockReturnValueOnce(receipt.promise);
  session.sendDiscussion("Investigate", true); await session.select("other");
  receipt.resolve({ conversationId: "original", turnId: "late", runId: "advice", status: "queued" });
  await Promise.resolve(); await Promise.resolve();
  expect(session.getSnapshot().selectedId).toBe("other"); expect(session.getSnapshot().turns).toEqual([]);
  expect(session.getSnapshot().discussion).toBeNull(); session.dispose();
});

it("sends trimmed guidance explicitly and reports recorded without resumed", async () => {
  const { transport, session } = setup(); await session.discussApproval("approval");
  transport.approvalGuidance.mockImplementationOnce(async () => { transport.discussApproval.mockResolvedValue({ ...context, canSendGuidance: false, guidanceToResume: "Read only the repository first." }); return { runResumed: false, resumeError: "The planner is unavailable." }; });
  session.setDraft("  Read only the repository first.  "); await session.sendGuidance();
  expect(transport.approvalGuidance).toHaveBeenCalledExactlyOnceWith("approval", "Read only the repository first.");
  expect(transport.approvalMessage).not.toHaveBeenCalled(); expect(transport.ask).not.toHaveBeenCalled();
  expect(session.getSnapshot().guidanceNotice).toBe("Guidance was recorded, but the work has not resumed. The planner is unavailable.");
  expect(session.getSnapshot().discussion?.canSendGuidance).toBe(false); session.dispose();
});

it("uses Unicode bounds and the server's guidance eligibility", async () => {
  const { transport, session } = setup(); await session.discussApproval("approval");
  session.setDraft("😀".repeat(4001)); expect(await session.sendGuidance()).toBe(false); expect(transport.approvalGuidance).not.toHaveBeenCalled();
  session.setDraft("😀".repeat(4000)); expect(await session.sendGuidance()).toBe(true);
  session.setDraft("Continue"); expect(await session.sendGuidance()).toBe(false); expect(transport.approvalGuidance).toHaveBeenCalledTimes(1); session.dispose();
});

it("keeps discussion, investigation and guidance separate with an editable composer", async () => {
  const { transport, session, setTurns } = setup(); await session.discussApproval("approval"); mount(session, transport);
  const input = screen.getByRole("textbox", { name: "Ask" });
  fireEvent.change(input, { target: { value: "Read only the repository." } });
  expect(screen.getByRole("button", { name: "Send guidance" })).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Ask another agent" }));
  await waitFor(() => expect(transport.approvalMessage).toHaveBeenCalledWith(expect.objectContaining({ advisor: true })));
  fireEvent.change(input, { target: { value: "My revised guidance" } });
  expect((input as HTMLTextAreaElement).readOnly).toBe(false);
  expect(screen.getByText("Advice is being prepared. You can keep writing.")).toBeTruthy();
  const turn = session.getSnapshot().turns[0]!;
  setTurns([{ ...turn, answer: "Inspect a narrower path first.", state: "done", endedAt: "2026-10-12T20:01:00Z" }]);
  await act(async () => session.reload());
  expect(await screen.findByText("Another agent")).toBeTruthy(); expect(screen.getByText("Advice")).toBeTruthy();
  expect((input as HTMLTextAreaElement).value).toBe("My revised guidance");
  expect(screen.queryByRole("button", { name: "Approve" })).toBeNull(); expect(transport.approvalGuidance).not.toHaveBeenCalled(); session.dispose();
});

it("keeps guidance refusal and composed text visible", async () => {
  const { transport, session } = setup(); await session.discussApproval("approval");
  transport.approvalGuidance.mockRejectedValueOnce(new Error("This approval has already been decided.")); mount(session, transport);
  fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "Different approach" } });
  fireEvent.click(screen.getByRole("button", { name: "Send guidance" }));
  expect(await screen.findByRole("alert")).toHaveProperty("textContent", "This approval has already been decided.");
  expect((screen.getByRole("textbox", { name: "Ask" }) as HTMLTextAreaElement).value).toBe("Different approach"); session.dispose();
});


it("retries recorded guidance with the original answer while preserving a new draft", async () => {
  const { transport, session } = setup(); await session.discussApproval("approval");
  const feedback = "Read only the repository first.";
  transport.approvalGuidance.mockImplementationOnce(async () => {
    transport.discussApproval.mockResolvedValue({ ...context, canSendGuidance: false, guidanceToResume: feedback });
    return { runResumed: false, resumeError: "The planner is unavailable." };
  });
  session.setDraft(feedback); await session.sendGuidance(); mount(session, transport);
  fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "A different note, not the saved guidance." } });
  expect(screen.queryByRole("button", { name: "Send guidance" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Retry continuing" }));
  await waitFor(() => expect(transport.approvalGuidance).toHaveBeenCalledTimes(2));
  expect(transport.approvalGuidance.mock.calls[1]).toEqual(["approval", feedback]);
  await waitFor(() => expect(screen.queryByRole("button", { name: "Retry continuing" })).toBeNull());
  expect(session.getSnapshot().draft).toBe("A different note, not the saved guidance.");
  expect(session.getSnapshot().guidanceNotice).toBe("Guidance sent. Work is continuing."); session.dispose();
});

it("restores a saved retry from the server after reopening without enabling a new decision", async () => {
  const { transport, session } = setup();
  transport.discussApproval.mockResolvedValue({ ...context, canSendGuidance: false, guidanceToResume: "Original stored guidance" });
  await session.discussApproval("approval"); session.setDraft("New discussion only");
  expect(await session.sendGuidance()).toBe(false);
  transport.approvalGuidance.mockResolvedValueOnce({ runResumed: false, resumeError: "Still unavailable" });
  expect(await session.retryGuidance()).toBe(true);
  expect(transport.approvalGuidance).toHaveBeenCalledExactlyOnceWith("approval", "Original stored guidance");
  expect(session.getSnapshot().guidanceRetry?.feedback).toBe("Original stored guidance");
  expect(session.getSnapshot().draft).toBe("New discussion only");
  session.leaveDiscussion(); expect(session.getSnapshot().guidanceRetry).toBeNull();
  await session.discussApproval("approval"); expect(session.getSnapshot().guidanceRetry?.feedback).toBe("Original stored guidance"); session.dispose();
});

it("retains exact guidance after an uncertain response, even if the composer is edited", async () => {
  const { transport, session } = setup(); await session.discussApproval("approval");
  transport.approvalGuidance.mockRejectedValueOnce(new Error("Response lost"));
  session.setDraft("Original guidance"); expect(await session.sendGuidance()).toBe(false);
  expect(session.getSnapshot().draft).toBe("Original guidance");
  session.setDraft("New text"); await session.reload();
  expect(await session.sendGuidance()).toBe(false);
  expect(await session.retryGuidance()).toBe(true);
  expect(transport.approvalGuidance.mock.calls).toEqual([["approval", "Original guidance"], ["approval", "Original guidance"]]);
  expect(session.getSnapshot().draft).toBe("New text"); session.dispose();
});

it("does not restore stale eligibility from a read started before guidance was recorded", async () => {
  const { transport, session } = setup(); await session.discussApproval("approval");
  const stale = deferred<ApprovalDiscussion>();
  transport.discussApproval.mockReturnValueOnce(stale.promise);
  const read = session.reload(); await Promise.resolve();
  session.setDraft("Original guidance"); await session.sendGuidance();
  stale.resolve(context); await read;
  expect(session.getSnapshot().discussion?.canSendGuidance).toBe(false); session.dispose();
});

it("opens discussion in place when the same conversation was showing its activity", async () => {
  const { transport, session } = setup(); await session.select("original"); session.setDraft("My unfinished plan"); mount(session, transport);
  fireEvent.click(screen.getByRole("button", { name: "Activity" }));
  expect(screen.queryByRole("button", { name: "Ask another agent" })).toBeNull();
  await act(async () => session.discussApproval("approval"));
  expect(screen.getByRole("button", { name: "Ask another agent" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Activity" })).toBeTruthy();
  expect((screen.getByRole("textbox", { name: "Ask" }) as HTMLTextAreaElement).value).toBe("My unfinished plan"); session.dispose();
});


it("keeps a resolved request readable and its draft editable without offering new discussion", async () => {
  const { transport, session } = setup();
  transport.discussApproval.mockResolvedValue({ ...context, approval: { ...context.approval, status: "answered" }, canSendGuidance: false, canAskAdvisor: false });
  await session.discussApproval("approval"); mount(session, transport);
  fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "Keep my next thought" } });
  expect(screen.getByRole("button", { name: "Send discussion message" })).toHaveProperty("disabled", true);
  expect(screen.getByText("This request is no longer waiting. Return to conversation to continue.")).toBeTruthy();
  expect(session.send("Try again", null)).toBe(false);
  expect(session.sendDiscussion("Another investigation", true)).toBe(false);
  expect(transport.approvalMessage).not.toHaveBeenCalled(); expect(transport.ask).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Return to conversation" }));
  expect(session.getSnapshot().draft).toBe("Keep my next thought");
  expect(session.getSnapshot().discussion).toBeNull(); session.dispose();
});

it("recovers an uncertain investigation after the request resolves without consuming a new draft", async () => {
  const { transport, session } = setup(); await session.discussApproval("approval");
  transport.approvalMessage.mockRejectedValueOnce(new Error("Response lost"));
  session.sendDiscussion("Investigate the checkpoint", true);
  await waitFor(() => expect(session.getSnapshot().discussionSending).toBe(false));
  transport.discussApproval.mockResolvedValue({ ...context, approval: { ...context.approval, status: "answered" }, canSendGuidance: false, canAskAdvisor: false });
  await session.reload(); session.setDraft("Keep this new idea"); mount(session, transport);
  expect(session.send("New unaccepted message", null)).toBe(false);
  fireEvent.click(screen.getByRole("button", { name: "Retry investigation" }));
  await waitFor(() => expect(session.getSnapshot().discussionSending).toBe(false));
  expect(transport.approvalMessage).toHaveBeenCalledTimes(2);
  expect(transport.approvalMessage.mock.calls[1]?.[0]).toEqual(transport.approvalMessage.mock.calls[0]?.[0]);
  expect(session.getSnapshot().draft).toBe("Keep this new idea");
  expect(screen.queryByRole("button", { name: "Retry investigation" })).toBeNull(); session.dispose();
});
