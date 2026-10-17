import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { AskSurface } from "../../src/ask/AskSurface";
import { AskWorkQueue } from "../../src/ask/AskWorkQueue";
import { ConversationSession, type AskTurn } from "../../src/ask/conversationSession";
import type { WorkGuidanceReceipt, WorkGuidanceRequest } from "../../src/ask/askController";

afterEach(cleanup);
function work(id = "work", fields: Partial<AskTurn> = {}): AskTurn {
  return { id, prompt: `Original ${id}`, workTitle: `Review ${id}`, runId: `run-${id}`, canGuideWork: true, guidanceEpoch: "epoch", workStatus: "running", background: true, state: "queued", answer: "", startedAt: "2026-10-13T10:00:00Z", activity: [], ...fields };
}
function receipt(request: WorkGuidanceRequest, status: "queued" | "applied" | "not-received" = "queued"): WorkGuidanceReceipt {
  return { ...request, status, turn: { id: request.requestId, prompt: request.prompt, answerOnly: true, guidanceTargetRunId: request.runId, guidanceStatus: status, answer: "", state: "done", startedAt: "2026-10-13T10:01:00Z", activity: [] } };
}
function deferred<T>() { let resolve!: (value: T) => void; let reject!: (reason: Error) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }
function setup(initial = [work()]) {
  let turns = initial;
  const transport = {
    ask: vi.fn(() => ({ cancel: vi.fn() })),
    guideWork: vi.fn(async (request: WorkGuidanceRequest) => { const result = receipt(request); turns = [...turns, result.turn]; return result; }),
    conversations: { list: vi.fn(async () => []), create: vi.fn(async () => ({ id: "new", title: "New" })), read: vi.fn(async (id: string) => id === "chat" ? turns : []) },
  };
  const session = new ConversationSession(transport);
  return { session, transport, setTurns: (value: AskTurn[]) => { turns = value; } };
}
function mount(value: ReturnType<typeof setup>, unavailable = false) {
  return render(<AskSurface conversation={value.session} transport={value.transport} variant="sheet" availability={{ state: unavailable ? "unavailable" : "ready", message: "", refresh() {} }} onOpenWork={vi.fn()} />);
}

it("targets explicit work and records a human-only receipt without a new goal", async () => {
  const test = setup(); await test.session.select("chat"); test.session.setDraft("Keep the change small."); mount(test);
  expect(screen.queryByRole("button", { name: /Guide this work/ })).toBeNull();
  act(() => { test.session.beginWorkGuidance("work"); });
  const input = screen.getByRole("textbox", { name: "Ask" });
  expect(input).toHaveProperty("value", "Keep the change small.");
  expect(screen.getByRole("region", { name: "Guiding work" })).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Send guidance" }));
  await waitFor(() => expect(test.session.getSnapshot().workGuidanceSending).toBe(false));
  expect(test.transport.guideWork).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({ runId: "run-work", conversationId: "chat", prompt: "Keep the change small." }));
  expect(test.transport.ask).not.toHaveBeenCalled(); expect(test.transport.conversations.create).not.toHaveBeenCalled();
  expect(screen.getByText("Queued for the next safe boundary.")).toBeTruthy();
  expect(test.session.getSnapshot().turns.at(-1)).toMatchObject({ guidanceTargetRunId: "run-work", guidanceStatus: "queued", answerOnly: true });
  expect(test.session.getSnapshot().turns.at(-1)?.runId).toBeUndefined(); test.session.dispose();
});

it("requires server eligibility and never interprets ordinary text as guidance", async () => {
  const test = setup([work("legacy", { canGuideWork: undefined }), work("done", { state: "done" }), work("advice", { approvalId: "approval" })]);
  await test.session.select("chat");
  for (const id of ["legacy", "done", "advice", "missing"]) expect(test.session.beginWorkGuidance(id)).toBe(false);
  expect(test.session.send("Do not start another run", null)).toBe(true);
  expect(test.transport.ask).toHaveBeenCalledOnce(); expect(test.transport.guideWork).not.toHaveBeenCalled(); test.session.dispose();
});

