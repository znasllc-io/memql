// The CDC-filter catalog read (ConceptsSubscribeMsg, memql#4233).
//
// The wrap is deliberately a one-shot catalog and NOTHING more: the engine's
// reply model groups node.created.<concept> filters by domain and has no
// registry-delta stream, so these tests pin the request envelope, the domain
// filter passthrough, the tolerant decode, and the error path -- and none of
// them claim liveness the wire cannot provide.

import test from "node:test";
import assert from "node:assert/strict";

import { QueryClient } from "../src/client/query.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

class MockDispatcher {
  readonly sent: ClientMessage[] = [];
  private reply: Record<string, unknown> = {};

  setReply(payload: Record<string, unknown>): void {
    this.reply = payload;
  }

  send(msg: ClientMessage): string {
    this.sent.push(msg);
    return "mock-0";
  }

  async sendAndWait(msg: ClientMessage): Promise<ServerMessage> {
    this.sent.push(msg);
    return this.reply as unknown as ServerMessage;
  }
}

function client(): { qc: QueryClient; dispatcher: MockDispatcher } {
  const dispatcher = new MockDispatcher();
  const qc = new QueryClient(dispatcher as unknown as Dispatcher);
  return { qc, dispatcher };
}

test("requests the whole catalog with an empty payload", async () => {
  const { qc, dispatcher } = client();
  dispatcher.setReply({
    conceptsSubscribeResult: {
      requestId: "r",
      domains: [
        { domain: "platform", filters: ["node.created.v1:platform:site"] },
        { domain: "identity", filters: ["node.created.v1:identity:user"] },
      ],
    },
  });

  const catalog = await qc.subscriptionCatalog();

  assert.deepEqual(dispatcher.sent, [{ conceptsSubscribe: {} }]);
  assert.deepEqual(catalog, [
    { domain: "platform", filters: ["node.created.v1:platform:site"] },
    { domain: "identity", filters: ["node.created.v1:identity:user"] },
  ]);
});

test("passes a domain restriction through", async () => {
  const { qc, dispatcher } = client();
  dispatcher.setReply({ conceptsSubscribeResult: { requestId: "r", domains: [] } });

  await qc.subscriptionCatalog(["platform"]);

  assert.deepEqual(dispatcher.sent, [{ conceptsSubscribe: { domains: ["platform"] } }]);
});

test("drops malformed catalog entries instead of surfacing them", async () => {
  const { qc, dispatcher } = client();
  dispatcher.setReply({
    conceptsSubscribeResult: {
      requestId: "r",
      domains: [{ domain: "", filters: ["x"] }, { filters: ["y"] }, { domain: "ok" }],
    },
  });

  const catalog = await qc.subscriptionCatalog();

  assert.deepEqual(catalog, [{ domain: "ok", filters: [] }]);
});

test("surfaces a queryError as a thrown, named error", async () => {
  const { qc, dispatcher } = client();
  dispatcher.setReply({
    queryError: { requestId: "r", error: { message: "concept registry not configured" } },
  });

  await assert.rejects(qc.subscriptionCatalog(), /subscriptionCatalog: concept registry not configured/);
});
