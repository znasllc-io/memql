// Mock-dispatcher tests for the authoring surface (memql#2128 / C1, consumed
// by memql#3309).
//
// Two things carry the risk here, and they are both about how a consumer
// READS the reply rather than how the request is shaped.
//
// First, ZERO MEANS "NO POSITION". protojson omits a zero int32, so a
// diagnostic with no reliable position arrives with `line` simply absent. If
// that decodes to anything other than the number 0 -- undefined, NaN -- the
// consumer's "is there a position" test breaks; and if the consumer reads the
// 0 as a coordinate, every positionless diagnostic lands on line 0 of the
// bundle's first file. The engine deliberately emits no position rather than a
// wrong one (memql#2375), so this is the common case, not an edge.
//
// Second, SKIPPED IS NOT FAILED. A bundle routinely carries constructs whose
// kind the pass does not compile, and each reports ok=false WITH skipped=true
// without failing the bundle. A consumer filtering on `!ok` renders those as
// compile errors on a bundle the engine called valid, which is why
// failedDiagnostics exists and is tested here rather than left to each caller.

import test from "node:test";
import assert from "node:assert/strict";

import { AuthoringClient, failedDiagnostics } from "../src/authoring/index.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

// MockDispatcher mirrors the stand-in deploy.test.ts and identity.test.ts use:
// only the methods this surface touches, kept local so each test file stays
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

  async sendAndWait(msg: ClientMessage, signal?: AbortSignal): Promise<ServerMessage> {
    const id = msg.messageId ?? `mock-${this.nextId++}`;
    this.sent.push({ msg: { ...msg, messageId: id }, messageId: id });
    return new Promise<ServerMessage>((resolve, reject) => {
      if (signal?.aborted) {
        reject(new Error("aborted"));
        return;
      }
      this.pendingReplies.set(id, resolve);
      if (signal) {
        signal.addEventListener(
          "abort",
          () => {
            this.pendingReplies.delete(id);
            reject(new Error("aborted"));
          },
          { once: true },
        );
      }
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

function newClient(): { mock: MockDispatcher; client: AuthoringClient } {
  const mock = new MockDispatcher();
  return { mock, client: new AuthoringClient(mock as unknown as Dispatcher) };
}

function envelopeField(mock: MockDispatcher, field: string): Record<string, unknown> {
  const sent = mock.lastSent() as unknown as Record<string, unknown>;
  const block = sent[field] as Record<string, unknown> | undefined;
  assert.ok(block, `envelope must carry a ${field} payload`);
  return block;
}

// -----------------------------------------------------------------------------
// Envelope shape
// -----------------------------------------------------------------------------

test("validateBundle -- rides authoringValidateBundle with the sources verbatim", async () => {
  const { mock, client } = newClient();
  const sources = "query space listSpaces {\n  filter true\n}\n";
  const promise = client.validateBundle(sources);
  const payload = envelopeField(mock, "authoringValidateBundle");
  assert.equal(payload.sources, sources);
  assert.equal(typeof payload.requestId, "string");
  assert.notEqual(payload.requestId, "");
  mock.reply({ authoringValidateBundleResult: { requestId: "r", ok: true } });
  await promise;
});

test("sessionDefineBundle -- rides its OWN envelope field, not validate's", async () => {
  const { mock, client } = newClient();
  const promise = client.sessionDefineBundle("logic x { }");
  const sent = mock.lastSent() as unknown as Record<string, unknown>;
  // The two messages are near-identical in shape and wildly different in
  // effect -- one mutates the caller's registry and one cannot. Landing a
  // define in validate's field would silently downgrade every run to
  // "validated but never injected", which then executes the DEPLOYED
  // construct instead of the buffer.
  assert.equal(sent.authoringValidateBundle, undefined);
  assert.ok(sent.authoringSessionDefineBundle);
  mock.reply({ authoringSessionDefineBundleResult: { requestId: "r", ok: true } });
  await promise;
});

// -----------------------------------------------------------------------------
// Reply decoding
// -----------------------------------------------------------------------------

test("validateBundle -- absent numeric position fields decode to 0, not undefined", async () => {
  const { mock, client } = newClient();
  const promise = client.validateBundle("query q { }");
  // Exactly what protojson emits for a diagnostic the engine could not
  // position: the four int32 fields are simply not on the wire.
  mock.reply({
    authoringValidateBundleResult: {
      requestId: "r",
      ok: false,
      diagnostics: [{ name: "q", kind: "query", ok: false, error: "boom" }],
    },
  });
  const result = await promise;
  const d = result.diagnostics[0];
  assert.ok(d);
  assert.equal(d.line, 0);
  assert.equal(d.column, 0);
  assert.equal(d.endLine, 0);
  assert.equal(d.endColumn, 0);
  assert.equal(d.skipped, false);
  assert.equal(d.error, "boom");
});

test("validateBundle -- stringified int32 positions decode to numbers", async () => {
  const { mock, client } = newClient();
  const promise = client.validateBundle("query q { }");
  // Some protojson runtimes stringify integers. A consumer doing arithmetic
  // on the result (bundle line -> buffer line) would get "121" from "12" - 1
  // coerced the wrong way, so the coercion belongs here, once.
  mock.reply({
    authoringValidateBundleResult: {
      requestId: "r",
      ok: false,
      diagnostics: [
        { name: "q", kind: "query", ok: false, error: "boom", line: "12", column: "4" },
      ],
    },
  });
  const d = (await promise).diagnostics[0];
  assert.ok(d);
  assert.equal(d.line, 12);
  assert.equal(d.column, 4);
});

test("validateBundle -- ok=true with no diagnostics array yields an empty list", async () => {
  const { mock, client } = newClient();
  const promise = client.validateBundle("query q { }");
  mock.reply({ authoringValidateBundleResult: { requestId: "r", ok: true } });
  const result = await promise;
  assert.equal(result.ok, true);
  assert.deepEqual(result.diagnostics, []);
});

test("sessionDefineBundle -- decodes defined constructs and the bundle-level error", async () => {
  const { mock, client } = newClient();
  const promise = client.sessionDefineBundle("query q { }");
  mock.reply({
    authoringSessionDefineBundleResult: {
      requestId: "r",
      ok: false,
      defined: [{ kind: "query", name: "q" }],
      error: "bundle rejected",
    },
  });
  const result = await promise;
  assert.equal(result.ok, false);
  assert.deepEqual(result.defined, [{ kind: "query", name: "q" }]);
  assert.equal(result.error, "bundle rejected");
  assert.deepEqual(result.diagnostics, []);
});

test("sessionDefineBundle -- a successful define reports error as the empty string", async () => {
  const { mock, client } = newClient();
  const promise = client.sessionDefineBundle("query q { }");
  mock.reply({ authoringSessionDefineBundleResult: { requestId: "r", ok: true } });
  const result = await promise;
  // Not undefined: consumers branch on `error !== ""`, and an undefined here
  // would make that test pass for a rejection that simply omitted the field.
  assert.equal(result.error, "");
});

// -----------------------------------------------------------------------------
// Failure paths
// -----------------------------------------------------------------------------

test("validateBundle -- a queryError reply throws with the engine message", async () => {
  const { mock, client } = newClient();
  const promise = client.validateBundle("query q { }");
  mock.reply({ authoringValidateBundleResult: undefined, queryError: { requestId: "r", error: { message: "ERR-a1b2c3 nope" } } });
  await assert.rejects(promise, /validateBundle: ERR-a1b2c3 nope/);
});

test("sessionDefineBundle -- an unrelated reply envelope throws rather than resolving empty", async () => {
  const { mock, client } = newClient();
  const promise = client.sessionDefineBundle("query q { }");
  // Resolving an empty result here would report "nothing was defined" for a
  // call whose outcome is genuinely unknown -- and the caller would then
  // invoke by name and silently hit the deployed construct.
  mock.reply({ heartbeat: {} });
  await assert.rejects(promise, /unexpected reply envelope/);
});

test("AuthoringClient -- refuses construction without a dispatcher", () => {
  assert.throws(
    () => new AuthoringClient(undefined as unknown as Dispatcher),
    /dispatcher is required/,
  );
});

// -----------------------------------------------------------------------------
// failedDiagnostics
// -----------------------------------------------------------------------------

test("failedDiagnostics -- a skipped construct is not a failure", () => {
  const diagnostics = [
    { name: "s", kind: "shape", ok: false, skipped: true, error: "kind not compiled", line: 0, column: 0, endLine: 0, endColumn: 0 },
    { name: "q", kind: "query", ok: true, skipped: false, error: "", line: 0, column: 0, endLine: 0, endColumn: 0 },
    { name: "m", kind: "mutation", ok: false, skipped: false, error: "boom", line: 3, column: 1, endLine: 0, endColumn: 0 },
  ];
  const failed = failedDiagnostics(diagnostics);
  assert.equal(failed.length, 1);
  assert.equal(failed[0]?.name, "m");
});

test("failedDiagnostics -- an all-clean set yields nothing", () => {
  const failed = failedDiagnostics([
    { name: "q", kind: "query", ok: true, skipped: false, error: "", line: 0, column: 0, endLine: 0, endColumn: 0 },
  ]);
  assert.deepEqual(failed, []);
});