it("keeps typing editable while acknowledgement is pending and never invents a receipt", async () => {
  const test = setup(); const pending = deferred<WorkGuidanceReceipt>(); test.transport.guideWork.mockReturnValueOnce(pending.promise);
  await test.session.select("chat"); test.session.beginWorkGuidance("work"); mount(test);
  const input = screen.getByRole("textbox", { name: "Ask" }); fireEvent.change(input, { target: { value: "First guidance" } });
  fireEvent.keyDown(input, { key: "Enter" }); fireEvent.change(input, { target: { value: "A newer draft" } });
  expect(input).toHaveProperty("readOnly", false); expect(test.session.getSnapshot().turns).toHaveLength(1);
  expect(screen.getByRole("button", { name: "Send guidance" })).toHaveProperty("disabled", true);
  const response = receipt(test.transport.guideWork.mock.calls[0]![0]); test.setTurns([work(), response.turn]);
  await act(async () => pending.resolve(response));
  expect(input).toHaveProperty("value", "A newer draft"); expect(test.transport.ask).not.toHaveBeenCalled(); test.session.dispose();
});

it("retries the exact uncertain write and preserves the new draft", async () => {
  const test = setup(); await test.session.select("chat"); test.session.beginWorkGuidance("work");
  test.transport.guideWork.mockRejectedValueOnce(new Error("Connection lost before acknowledgement")); mount(test);
  const input = screen.getByRole("textbox", { name: "Ask" }); fireEvent.change(input, { target: { value: "Original guidance" } });
  fireEvent.click(screen.getByRole("button", { name: "Send guidance" }));
  await screen.findByRole("alert"); fireEvent.change(input, { target: { value: "New guidance" } });
  fireEvent.keyDown(input, { key: "Enter" }); expect(test.transport.guideWork).toHaveBeenCalledOnce();
  fireEvent.click(screen.getByRole("button", { name: "Retry guidance" }));
  await waitFor(() => expect(test.session.getSnapshot().workGuidanceSending).toBe(false));
  expect(test.transport.guideWork.mock.calls[1]).toEqual(test.transport.guideWork.mock.calls[0]);
  expect(input).toHaveProperty("value", "New guidance"); expect(test.transport.ask).not.toHaveBeenCalled(); test.session.dispose();
});

it.each(["target", "request", "prompt", "execution"])("refuses a mismatched %s receipt and retains scoped retry", async mismatch => {
  const test = setup(); await test.session.select("chat"); test.session.beginWorkGuidance("work");
  test.transport.guideWork.mockImplementationOnce(async request => { const result = receipt(request); if (mismatch === "target") result.runId = "other"; if (mismatch === "request") result.requestId = "other"; if (mismatch === "prompt") result.turn.prompt = "other"; if (mismatch === "execution") result.turn.runId = "unexpected-execution"; return result; });
  test.session.send("Guidance", null); await waitFor(() => expect(test.session.getSnapshot().workGuidanceSending).toBe(false));
  expect(test.session.getSnapshot().workGuidanceRetry?.prompt).toBe("Guidance"); expect(test.session.getSnapshot().turns).toHaveLength(1);
  expect(test.session.send("Try ordinary", null)).toBe(false); expect(test.transport.ask).not.toHaveBeenCalled(); test.session.dispose();
});

