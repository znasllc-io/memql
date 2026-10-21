import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AskProvider, useAsk } from "../../src/ask/AskProvider";
import { AskSurface } from "../../src/ask/AskSurface";
import type { AskCallbacks, AskTransport } from "../../src/ask/askController";
import type { ApprovalDiscussion, AskTurn } from "../../src/ask/conversationSession";
import { READY_ASK } from "../../src/ask/useAskReadiness";

afterEach(() => { cleanup(); vi.useRealTimers(); });
const lost: AskTurn = { id: "original", runId: "same-run", prompt: "Verify a change", workTitle: "Verify change", workStatus: "abandoned", state: "error", error: "Lost", answer: "", background: true, startedAt: "2026-10-13T10:00:00Z", activity: [] };
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(yes => { resolve = yes; }); return { promise, resolve }; }
async function setup() {
  vi.useFakeTimers();
  let controls!: ReturnType<typeof useAsk>;
  let callbacks!: AskCallbacks;
  let stored: AskTurn[] = [lost];
  const store = { list: vi.fn(async () => []), create: vi.fn(async () => ({ id: "new", title: "New" })), read: vi.fn(async (_id: string) => stored) };
  const discussion: ApprovalDiscussion = { conversationId: "chat", approval: { id: "approval", runId: "same-run", kind: "feedback", question: "Review this request", status: "pending" }, canAskAdvisor: true, canSendGuidance: true };
  const transport: AskTransport = {
    conversations: store,
    ask: vi.fn((_prompt, _context, on) => { callbacks = on; return { cancel: vi.fn() }; }),
    discussApproval: vi.fn(async () => discussion),
  };
  function Harness() {
    controls = useAsk();
    return <><button onClick={() => controls.openAsk()}>Open Ask</button>{controls.sheet.open && <AskSurface transport={transport} conversation={controls.conversation} availability={READY_ASK} variant="sheet" onClose={controls.closeAsk} onOpenWork={vi.fn()} />}</>;
  }
  render(<AskProvider transport={transport} availability={READY_ASK}><Harness /></AskProvider>);
  await act(async () => { await controls.conversation.select("chat"); });
  return { get controls() { return controls; }, store, transport, callbacks: () => callbacks, setStored: (turns: AskTurn[]) => { stored = turns; } };
}
async function open() { await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Open Ask" })); }); }

it("refreshes a terminal linked run on ordinary reopen while retaining its draft", async () => {
  const test = await setup(); await open();
  fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: "My unsent review" } });
  act(() => test.controls.closeAsk());
  const reads = test.store.read.mock.calls.length, histories = test.store.list.mock.calls.length;
  await act(async () => { await vi.advanceTimersByTimeAsync(60000); });
  expect(test.store.read).toHaveBeenCalledTimes(reads); // Terminal work does not poll forever.
  test.setStored([{ ...lost, error: undefined, state: "waiting", workStatus: "waiting" }]);
  await open();
  expect(test.controls.conversation.getSnapshot().turns[0]).toMatchObject({ runId: "same-run", state: "waiting" });
  expect(screen.queryByText("Lost")).toBeNull();
  expect(screen.getByRole("textbox", { name: "Ask" })).toHaveProperty("value", "My unsent review");
  act(() => test.controls.closeAsk());
  test.setStored([{ ...lost, error: undefined, state: "queued", workStatus: "running" }]);
  await open();
  expect(test.controls.conversation.getSnapshot().turns[0]).toMatchObject({ runId: "same-run", state: "queued", workStatus: "running" });
  expect(screen.getByRole("textbox", { name: "Ask" })).toHaveProperty("value", "My unsent review");
  expect(test.store.list).toHaveBeenCalledTimes(histories);
  expect(test.transport.ask).not.toHaveBeenCalled();
});

it("coalesces a rapid reopen with an existing snapshot read without fetching the history list", async () => {
  const test = await setup(); const pending = deferred<AskTurn[]>();
  test.store.read.mockReturnValueOnce(pending.promise);
  const reads = test.store.read.mock.calls.length, histories = test.store.list.mock.calls.length;
  await open(); act(() => test.controls.closeAsk()); await open();
  const refresh = test.controls.conversation.reload(false);
  expect(test.store.read).toHaveBeenCalledTimes(reads + 1);
  await act(async () => { pending.resolve([lost]); await refresh; });
  expect(test.store.list).toHaveBeenCalledTimes(histories);
});

it("ignores a delayed reopen snapshot after a new stream finishes", async () => {
  const test = await setup(); const pending = deferred<AskTurn[]>();
  test.store.read.mockReturnValueOnce(pending.promise); await open();
  await act(async () => { test.controls.conversation.send("A new request", null); });
  // As the cluster does, the finished turn is saved before the stream says done.
  const sent = test.controls.conversation.getSnapshot().turns.at(-1)!;
  test.setStored([lost, { ...sent, answer: "Fresh reply", state: "done", endedAt: "2026-10-13T10:05:00Z" }]);
  act(() => { test.callbacks().delta("Fresh reply"); test.callbacks().done(); });
  await act(async () => { pending.resolve([lost]); });
  expect(screen.getByText("Fresh reply")).toBeTruthy();
  expect(test.controls.conversation.getSnapshot().turns).toHaveLength(2);
});

it("does not replace a newer conversation selection with the reopened snapshot", async () => {
  const test = await setup(); const pending = deferred<AskTurn[]>();
  test.store.read.mockReturnValueOnce(pending.promise); await open();
  test.setStored([]);
  await act(async () => { await test.controls.conversation.select("other"); test.controls.conversation.setDraft("Other draft"); });
  await act(async () => { pending.resolve([lost]); });
  expect(test.controls.conversation.getSnapshot()).toMatchObject({ selectedId: "other", turns: [], draft: "Other draft" });
});

it("preserves a newly opened approval discussion when the earlier reopen read arrives", async () => {
  const test = await setup(); const pending = deferred<AskTurn[]>();
  test.store.read.mockReturnValueOnce(pending.promise); await open();
  test.setStored([]);
  await act(async () => { await test.controls.conversation.discussApproval("approval"); test.controls.conversation.setDraft("Discuss this decision"); });
  await act(async () => { pending.resolve([lost]); });
  expect(test.controls.conversation.getSnapshot()).toMatchObject({ turns: [], discussion: { approval: { id: "approval" } }, draft: "Discuss this decision" });
  expect(screen.getByRole("button", { name: "Send guidance" })).toBeTruthy();
  expect(test.transport.ask).not.toHaveBeenCalled();
});
