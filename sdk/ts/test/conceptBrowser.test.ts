// Concept-browser tests. The query string is the contract: it MUST declare
// sort + paginate, or the engine applies its implicit unmarked-list backstop
// (MEMORY_ENGINE_DEFAULT_LIST_CAP, 50 rows) and silently truncates with no
// continuation cursor (memql#2008). These tests pin the emitted string.

import test from "node:test";
import assert from "node:assert/strict";

import {
  browseConceptPage,
  getRowByConceptAndId,
  DEFAULT_CONCEPT_BROWSE_PAGE_SIZE,
} from "../src/client/conceptBrowser.js";
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

  addEventListener(): () => void {
    return () => {};
  }

  registerStream(): () => void {
    return () => {};
  }

  lastQuery(): string {
    const last = this.sent.at(-1) as { executeQuery?: { query?: string } } | undefined;
    return last?.executeQuery?.query ?? "";
  }

  lastCursor(): string | undefined {
    const last = this.sent.at(-1) as { executeQuery?: { cursor?: string } } | undefined;
    return last?.executeQuery?.cursor;
  }
}

function client(d: MockDispatcher): QueryClient {
  return new QueryClient(d as unknown as Dispatcher);
}

function bundleReply(nodes: unknown[], cursor?: string): Record<string, unknown> {
  return {
    queryResult: {
      requestId: "r",
      result: {
        bundle: { nodes },
        ...(cursor === undefined ? {} : { meta: { cursor } }),
      },
    },
  };
}

test("emits a sort+paginate query so the keyset window applies", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([]));
  await browseConceptPage(client(d), "v1:agents:agent");
  assert.equal(
    d.lastQuery(),
    `sort(paginate(concept==v1:agents:agent, ${DEFAULT_CONCEPT_BROWSE_PAGE_SIZE}), "createdAt", "asc")`,
  );
});

test("honors an explicit page size", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([]));
  await browseConceptPage(client(d), "v1:agents:agent", { pageSize: 25 });
  assert.match(d.lastQuery(), /paginate\(concept==v1:agents:agent, 25\)/);
});

test("falls back to the default page size for a non-positive value", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([]));
  await browseConceptPage(client(d), "v1:agents:agent", { pageSize: 0 });
  assert.match(
    d.lastQuery(),
    new RegExp(`paginate\\(concept==v1:agents:agent, ${DEFAULT_CONCEPT_BROWSE_PAGE_SIZE}\\)`),
  );
});

test("forwards the continuation cursor", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([]));
  await browseConceptPage(client(d), "v1:agents:agent", { cursor: "opaque-1" });
  assert.equal(d.lastCursor(), "opaque-1");
});

test("omits the cursor on a first page", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([]));
  await browseConceptPage(client(d), "v1:agents:agent");
  assert.equal(d.lastCursor(), undefined);
});

test("preserves the full nested node shape rather than flattening", async () => {
  const d = new MockDispatcher();
  d.setReply(
    bundleReply([
      { id: "a1", concept: "v1:agents:agent", payload: { name: "Sofia" } },
    ]),
  );
  const page = await browseConceptPage(client(d), "v1:agents:agent");
  assert.equal(page.rows.length, 1);
  assert.deepEqual(page.rows[0]?.["payload"], { name: "Sofia" });
});

test("returns the next cursor when the engine mints one", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([{ id: "a1" }], "next-page"));
  const page = await browseConceptPage(client(d), "v1:agents:agent");
  assert.equal(page.nextCursor, "next-page");
});

test("returns an empty cursor when the set is exhausted", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([{ id: "a1" }]));
  const page = await browseConceptPage(client(d), "v1:agents:agent");
  assert.equal(page.nextCursor, "");
});

test("rejects an empty concept id", async () => {
  const d = new MockDispatcher();
  await assert.rejects(
    () => browseConceptPage(client(d), ""),
    /conceptId is required/,
  );
});

test("getRowByConceptAndId queries by concept and id", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([{ id: "a1", payload: { name: "Sofia" } }]));
  const row = await getRowByConceptAndId(client(d), "v1:agents:agent", "a1");
  assert.equal(d.lastQuery(), "concept==v1:agents:agent && id==a1");
  assert.deepEqual(row?.["payload"], { name: "Sofia" });
});

test("getRowByConceptAndId returns null when nothing matches", async () => {
  const d = new MockDispatcher();
  d.setReply(bundleReply([]));
  const row = await getRowByConceptAndId(client(d), "v1:agents:agent", "nope");
  assert.equal(row, null);
});

test("getRowByConceptAndId rejects a missing row id", async () => {
  const d = new MockDispatcher();
  await assert.rejects(
    () => getRowByConceptAndId(client(d), "v1:agents:agent", ""),
    /rowId is required/,
  );
});
