import { expect, it, vi } from "vitest";
import { SdkAskTransport } from "../../src/ask/sdkTransport";
import type { Dispatcher } from "@znasllc-io/memql-sdk-core/client";

it("sends policy requests through the same permission-aware agent as other requests", async () => {
  const stream = vi.fn(() => ({ result: Promise.resolve({ message: { content: "Done" } }), deltas: (async function* () { yield { textDelta: "Done" }; })() }));
  await new Promise<void>((done, error) => new SdkAskTransport(() => ({} as Dispatcher), stream).ask("Create a local-first policy", "app:fleet", { delta: () => {}, done, error }, { conversationId: "conversation", turnId: "turn" }));
  expect(stream).toHaveBeenCalledWith(expect.anything(), [{ role: "user", content: "Create a local-first policy" }], expect.objectContaining({ conversationId: "conversation", requestId: "turn", pageContext: "app:fleet" }));
});
