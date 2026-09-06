// The generated typed construct surface (memql#4232).
//
// These tests pin the three properties the generated layer exists for,
// against REAL generated constructs (siteById / todos) rather than
// fixtures, so a generator regression that survives typecheck -- a
// builder composing the wrong call string, a prototype method that never
// installed -- fails here instead of at a portal runtime boundary.
//
//   1. Importing the client barrel INSTALLS the generated methods on
//      QueryClient.prototype (the side-effect the barrel comment calls
//      load-bearing -- a tree-shaken or dropped import would typecheck
//      fine via declare-module and then be undefined at runtime).
//   2. Builders compose the kind-prefixed named-args invocation form,
//      omitting optional args that are undefined.
//   3. A generated method dispatches through executeNamed: the wire
//      envelope is the same executeQuery payload every hand-written
//      named call composes -- no new wire surface.
//
// The concept-metadata module is asserted alongside: BoundConcepts is
// the machine-readable construct -> concept join the generated method
// comments point at.

import test from "node:test";
import assert from "node:assert/strict";

// The BARREL import, deliberately -- not the generated modules directly.
// Consumers import the barrel; if the barrel ever stops executing the
// generated modules, the prototype assertions below are the alarm.
import {
  QueryClient,
  buildSiteById,
  buildTodos,
  buildCampaignStartSend,
  buildCampaignScheduleSend,
  buildCampaignPauseSend,
  buildCampaignResumeSend,
  buildIntegrationStatus,
  BoundConcepts,
  Concepts,
  topicFor,
  filterFor,
} from "../src/client/index.js";
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

test("barrel import installs generated methods on QueryClient.prototype", () => {
  // One per generated kind file, so a single dropped side-effect import
  // is caught by the file it lives in.
  assert.equal(typeof QueryClient.prototype.siteById, "function"); // queries
  assert.equal(typeof QueryClient.prototype.todos, "function"); // queries (optional args)
});

test("builder composes the kind-prefixed named-args call", () => {
  assert.equal(buildSiteById({ siteId: "s-1" }), 'query siteById(siteId: "s-1")');
});

test("builder omits optional args that are undefined", () => {
  assert.equal(buildTodos({}), "query todos()");
  assert.equal(buildTodos({ done: true }), "query todos(done: true)");
});

test("generated method dispatches through executeNamed's executeQuery envelope", async () => {
  const { qc, dispatcher } = client();
  dispatcher.setReply({ queryResult: { requestId: "r", result: { bundle: { nodes: [] } } } });

  await qc.siteById({ siteId: "s-1" });

  assert.equal(dispatcher.sent.length, 1);
  const msg = dispatcher.sent[0] as { executeQuery?: { query?: string } };
  assert.equal(msg.executeQuery?.query, 'query siteById(siteId: "s-1")');
});

test("generated method surfaces a queryError as a thrown, name-prefixed error", async () => {
  const { qc, dispatcher } = client();
  dispatcher.setReply({
    queryError: { requestId: "r", error: { message: "refused" } },
  });

  await assert.rejects(qc.siteById({ siteId: "s-1" }), /siteById: refused/);
});

test("concept metadata joins constructs to canonical ids and composes topics", () => {
  assert.equal(BoundConcepts.siteById, "v1:platform:site");
  assert.equal(Concepts.PLATFORM_SITE, "v1:platform:site");
  assert.equal(topicFor("v1:platform:site", "created"), "graph.node.created.v1:platform:site");
  assert.equal(filterFor("v1:platform:site", "updated"), "node.updated.v1:platform:site");
});

// The five builtins memql#4239 put on the generated surface: the four
// operator send actions and the integration-status read. A builtin is
// emitted only when marked @sdk, so this pins both that the marker held and
// that the builders compose the exact wire forms a client sends. The
// portal's tests pinned the same forms from the consumer side until epic
// memql#4984 retired them, so this file and its Go twin
// (sdk/go/client/generated_builders_test.go) are now the only guard.
test("the @sdk builtins are installed on the prototype and compose the kind-prefixed form", () => {
  assert.equal(typeof QueryClient.prototype.campaignStartSend, "function"); // builtins
  assert.equal(typeof QueryClient.prototype.integrationStatus, "function"); // builtins

  assert.equal(
    buildCampaignStartSend({ campaignId: "camp-1" }),
    'builtin campaignStartSend(campaignId: "camp-1")',
  );
  assert.equal(
    buildCampaignScheduleSend({ campaignId: "camp-1", scheduledAt: "2026-09-01T09:00:00Z" }),
    'builtin campaignScheduleSend(campaignId: "camp-1", scheduledAt: "2026-09-01T09:00:00Z")',
  );
  assert.equal(
    buildCampaignPauseSend({ campaignId: "camp-1" }),
    'builtin campaignPauseSend(campaignId: "camp-1")',
  );
  assert.equal(
    buildCampaignResumeSend({ campaignId: "camp-1" }),
    'builtin campaignResumeSend(campaignId: "camp-1")',
  );

  // probe is an optional bool. Undefined is omitted; false is SENT -- the
  // portal's configuration read says `probe: false` explicitly and its
  // test asserts the string, so a builder that dropped a false would pass
  // typecheck and fail the portal.
  assert.equal(buildIntegrationStatus({}), "builtin integrationStatus()");
  assert.equal(buildIntegrationStatus({ probe: false }), "builtin integrationStatus(probe: false)");
  assert.equal(buildIntegrationStatus({ probe: true }), "builtin integrationStatus(probe: true)");
});
