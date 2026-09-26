import { expect, it, vi } from "vitest";
import type { Dispatcher } from "@znasllc-io/memql-sdk-core/client";
import { SdkAskTransport, type AskStreamFn } from "../../src/ask/sdkTransport";
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
