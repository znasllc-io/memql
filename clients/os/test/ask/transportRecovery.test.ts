import { afterEach, expect, it, vi } from "vitest";
import type { Dispatcher } from "@znasllc-io/memql-sdk-core/client";
import { SdkAskTransport, type AskStreamFn } from "../../src/ask/sdkTransport";
import { ConversationSession, type AskTurn } from "../../src/ask/conversationSession";
import type { AskCallbacks, AskOptions, AskTransport } from "../../src/ask/askController";
afterEach(() => vi.useRealTimers());
const tick = () => new Promise((r) => setTimeout(r, 0));
it("recovers an older interrupted request without detaching a newer reply", async () => {
  const callbacks: AskCallbacks[] = [], options: AskOptions[] = [], cancels: ReturnType<typeof vi.fn>[] = [];
  let stored: AskTurn[] = [];
  const transport: AskTransport = { ask: vi.fn((_prompt,_context,on,opts) => {callbacks.push(on);options.push(opts!);const cancel=vi.fn();cancels.push(cancel);return {cancel};}), conversations:{create:async()=>({id:"chat",title:"Chat"}),list:async()=>[],read:async()=>stored} };
  const session = new ConversationSession(transport);
  try {
    session.send("First task",null); await tick(); callbacks[0]!.interrupted!();
    const old = session.getSnapshot().turns[0]!;
    session.send("Another task",null); await tick();
    stored = [{...old,runId:"old-run",state:"waiting",background:true}];
    await session.reload(false);
    expect(session.getSnapshot().busy).toBe(true); expect(cancels[1]).not.toHaveBeenCalled();
    callbacks[1]!.delta("The new reply"); callbacks[1]!.done();
    expect(session.getSnapshot().turns).toEqual(expect.arrayContaining([expect.objectContaining({id:options[1]!.turnId,answer:"The new reply",state:"done"})]));
    expect(transport.ask).toHaveBeenCalledTimes(2);
  } finally {session.dispose();}
});

it("retains an unconfirmed request after a bounded recovery without resubmitting it", async () => {
  vi.useFakeTimers(); let callback!: AskCallbacks;
  const transport: AskTransport = {ask:vi.fn((_prompt,_context,on)=>{callback=on;return {cancel:vi.fn()};}),conversations:{create:async()=>({id:"chat",title:"Chat"}),list:async()=>[],read:async()=>[]}};
  const session = new ConversationSession(transport);
  try {
    session.send("An uncertain request",null); await vi.advanceTimersByTimeAsync(1); callback.interrupted!();
    session.setDraft("My next draft"); await vi.advanceTimersByTimeAsync(30_001);
    expect(session.getSnapshot().turns[0]).toMatchObject({prompt:"An uncertain request",state:"error",error:expect.stringContaining("could not be confirmed")});
    await session.reload(false);
    expect(session.getSnapshot().turns[0]!.prompt).toBe("An uncertain request");
    expect(session.getSnapshot().draft).toBe("My next draft"); expect(transport.ask).toHaveBeenCalledOnce();
  } finally {session.dispose();}
});
it.each([true, false])("reconciles transport loss without an error or another submission (run event received: %s)", async hasRun => {
  vi.useFakeTimers();
  let rejectResult!: (error: Error) => void;
  const terminal = new Promise((_, reject) => { rejectResult = reject; }); void terminal.catch(() => {});
  const stream: AskStreamFn = vi.fn(() => ({ result: terminal, deltas: (async function* () {
    if (hasRun) yield { metadata: { ask: { id: "run-event", kind: "run", phase: "running", arguments: { runId: "run", goalId: "goal" } } } };
    await terminal;
  })() }));
  const transport = new SdkAskTransport(() => ({}) as Dispatcher, stream);
  transport.conversations.create = async () => ({ id: "chat", title: "Image" });
  transport.conversations.list = async () => [];
  let stored: AskTurn[] = [];
  transport.conversations.read = vi.fn(async () => stored);
  const session = new ConversationSession(transport);
  try {
    session.send("Create the image", null); await vi.advanceTimersByTimeAsync(1);
    session.setDraft("An unfinished follow-up");
    rejectResult(Object.assign(new Error("stream closed"), { reason: "transport" }));
    await vi.advanceTimersByTimeAsync(1);
    const pending = session.getSnapshot().turns[0]!;
    expect(pending).toMatchObject({ state: "interrupted", error: undefined });
    expect(session.getSnapshot().error).toBe("");
    transport.conversations.read = vi.fn().mockRejectedValueOnce(Object.assign(new Error("stream closed"), {reason:"transport"})).mockImplementation(async () => stored);
    await session.reload(false); expect(session.getSnapshot().snapshotError).toBe("");
    stored = [{ ...pending, runId: "run", state: "waiting", workStatus: "waiting", background: true }];
    await session.reload(false);
    expect(session.getSnapshot().turns[0]).toMatchObject({state:"waiting",runId:"run",error:undefined});
    expect(session.getSnapshot().draft).toBe("An unfinished follow-up");
    expect(stream).toHaveBeenCalledOnce();
  } finally { session.dispose(); }
  expect(vi.getTimerCount()).toBe(0);
});
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
