import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ApprovalGuidanceReceipt } from "../../src/ask/askController";
import { ConversationSession, type ApprovalDiscussion } from "../../src/ask/conversationSession";
import { AskSurface } from "../../src/ask/AskSurface";

afterEach(cleanup);
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(done => { resolve = done; }); return { promise, resolve }; }
function setup(savedGuidance = "") {
  let discussion: ApprovalDiscussion = { conversationId: "original", approval: { id: "budget", runId: "parent", kind: "budget", question: "This work reached its limit.", status: "pending" }, canAskAdvisor: true, canSendGuidance: true, guidanceMode: "deferred", savedGuidance };
  const transport = {
    ask: vi.fn(() => ({ cancel: vi.fn() })),
    discussApproval: vi.fn(async (_id: string): Promise<ApprovalDiscussion> => discussion),
    approvalMessage: vi.fn(async () => ({ conversationId: "original", turnId: "advice", runId: "advice-run", status: "queued" })),
    approvalGuidance: vi.fn(async (_id: string, feedback: string): Promise<ApprovalGuidanceReceipt> => {
      discussion = { ...discussion, savedGuidance: feedback };
      return { runResumed: false, guidanceSaved: true, savedGuidance: feedback };
    }),
    conversations: { list: vi.fn(async () => []), create: vi.fn(async () => ({ id: "unused", title: "Unused" })), read: vi.fn(async () => []) },
  };
  const session = new ConversationSession(transport);
  return { transport, session, update: (patch: Partial<ApprovalDiscussion>) => { discussion = { ...discussion, ...patch }; } };
}
function mount(session: ConversationSession, transport: ReturnType<typeof setup>["transport"]) {
  return render(<AskSurface transport={transport} conversation={session} variant="sheet" availability={{ state: "ready", message: "", refresh: vi.fn() }} />);
}

it("saves guidance explicitly without resuming or deciding the budget, and can replace it", async () => {
  const { transport, session } = setup(); await session.discussApproval("budget"); mount(session, transport);
  const input = screen.getByRole("textbox", { name: "Ask" });
  fireEvent.change(input, { target: { value: "  Use the known identifier and verify the change.  " } });
  expect(screen.queryByRole("button", { name: "Send guidance" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Save guidance" }));
  await waitFor(() => expect(session.getSnapshot().guidanceSending).toBe(false));
  expect(transport.approvalGuidance).toHaveBeenCalledExactlyOnceWith("budget", "Use the known identifier and verify the change.");
  expect(session.getSnapshot().discussion).toMatchObject({ approval: { status: "pending" }, canSendGuidance: true, savedGuidance: "Use the known identifier and verify the change." });
  expect(screen.getByText("Guidance saved. Work stays paused until you raise the limit.")).toBeTruthy();
  expect(screen.getByText("Saved guidance")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Retry continuing" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Retry saving" })).toBeNull();
  expect(input).toHaveProperty("value", "");
  expect(transport.approvalMessage).not.toHaveBeenCalled(); expect(transport.ask).not.toHaveBeenCalled();
  fireEvent.change(input, { target: { value: "Verify the current record first." } });
  fireEvent.click(screen.getByRole("button", { name: "Save guidance" }));
  await waitFor(() => expect(session.getSnapshot().discussion?.savedGuidance).toBe("Verify the current record first."));
  expect(session.getSnapshot().guidanceRetry).toBeNull(); session.dispose();
});

it("reopens persisted budget guidance as saved, with no continuation retry", async () => {
  const { transport, session } = setup("Use the existing result.");
  await session.discussApproval("budget"); mount(session, transport);
  expect(screen.getByText("Use the existing result.")).toBeTruthy();
  expect(screen.getByText("Guidance saved. Work stays paused until you raise the limit.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: /Retry/ })).toBeNull();
  expect(transport.approvalGuidance).not.toHaveBeenCalled(); expect(transport.ask).not.toHaveBeenCalled(); session.dispose();
});

it("prevents duplicate saves and discussion sends while saving without losing new writing", async () => {
  const { transport, session, update } = setup(); await session.discussApproval("budget");
  const saved = deferred<ApprovalGuidanceReceipt>(); transport.approvalGuidance.mockReturnValueOnce(saved.promise);
  session.setDraft("Original guidance"); mount(session, transport);
  fireEvent.click(screen.getByRole("button", { name: "Save guidance" }));
  expect(screen.getByText("Saving guidance. Work remains paused.")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Save guidance" })).toHaveProperty("disabled", true);
  fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "New writing" } });
  expect(screen.getByRole("button", { name: "Send discussion message" })).toHaveProperty("disabled", true);
  expect(await session.sendGuidance()).toBe(false);
  expect(transport.approvalGuidance).toHaveBeenCalledExactlyOnceWith("budget", "Original guidance");
  await act(async () => { update({ savedGuidance: "Original guidance" }); saved.resolve({ runResumed: false, guidanceSaved: true, savedGuidance: "Original guidance" }); });
  await waitFor(() => expect(session.getSnapshot().guidanceSending).toBe(false));
  expect(session.getSnapshot().draft).toBe("New writing");
  expect(session.getSnapshot().discussion?.savedGuidance).toBe("Original guidance");
  expect(transport.approvalMessage).not.toHaveBeenCalled(); session.dispose();
});

it("keeps ordinary budget discussion advisory until Save guidance is pressed", async () => {
  const { transport, session } = setup(); await session.discussApproval("budget"); mount(session, transport);
  fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "Why is the search repeating?" } });
  fireEvent.click(screen.getByRole("button", { name: "Send discussion message" }));
  await waitFor(() => expect(transport.approvalMessage).toHaveBeenCalledOnce());
  expect(transport.approvalGuidance).not.toHaveBeenCalled(); expect(transport.ask).not.toHaveBeenCalled();
  expect(session.getSnapshot().discussion?.savedGuidance).toBe(""); session.dispose();
});

