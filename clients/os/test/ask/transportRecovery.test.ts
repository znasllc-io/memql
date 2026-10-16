import { afterEach, expect, it, vi } from "vitest";
import type { Dispatcher } from "@znasllc-io/memql-sdk-core/client";
import { SdkAskTransport, type AskStreamFn } from "../../src/ask/sdkTransport";
import { ConversationSession, type AskTurn } from "../../src/ask/conversationSession";
afterEach(() => vi.useRealTimers());
const tick = () => new Promise((r) => setTimeout(r, 0));
it("shows a rejected result while the delta iterator still waits", async () => {
  let fail: (e: Error) => void = () => {}; let release: () => void = () => {}; let signal: AbortSignal | undefined;
  const result = new Promise((_, reject) => { fail = reject; }); void result.catch(() => {});
  const stalled = new Promise<void>((r) => { release = r; });
  const stream: AskStreamFn = (_d, _m, opts) => { signal = opts.signal; return { result, deltas: (async function* () { await stalled; })() }; };
  const error = vi.fn(); const done = vi.fn();
  new SdkAskTransport(() => ({}) as Dispatcher, stream).ask("hi", null, { delta: vi.fn(), done, error }, { conversationId: "thread", turnId: "turn" });
  fail(new Error("Peer disconnected.")); await tick();
  expect(error).toHaveBeenCalledWith("Peer disconnected."); expect(signal?.aborted).toBe(true); expect(done).not.toHaveBeenCalled(); release();
});
it("rejects an ended iterator with no terminal result", async () => {
  const error = vi.fn();
  const stream: AskStreamFn = () => ({ deltas: (async function* () { yield { textDelta: "partial" }; })(), result: new Promise(() => {}) });
  new SdkAskTransport(() => ({}) as Dispatcher, stream).ask("hi", null, { delta: vi.fn(), done: vi.fn(), error }, { conversationId: "thread", turnId: "turn" });
  await tick(); expect(error).toHaveBeenCalledWith(expect.stringMatching(/ended before.*completion/));
});
it("uses a terminal answer without deltas and rejects an empty answer", async () => {
  for (const answer of ["A complete answer", ""]) {
    const delta = vi.fn(); const done = vi.fn(); const error = vi.fn();
    const stream: AskStreamFn = () => ({ deltas: (async function* () {})(), result: Promise.resolve({ message: { content: answer } }) });
    new SdkAskTransport(() => ({}) as Dispatcher, stream).ask("hi", null, { delta, done, error }, { conversationId: "thread", turnId: "turn" }); await tick();
    if (answer) { expect(delta).toHaveBeenCalledWith(answer); expect(done).toHaveBeenCalledOnce(); }
    else { expect(error).toHaveBeenCalledWith(expect.stringMatching(/without an answer/)); expect(done).not.toHaveBeenCalled(); }
  }
});

it.each([
  { phase: "running", arguments: { background: true, workStatus: "compiling" } },
  { phase: "queued", arguments: {} },
  { phase: "waiting", arguments: {} },
])("accepts an empty background handoff: $phase", async receipt => {
  const done = vi.fn(); const error = vi.fn(); const activity = vi.fn();
  const event = { id: "work:r", kind: "run", ...receipt, arguments: { runId: "r", goalId: "g", ...receipt.arguments } };
  const stream: AskStreamFn = () => ({ deltas: (async function* () { yield { metadata: { ask: event } }; })(), result: Promise.resolve({ message: { content: "" } }) });
  new SdkAskTransport(() => ({}) as Dispatcher, stream).ask("hola hola", null, { delta: vi.fn(), done, error, activity }, { conversationId: "chat", turnId: "turn" });
  await tick();
  expect(activity).toHaveBeenCalledWith(event); expect(done).toHaveBeenCalledOnce(); expect(error).not.toHaveBeenCalled();
});

it("keeps an unmarked empty running stream as an error", async () => {
  const done = vi.fn(); const error = vi.fn();
  const stream: AskStreamFn = () => ({ deltas: (async function* () { yield { metadata: { ask: { id: "work:r", kind: "run", phase: "running", arguments: { runId: "r" } } } }; })(), result: Promise.resolve({ message: { content: "" } }) });
  new SdkAskTransport(() => ({}) as Dispatcher, stream).ask("hola", null, { delta: vi.fn(), done, error }, { conversationId: "chat", turnId: "turn" });
  await tick(); expect(error).toHaveBeenCalledWith(expect.stringMatching(/without an answer/)); expect(done).not.toHaveBeenCalled();
});

it("observes a slow greeting through the real transport without showing a false error", async () => {
  vi.useFakeTimers();
  const stream: AskStreamFn = vi.fn(() => ({ deltas: (async function* () {
    yield { metadata: { ask: { id: "work:r", kind: "run", phase: "running", arguments: { runId: "r", goalId: "g", background: true, workStatus: "compiling" } } } };
  })(), result: Promise.resolve({ message: { content: "" } }) }));
  const transport = new SdkAskTransport(() => ({}) as Dispatcher, stream);
  transport.conversations.create = async () => ({ id: "chat", title: "Greeting" });
  transport.conversations.list = async () => [];
  let stored: AskTurn[] = [];
  transport.conversations.read = vi.fn(async () => stored);
  const session = new ConversationSession(transport);
  try {
    expect(session.send("hola hola", null)).toBe(true);
    await vi.advanceTimersByTimeAsync(1);
    const pending = session.getSnapshot().turns[0]!;
    expect(pending).toMatchObject({ state: "queued", background: true, runId: "r", answer: "", workStatus: "compiling" });
    expect(pending.error).toBeUndefined(); expect(session.getSnapshot().busy).toBe(false);
    stored = [{ ...pending, state: "done", workStatus: "succeeded", answer: "¡Hola! ¿En qué puedo ayudarte hoy?", endedAt: new Date().toISOString() }];
    await vi.advanceTimersByTimeAsync(2500);
    expect(session.getSnapshot().turns[0]).toMatchObject({ state: "done", answer: stored[0]!.answer });
    expect(stream).toHaveBeenCalledOnce(); expect(transport.conversations.read).toHaveBeenCalledOnce();
  } finally { session.dispose(); }
  expect(vi.getTimerCount()).toBe(0);
});