it("requires explicit cancellation before normal send and keeps the draft", async () => {
  const test = setup(); await test.session.select("chat"); test.session.beginWorkGuidance("work"); mount(test);
  fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "Separate question" } });
  fireEvent.click(screen.getByRole("button", { name: "Return to conversation" }));
  expect(test.session.getSnapshot().draft).toBe("Separate question"); expect(screen.queryByRole("region", { name: "Guiding work" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Send" })); expect(test.transport.ask).toHaveBeenCalledWith("Separate question", null, expect.anything(), expect.anything());
  expect(test.transport.guideWork).not.toHaveBeenCalled(); test.session.dispose();
});

it("keeps a completed target scoped and refuses instead of creating new work", async () => {
  const test = setup(); await test.session.select("chat"); test.session.beginWorkGuidance("work");
  test.setTurns([work("work", { state: "done", canGuideWork: false })]); await test.session.reload();
  expect(test.session.send("Please continue", null)).toBe(false); expect(test.session.getSnapshot().error).toContain("no longer available");
  expect(test.session.getSnapshot().workGuidance?.runId).toBe("run-work"); expect(test.transport.ask).not.toHaveBeenCalled(); expect(test.transport.guideWork).not.toHaveBeenCalled(); test.session.dispose();
});

it.each(["selection", "new", "dispose"])("ignores late acknowledgements after %s", async action => {
  const test = setup(); const pending = deferred<WorkGuidanceReceipt>(); test.transport.guideWork.mockReturnValueOnce(pending.promise);
  await test.session.select("chat"); test.session.beginWorkGuidance("work"); test.session.send("Original guidance", null);
  if (action === "selection") await test.session.select("other"); else if (action === "new") test.session.newConversation(); else test.session.dispose();
  test.session.setDraft("New draft"); const before = test.session.getSnapshot(); pending.resolve(receipt(test.transport.guideWork.mock.calls[0]![0]));
  await Promise.resolve(); await Promise.resolve();
  expect(test.session.getSnapshot()).toEqual(before); expect(test.transport.ask).not.toHaveBeenCalled(); test.session.dispose();
});

it("restores uncertain retry after cleanup/reactivation without a stuck composer", async () => {
  const test = setup(); const pending = deferred<WorkGuidanceReceipt>(); test.transport.guideWork.mockReturnValueOnce(pending.promise);
  await test.session.select("chat"); test.session.beginWorkGuidance("work"); test.session.send("Original", null);
  test.session.dispose(); test.session.activate();
  expect(test.session.getSnapshot().workGuidanceSending).toBe(false); expect(test.session.getSnapshot().workGuidanceRetry?.prompt).toBe("Original");
  pending.reject(new Error("Lost")); await Promise.resolve(); await Promise.resolve();
  test.session.retryWorkGuidance(); await waitFor(() => expect(test.session.getSnapshot().workGuidanceSending).toBe(false));
  expect(test.transport.guideWork.mock.calls[1]).toEqual(test.transport.guideWork.mock.calls[0]); test.session.dispose();
});

it("does not let a stale poll erase an acknowledged guidance turn", async () => {
  const test = setup(); await test.session.select("chat"); const old = deferred<AskTurn[]>(); test.transport.conversations.read.mockReturnValueOnce(old.promise);
  const polling = test.session.reload(); test.session.beginWorkGuidance("work"); test.session.send("Guidance", null);
  await waitFor(() => expect(test.session.getSnapshot().workGuidanceSending).toBe(false)); old.resolve([work()]); await polling;
  expect(test.session.getSnapshot().turns).toHaveLength(2); test.session.dispose();
});

it("shows receipt acknowledgement only from the refreshed durable receipt", async () => {
  const test = setup(); await test.session.select("chat"); test.session.beginWorkGuidance("work"); test.session.send("Guidance", null);
  await waitFor(() => expect(test.session.getSnapshot().workGuidanceSending).toBe(false)); mount(test);
  expect(screen.queryByText("Received by this work.")).toBeNull();
  test.setTurns([work(), { ...test.session.getSnapshot().turns[1]!, guidanceStatus: "applied" }]); await act(async () => test.session.reload());
  expect(screen.getByText("Received by this work.")).toBeTruthy(); test.session.dispose();
});

it("records with models unavailable, bounds Unicode guidance and refuses live voice", async () => {
  const test = setup(); await test.session.select("chat"); test.session.beginWorkGuidance("work"); mount(test, true);
  await expect(test.session.beginVoice()).rejects.toThrow("Use typing or dictation");
  expect(test.session.send("😀".repeat(4001), null)).toBe(false);
  fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "😀".repeat(4000) } }); fireEvent.click(screen.getByRole("button", { name: "Send guidance" }));
  await waitFor(() => expect(test.session.getSnapshot().workGuidanceSending).toBe(false)); expect(test.transport.guideWork).toHaveBeenCalledOnce(); test.session.dispose();
});

it("keeps only the Nexus shortcut for many cards, including guidable work", () => {
  const onOpen = vi.fn(); render(<AskWorkQueue turns={[work("a"), work("b"), work("c"), work("d")]} onOpen={onOpen} />);
  expect(screen.getAllByRole("button")).toHaveLength(1);
  expect(screen.queryByRole("button", { name: /Guide this work/ })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "4 tasks in progress" }));
  expect(onOpen).toHaveBeenCalledExactlyOnceWith();
});


