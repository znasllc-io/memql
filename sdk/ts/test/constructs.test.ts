// Mock-dispatcher tests for the construct catalog client (memql#3750).
//
// Two things matter here beyond the usual envelope/unwrap coverage, and both
// are cases where a plausible-looking client would be quietly wrong.
//
// THE DEFAULTS. protojson omits a zero int, an empty string, a false bool and
// an empty list ENTIRELY, so the wire form of the commonest construct in the
// tree -- a view-only one, not runnable, no args, no bound concept -- is very
// nearly `{}`. A client that passes the wire object through hands its consumer
// `undefined` for exactly those fields, and `args` in particular has to be an
// array or every caller that iterates it throws.
//
// THE `source` RULE. `source` is populated only when there is no file to read
// it from, which in practice means a promoted construct. The tests below pin
// both halves, because a client that assumed either "always present" or "never
// present" would render an empty source panel for the one case the Constructs
// view exists to show.

import test from "node:test";
import assert from "node:assert/strict";

import { ConstructsClient } from "../src/constructs/constructs.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

// MockDispatcher mirrors the stand-in the other surface tests use: only the
// methods this client touches, kept local so each test file stays
// self-contained.
class MockDispatcher {
  readonly sent: Array<{ msg: ClientMessage; messageId: string }> = [];
  private pendingReplies = new Map<string, (msg: ServerMessage) => void>();
  private nextId = 0;

  send(msg: ClientMessage): string {
    const id = msg.messageId ?? `mock-${this.nextId++}`;
    this.sent.push({ msg: { ...msg, messageId: id }, messageId: id });
    return id;
  }

  async sendAndWait(msg: ClientMessage): Promise<ServerMessage> {
    const id = msg.messageId ?? `mock-${this.nextId++}`;
    this.sent.push({ msg: { ...msg, messageId: id }, messageId: id });
    return new Promise<ServerMessage>((resolve) => {
      this.pendingReplies.set(id, resolve);
    });
  }

  registerStream(_requestId: string, _handler: (msg: ServerMessage) => void): () => void {
    return () => {};
  }

  reply(payload: Record<string, unknown>): void {
    const last = this.sent.at(-1);
    if (!last) throw new Error("MockDispatcher.reply: nothing sent yet");
    const resolver = this.pendingReplies.get(last.messageId);
    if (!resolver) throw new Error(`MockDispatcher.reply: no pending entry for ${last.messageId}`);
    this.pendingReplies.delete(last.messageId);
    resolver({ correlateTo: last.messageId, ...payload } as ServerMessage);
  }

  lastSent(): ClientMessage {
    const last = this.sent.at(-1);
    if (!last) throw new Error("MockDispatcher.lastSent: nothing sent yet");
    return last.msg;
  }
}

function newClient(): { mock: MockDispatcher; client: ConstructsClient } {
  const mock = new MockDispatcher();
  return { mock, client: new ConstructsClient(mock as unknown as Dispatcher) };
}

test("listConstructs sends a listConstructs envelope carrying a requestId", async () => {
  const { mock, client } = newClient();
  const pending = client.listConstructs();
  const sent = mock.lastSent() as unknown as Record<string, { requestId?: string }>;
  assert.ok(sent.listConstructs, "envelope must carry a listConstructs payload");
  assert.ok(
    (sent.listConstructs.requestId ?? "").length > 0,
    "the request must carry a requestId so the reply correlates",
  );
  mock.reply({ listConstructsResult: { requestId: sent.listConstructs.requestId, constructs: [] } });
  assert.deepEqual(await pending, { constructs: [] });
});

test("a fully-populated construct round-trips every field", async () => {
  const { mock, client } = newClient();
  const pending = client.listConstructs();
  mock.reply({
    listConstructsResult: {
      requestId: "r",
      constructs: [
        {
          name: "spaceParticipants",
          kind: "query",
          namespace: "cognition",
          origin: "core",
          originPath: "cognition/queries.memql",
          description: "Get space participants",
          runnable: true,
          boundConcept: "v1:cognition:participant",
          sourceHash: "abc123",
          args: [
            {
              name: "spaceId",
              type: "string",
              required: true,
              enumValues: ["a", "b"],
              description: "The space",
              autoInjected: true,
            },
          ],
        },
      ],
    },
  });
  const { constructs } = await pending;
  assert.equal(constructs.length, 1);
  assert.deepEqual(constructs[0], {
    name: "spaceParticipants",
    kind: "query",
    namespace: "cognition",
    origin: "core",
    originPath: "cognition/queries.memql",
    description: "Get space participants",
    runnable: true,
    args: [
      {
        name: "spaceId",
        type: "string",
        required: true,
        enum: ["a", "b"],
        description: "The space",
        autoInjected: true,
      },
    ],
    boundConcept: "v1:cognition:participant",
    sourceHash: "abc123",
    source: "",
  });
});

