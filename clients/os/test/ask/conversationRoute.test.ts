import { describe, expect, it, vi } from "vitest";
import type { Dispatcher } from "@znasllc-io/memql-sdk-core/client";
import { SdkAskTransport } from "../../src/ask/sdkTransport";
import { AUTO_ROUTING, type AskRouting } from "../../src/ask/askRoute";
const CLAUDE_STRONG: AskRouting = { source: "app:claude-code", level: "strong" };
describe("the SDK transport", () => {
  it("sends the source as provider and the level beside it", async () => {
    const stream = vi.fn(() => ({ result: Promise.resolve({ message: { content: "Done" } }), deltas: (async function* () { yield { textDelta: "Done" }; })() }));
    await new Promise<void>((done, error) => new SdkAskTransport(() => ({} as Dispatcher), stream).ask("hi", null, { delta: () => {}, done, error }, { conversationId: "c1", turnId: "t1", routing: CLAUDE_STRONG }));
    expect(stream).toHaveBeenCalledWith(expect.anything(), [{ role: "user", content: "hi" }], expect.objectContaining({ conversationId: "c1", requestId: "t1", provider: "app:claude-code", level: "strong" }));
  });

  it("leaves both unset on Auto so the cluster's rules decide", async () => {
    const stream = vi.fn(() => ({ result: Promise.resolve({ message: { content: "Done" } }), deltas: (async function* () { yield { textDelta: "Done" }; })() }));
    await new Promise<void>((done, error) => new SdkAskTransport(() => ({} as Dispatcher), stream).ask("hi", null, { delta: () => {}, done, error }, { conversationId: "c1", turnId: "t1", routing: AUTO_ROUTING }));
    const opts = (stream.mock.calls[0] as unknown as [unknown, unknown, Record<string, unknown>])[2];
    expect(opts.provider ?? "").toBe("");
    expect(opts.level ?? "").toBe("");
  });
});