it("keeps target epoch unchanged through retry and refuses a refreshed different lineage", async () => {
  const test = setup(); await test.session.select("chat"); test.session.beginWorkGuidance("work");
  test.setTurns([work("work", { guidanceEpoch: "replanned" })]); await test.session.reload();
  expect(test.session.send("For the earlier plan", null)).toBe(false); expect(test.transport.guideWork).not.toHaveBeenCalled();
  expect(test.session.beginWorkGuidance("work")).toBe(true);
  test.transport.guideWork.mockRejectedValueOnce(new Error("Response lost")); test.session.send("For the new plan", null);
  await waitFor(() => expect(test.session.getSnapshot().workGuidanceSending).toBe(false)); test.session.retryWorkGuidance();
  await waitFor(() => expect(test.session.getSnapshot().workGuidanceSending).toBe(false));
  expect(test.transport.guideWork.mock.calls[0]?.[0].targetEpoch).toBe("replanned"); expect(test.transport.guideWork.mock.calls[1]).toEqual(test.transport.guideWork.mock.calls[0]); test.session.dispose();
});

it("cannot mix a failed approval retry with an active work target", async () => {
  const test = setup([work(), work("advice", { approvalId: "approval", state: "error", error: "Timed out" })]); await test.session.select("chat"); test.session.beginWorkGuidance("work");
  expect(await test.session.prepareApprovalTurnRetry("advice")).toBe(false); expect(test.session.getSnapshot().discussionDraft).toBeNull();
  expect(await test.session.discussApproval("approval")).toBe(false); expect(test.session.getSnapshot().workGuidance?.runId).toBe("run-work"); test.session.dispose();
});


it("shows a saved message not received after work ends without retrying execution", async () => {
  const test = setup(); await test.session.select("chat"); test.session.beginWorkGuidance("work");
  test.transport.guideWork.mockImplementationOnce(async request => { const result = receipt(request, "not-received"); test.setTurns([work("work", { state: "done", canGuideWork: false }), result.turn]); return result; });
  test.session.send("Late guidance", null); await waitFor(() => expect(test.session.getSnapshot().workGuidanceSending).toBe(false)); mount(test);
  expect(screen.getByText("Not received. This work ended or changed.")).toBeTruthy(); expect(screen.queryByRole("button", { name: "Retry guidance" })).toBeNull();
  expect(test.transport.ask).not.toHaveBeenCalled(); test.session.dispose();
});


it("a late original acknowledgement cannot erase a newer uncertain request", async () => {
  const test = setup(); const original = deferred<WorkGuidanceReceipt>(), newer = deferred<WorkGuidanceReceipt>();
  test.transport.guideWork.mockReturnValueOnce(original.promise);
  await test.session.select("chat"); test.session.beginWorkGuidance("work"); test.session.send("First", null);
  const first = test.transport.guideWork.mock.calls[0]![0];
  test.session.dispose(); test.session.activate(); test.session.retryWorkGuidance();
  await waitFor(() => expect(test.session.getSnapshot().workGuidanceSending).toBe(false));
  test.transport.guideWork.mockReturnValueOnce(newer.promise); test.session.send("Second", null);
  const second = test.transport.guideWork.mock.calls[2]![0]; expect(second.requestId).not.toBe(first.requestId);
  original.resolve(receipt(first)); await Promise.resolve(); await Promise.resolve();
  test.session.leaveWorkGuidance(); newer.reject(new Error("Response lost")); await Promise.resolve(); await Promise.resolve();
  expect(test.session.beginWorkGuidance("work")).toBe(true);
  expect(test.session.getSnapshot().workGuidanceRetry).toEqual(second); expect(test.session.send("Third", null)).toBe(false);
  test.session.retryWorkGuidance(); await waitFor(() => expect(test.session.getSnapshot().workGuidanceSending).toBe(false));
  expect(test.transport.guideWork.mock.calls[3]?.[0]).toEqual(second); test.session.dispose();
});