// The commonest construct in the tree is view-only, and protojson sends it as
// very nearly `{}`. Every field must still arrive typed, and `args` must be an
// array -- a caller iterating `undefined` throws.
test("an all-defaults construct arrives fully typed, with args as an array", async () => {
  const { mock, client } = newClient();
  const pending = client.listConstructs();
  mock.reply({ listConstructsResult: { constructs: [{ name: "spaceCard", kind: "shape" }] } });
  const c = (await pending).constructs[0];
  assert.ok(c, "the reply must carry the construct");
  assert.equal(c.namespace, "");
  assert.equal(c.originPath, "");
  assert.equal(c.description, "");
  assert.equal(c.boundConcept, "");
  assert.equal(c.sourceHash, "");
  assert.equal(c.source, "");
  assert.equal(c.runnable, false);
  assert.ok(Array.isArray(c.args), "args must be an array, never undefined");
  assert.equal(c.args.length, 0);
  assert.equal(c.origin, "core", "an absent origin resolves to core, not undefined");
});

// A promoted construct is the one case with a source and no path. The client
// must not normalise that away, because the Constructs view keys its
// open-the-file behaviour on exactly this pair.
// The trigger (memql#3805) is what decides an automation's run form, so it has
// to survive the decode intact -- and its ABSENCE has to survive too, because
// undefined and an empty trigger are different claims about the automation.
test("an automation carries its trigger through the decode", async () => {
  const { mock, client } = newClient();
  const pending = client.listConstructs();
  mock.reply({
    listConstructsResult: {
      constructs: [
        {
          name: "autoJoinSI",
          kind: "automation",
          runnable: true,
          trigger: { event: "node.created", concept: "v1:cognition:participant" },
        },
      ],
    },
  });
  const c = (await pending).constructs[0]!;
  // The three members are defaulted like every other field: protojson omits an
  // empty string, and a form reading `schedule` raw would get undefined.
  assert.deepEqual(c.trigger, {
    concept: "v1:cognition:participant",
    event: "node.created",
    schedule: "",
  });
});

test("a scheduled automation carries its cron and no event", async () => {
  const { mock, client } = newClient();
  const pending = client.listConstructs();
  mock.reply({
    listConstructsResult: {
      constructs: [{ name: "sweepStale", kind: "automation", trigger: { schedule: "0 */10 * * * *" } }],
    },
  });
  assert.deepEqual((await pending).constructs[0]!.trigger, {
    concept: "",
    event: "",
    schedule: "0 */10 * * * *",
  });
});

// UNDEFINED, not an empty object. Every other field on Construct is defaulted
// into existence; this one must not be, because the run form reads manual-run
// off the absence and an empty trigger is a claim that the automation fires on
// nothing. A view-only construct has none either, and shipping one on all ~900
// of them is the other thing this avoids.
test("a construct with no trigger leaves it undefined rather than empty", async () => {
  const { mock, client } = newClient();
  const pending = client.listConstructs();
  mock.reply({
    listConstructsResult: {
      constructs: [
        { name: "manualOnly", kind: "automation", runnable: true },
        { name: "spaceCard", kind: "shape" },
      ],
    },
  });
  const { constructs } = await pending;
  const automation = constructs[0]!;
  const shape = constructs[1]!;
  assert.equal(automation.trigger, undefined);
  assert.equal(shape.trigger, undefined);
  assert.ok(!("trigger" in shape), "the key itself must be absent, not present-and-undefined");
});

test("a promoted construct keeps its source and reports no path", async () => {
  const { mock, client } = newClient();
  const pending = client.listConstructs();
  mock.reply({
    listConstructsResult: {
      constructs: [
        {
          name: "mySpec",
          kind: "spec",
          origin: "promoted",
          sourceHash: "deadbeef",
          source: "spec actorEnvelope mySpec {\n  return role == \"admin\"\n}",
        },
      ],
    },
  });
  const c = (await pending).constructs[0];
  assert.ok(c, "the reply must carry the construct");
  assert.equal(c.origin, "promoted");
  assert.equal(c.originPath, "", "a promoted construct has no file");
  assert.equal(c.namespace, "");
  assert.match(c.source, /^spec actorEnvelope mySpec/);
});

// An origin outside the closed set means the cluster is newer than this client.
// Rendering it as core is the least wrong of the three, and it spares every
// consumer a default branch on a field the engine owns.
test("an unrecognised origin resolves to core rather than passing through", async () => {
  const { mock, client } = newClient();
  const pending = client.listConstructs();
  mock.reply({
    listConstructsResult: { constructs: [{ name: "x", kind: "query", origin: "quantum" }] },
  });
  const [only] = (await pending).constructs;
  assert.ok(only, "the reply must carry the construct");
  assert.equal(only.origin, "core");
});

test("a queryError reply throws with the engine's message", async () => {
  const { mock, client } = newClient();
  const pending = client.listConstructs();
  mock.reply({ queryError: { requestId: "r", error: { message: "not permitted" } } });
  await assert.rejects(pending, /listConstructs: not permitted/);
});

// A cluster that predates the message answers with an envelope this does not
// recognise. Throwing is what lets a caller render a stated version mismatch;
// resolving with an empty catalog would render as "this cluster has no
// constructs", which is the one wrong answer.
test("an unexpected reply envelope throws rather than resolving empty", async () => {
  const { mock, client } = newClient();
  const pending = client.listConstructs();
  mock.reply({ listPackDomainsResult: { requestId: "r", domains: [] } });
  await assert.rejects(pending, /unexpected reply envelope/);
});
