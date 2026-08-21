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