it("retries an uncertain save with its original text and preserves newer writing", async () => {
  const { transport, session } = setup(); await session.discussApproval("budget");
  transport.approvalGuidance.mockRejectedValueOnce(new Error("Response lost"));
  session.setDraft("Original guidance"); expect(await session.sendGuidance()).toBe(false);
  session.setDraft("New discussion"); mount(session, transport);
  expect(screen.queryByRole("button", { name: "Retry continuing" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Retry saving" }));
  await waitFor(() => expect(session.getSnapshot().guidanceSending).toBe(false));
  expect(transport.approvalGuidance.mock.calls).toEqual([["budget", "Original guidance"], ["budget", "Original guidance"]]);
  expect(session.getSnapshot().draft).toBe("New discussion");
  expect(session.getSnapshot().guidanceRetry).toBeNull();
  expect(session.getSnapshot().discussion?.savedGuidance).toBe("Original guidance"); session.dispose();
});

it("resolves a lost save response from the exact durable text without submitting again", async () => {
  const { transport, session, update } = setup(); await session.discussApproval("budget");
  transport.approvalGuidance.mockImplementationOnce(async (_id, feedback) => { update({ savedGuidance: feedback }); throw new Error("Response lost"); });
  session.setDraft("Original guidance"); await session.sendGuidance(); session.setDraft("New text");
  await session.reload(); mount(session, transport);
  expect(session.getSnapshot().guidanceRetry).toBeNull();
  expect(session.getSnapshot().error).toBe("");
  expect(session.getSnapshot().draft).toBe("New text");
  expect(screen.queryByRole("button", { name: /Retry/ })).toBeNull();
  expect(transport.approvalGuidance).toHaveBeenCalledOnce(); session.dispose();
});

it.each([{ runResumed: false }, { runResumed: true, guidanceSaved: true, savedGuidance: "Original" }, { runResumed: false, guidanceSaved: true, savedGuidance: "Different" }])("does not claim a save from a mismatched receipt %j", async receipt => {
  const { transport, session } = setup(); await session.discussApproval("budget");
  transport.approvalGuidance.mockResolvedValueOnce(receipt);
  session.setDraft("Original"); expect(await session.sendGuidance()).toBe(false);
  expect(session.getSnapshot().discussion?.savedGuidance).toBe("");
  expect(session.getSnapshot().draft).toBe("Original");
  expect(session.getSnapshot().guidanceRetry?.feedback).toBe("Original"); session.dispose();
});

it("preserves a new draft and ignores a stale read while a budget save completes", async () => {
  const { transport, session, update } = setup(); await session.discussApproval("budget");
  const stale = deferred<ApprovalDiscussion>(); transport.discussApproval.mockReturnValueOnce(stale.promise);
  const read = session.reload(); await Promise.resolve();
  const saved = deferred<ApprovalGuidanceReceipt>(); transport.approvalGuidance.mockReturnValueOnce(saved.promise);
  session.setDraft("Original guidance"); const write = session.sendGuidance(); session.setDraft("New writing");
  await act(async () => { update({ savedGuidance: "Original guidance" }); saved.resolve({ runResumed: false, guidanceSaved: true, savedGuidance: "Original guidance" }); });
  // The new read must agree with the committed receipt; the earlier one must not win.
  stale.resolve({ conversationId: "original", approval: { id: "budget", runId: "parent", kind: "budget", question: "Limit", status: "pending" }, canSendGuidance: true, canAskAdvisor: true, guidanceMode: "deferred", savedGuidance: "" });
  await read; await write;
  expect(session.getSnapshot().draft).toBe("New writing");
  expect(session.getSnapshot().discussion?.savedGuidance).toBe("Original guidance");
  expect(session.getSnapshot().guidanceRetry).toBeNull(); session.dispose();
});

it("does not offer Save guidance for an unsupported legacy budget or a resolved request", async () => {
  const { transport, session, update } = setup(); update({ canSendGuidance: false });
  await session.discussApproval("budget"); session.setDraft("Keep my draft"); mount(session, transport);
  expect(screen.queryByRole("button", { name: "Save guidance" })).toBeNull();
  expect(await session.sendGuidance()).toBe(false);
  update({ approval: { id: "budget", runId: "parent", kind: "budget", question: "Limit", status: "approved" }, canAskAdvisor: false });
  await act(async () => session.reload());
  expect(screen.getByRole("button", { name: "Send discussion message" })).toHaveProperty("disabled", true);
  expect(transport.approvalGuidance).not.toHaveBeenCalled(); expect(session.getSnapshot().draft).toBe("Keep my draft"); session.dispose();
});

it("does not put a late budget-save receipt into a different conversation", async () => {
  const { transport, session } = setup(); await session.discussApproval("budget");
  const saved = deferred<ApprovalGuidanceReceipt>(); transport.approvalGuidance.mockReturnValueOnce(saved.promise);
  session.setDraft("Original guidance"); const write = session.sendGuidance();
  await session.select("other"); session.setDraft("Another conversation's draft");
  saved.resolve({ runResumed: false, guidanceSaved: true, savedGuidance: "Original guidance" }); await write;
  expect(session.getSnapshot()).toMatchObject({ selectedId: "other", draft: "Another conversation's draft", discussion: null, guidanceRetry: null });
  expect(transport.ask).not.toHaveBeenCalled(); session.dispose();
});
