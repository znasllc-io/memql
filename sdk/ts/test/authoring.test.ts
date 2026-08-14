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

import {
  AuthoringClient,
  DemoteOutcomeRemoved,
  DemoteOutcomeRetired,
  failedDiagnostics,
} from "../src/authoring/index.js";
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

// -----------------------------------------------------------------------------
// Durable promote / demote (memql#3760)
//
// A third risk joins the two the file opens with, and it is the reason both
// replies carry a second list beside the one naming the constructs: FOR A
// CONCEPT, ok=true DOES NOT SAY WHAT HAPPENED. A demoted concept with rows
// under it is retired rather than removed and its name stays claimed; a
// re-promoted concept whose change is breaking is refused, and the refusal
// carries the diff that explains it. Both facts are values on the reply, and a
// consumer that reads them out of the prose in `error` is parsing English.
//
// The fourth is `rowCountKnown`. A node with no database cannot count rows, and
// its `rowsAffected` is then 0 -- not because nothing is affected, but because
// nobody looked. Rendering the count without the flag turns "unknown" into
// "safe" at the one moment that distinction matters.
// -----------------------------------------------------------------------------

test("durablePromoteBundle -- rides durablePromoteBundle with the sources verbatim", async () => {
  const { mock, client } = newClient();
  const sources = "concept invoice {\n  total number\n}\n";
  const promise = client.durablePromoteBundle(sources);
  const payload = envelopeField(mock, "durablePromoteBundle");
  assert.equal(payload.sources, sources);
  assert.equal(typeof payload.requestId, "string");
  assert.notEqual(payload.requestId, "");
  mock.reply({ durablePromoteBundleResult: { requestId: "r", ok: true } });
  await promise;
});

test("durablePromoteBundle -- an ordinary promote carries NO allowBreaking field", async () => {
  const { mock, client } = newClient();
  const promise = client.durablePromoteBundle("concept invoice { }");
  const payload = envelopeField(mock, "durablePromoteBundle");
  // Absent, not false. proto3 reads the two identically, so this is not about
  // what the engine does -- it is about the flag that lands rows-stranding
  // schema changes never appearing on a frame nobody asked for it on.
  assert.equal("allowBreaking" in payload, false);
  mock.reply({ durablePromoteBundleResult: { requestId: "r", ok: true } });
  await promise;
});

test("durablePromoteBundle -- allowBreaking:false stays off the wire; true puts it on", async () => {
  const { mock, client } = newClient();
  const off = client.durablePromoteBundle("concept invoice { }", { allowBreaking: false });
  assert.equal("allowBreaking" in envelopeField(mock, "durablePromoteBundle"), false);
  mock.reply({ durablePromoteBundleResult: { requestId: "r", ok: true } });
  await off;

  const on = client.durablePromoteBundle("concept invoice { }", { allowBreaking: true });
  assert.equal(envelopeField(mock, "durablePromoteBundle").allowBreaking, true);
  mock.reply({ durablePromoteBundleResult: { requestId: "r", ok: true } });
  await on;
});

test("durableDemoteBundle -- rides its OWN envelope field, not promote's", async () => {
  const { mock, client } = newClient();
  const promise = client.durableDemoteBundle("concept invoice { }");
  const sent = mock.lastSent() as unknown as Record<string, unknown>;
  // The two are exact inverses over an identical payload shape, so landing a
  // demote in the promote field would re-register the very construct the caller
  // asked to withdraw -- across the whole cluster, durably.
  assert.equal(sent.durablePromoteBundle, undefined);
  assert.ok(sent.durableDemoteBundle);
  mock.reply({ durableDemoteBundleResult: { requestId: "r", ok: true } });
  await promise;
});

test("durablePromoteBundle -- a refusal resolves with the diff rather than throwing", async () => {
  const { mock, client } = newClient();
  const promise = client.durablePromoteBundle("concept invoice { }");
  // The shape the engine sends when a breaking change is refused: ok=false, the
  // prose error a human reads, AND the structured diff a client renders. A
  // refusal is an ANSWER, so it must not throw -- throwing here would leave the
  // caller with the sentence and none of the values in it.
  mock.reply({
    durablePromoteBundleResult: {
      requestId: "r",
      ok: false,
      error: "durable_promote_bundle: breaking schema change refused",
      conceptDiffs: [
        {
          concept: "v1:billing:invoice",
          breaking: true,
          changes: [
            {
              concept: "v1:billing:invoice",
              field: "total",
              kind: "field_type_changed",
              breaking: true,
              was: "string",
              now: "number",
              rowsAffected: "1204",
              rowCountKnown: true,
              referencedBy: ["query:invoicesForOwner", "mutation:createInvoice"],
              detail: "1204 rows carry a string total",
            },
          ],
          summary: "invoice: 1 breaking change",
        },
      ],
    },
  });
  const result = await promise;
  assert.equal(result.ok, false);
  assert.match(result.error, /breaking schema change refused/);
  assert.deepEqual(result.promoted, []);
  const diff = result.conceptDiffs[0];
  assert.ok(diff);
  assert.equal(diff.concept, "v1:billing:invoice");
  assert.equal(diff.breaking, true);
  assert.equal(diff.overridden, false);
  assert.equal(diff.summary, "invoice: 1 breaking change");
  const change = diff.changes[0];
  assert.ok(change);
  assert.equal(change.field, "total");
  assert.equal(change.kind, "field_type_changed");
  assert.equal(change.was, "string");
  assert.equal(change.now, "number");
  // int64 arrives as a protojson STRING; a consumer sorting or comparing on it
  // would otherwise be doing it lexically.
  assert.equal(change.rowsAffected, 1204);
  assert.equal(change.rowCountKnown, true);
  assert.deepEqual(change.referencedBy, ["query:invoicesForOwner", "mutation:createInvoice"]);
});

test("durablePromoteBundle -- an uncounted change decodes rowCountKnown=false with rowsAffected 0", async () => {
  const { mock, client } = newClient();
  const promise = client.durablePromoteBundle("concept invoice { }");
  // What a node with no database sends: it omits both int64 rowsAffected and
  // the false bool. The pair must survive as (0, false) -- the 0 is NOT a
  // count, and a consumer that renders it without the flag says "0 rows carry
  // it" about a count nobody took.
  mock.reply({
    durablePromoteBundleResult: {
      requestId: "r",
      ok: false,
      conceptDiffs: [
        {
          concept: "v1:billing:invoice",
          breaking: true,
          changes: [{ concept: "v1:billing:invoice", field: "total", kind: "field_removed", breaking: true }],
        },
      ],
    },
  });
  const change = (await promise).conceptDiffs[0]?.changes[0];
  assert.ok(change);
  assert.equal(change.rowsAffected, 0);
  assert.equal(change.rowCountKnown, false);
  // Absent repeated fields decode to empty arrays, never undefined -- consumers
  // map over these directly.
  assert.deepEqual(change.referencedBy, []);
  assert.equal(change.was, "");
  assert.equal(change.detail, "");
});

test("durablePromoteBundle -- an overridden breaking change lands with the diff still attached", async () => {
  const { mock, client } = newClient();
  const promise = client.durablePromoteBundle("concept invoice { }", { allowBreaking: true });
  mock.reply({
    durablePromoteBundleResult: {
      requestId: "r",
      ok: true,
      promoted: [{ kind: "concept", name: "invoice" }],
      conceptDiffs: [{ concept: "v1:billing:invoice", breaking: true, overridden: true }],
    },
  });
  const result = await promise;
  assert.equal(result.ok, true);
  assert.deepEqual(result.promoted, [{ kind: "concept", name: "invoice" }]);
  // breaking && overridden is "it broke something and went through anyway",
  // which is a different thing to show an operator than breaking && !overridden.
  assert.equal(result.conceptDiffs[0]?.breaking, true);
  assert.equal(result.conceptDiffs[0]?.overridden, true);
  assert.deepEqual(result.conceptDiffs[0]?.changes, []);
});

test("durablePromoteBundle -- a first promote yields an empty diff list and an empty error", async () => {
  const { mock, client } = newClient();
  const promise = client.durablePromoteBundle("query q { }");
  mock.reply({
    durablePromoteBundleResult: { requestId: "r", ok: true, promoted: [{ kind: "query", name: "q" }] },
  });
  const result = await promise;
  // Empty means "no concept changed", and it must be an ARRAY -- a consumer
  // that renders `conceptDiffs.length` should read 0, not crash on undefined.
  assert.deepEqual(result.conceptDiffs, []);
  assert.equal(result.error, "");
  assert.deepEqual(result.diagnostics, []);
});

test("durableDemoteBundle -- decodes retire and remove as distinct outcomes", async () => {
  const { mock, client } = newClient();
  const promise = client.durableDemoteBundle("concept invoice { }\nconcept draft { }\n");
  mock.reply({
    durableDemoteBundleResult: {
      requestId: "r",
      ok: true,
      demoted: [
        { kind: "concept", name: "invoice" },
        { kind: "concept", name: "draft" },
      ],
      outcomes: [
        {
          kind: "concept",
          name: "invoice",
          conceptId: "v1:billing:invoice",
          outcome: "retired",
          rowCount: "1204",
        },
        { kind: "concept", name: "draft", conceptId: "v1:billing:draft", outcome: "removed" },
      ],
    },
  });
  const result = await promise;
  // Both are ok=true. Nothing in the success says the first name is still
  // claimed and the second is free again -- only the outcomes do.
  assert.equal(result.ok, true);
  assert.equal(result.outcomes.length, 2);
  assert.equal(result.outcomes[0]?.outcome, DemoteOutcomeRetired);
  assert.equal(result.outcomes[0]?.conceptId, "v1:billing:invoice");
  assert.equal(result.outcomes[0]?.rowCount, 1204);
  assert.equal(result.outcomes[1]?.outcome, DemoteOutcomeRemoved);
  assert.equal(result.outcomes[1]?.rowCount, 0);
  // demoted stays the identity list and must line up index for index with
  // outcomes -- the engine builds both from one slice in one loop.
  assert.deepEqual(
    result.demoted.map((c) => c.name),
    result.outcomes.map((o) => o.name),
  );
});

test("DemoteOutcomeRetired / DemoteOutcomeRemoved -- match the engine vocabulary", () => {
  // Pinned so a consumer comparing against the exported constants is comparing
  // against the strings the engine actually sends. If the engine's vocabulary
  // ever moves, this is the line that has to move with it.
  assert.equal(DemoteOutcomeRetired, "retired");
  assert.equal(DemoteOutcomeRemoved, "removed");
});

test("durableDemoteBundle -- a non-concept outcome carries no concept id and no rows", async () => {
  const { mock, client } = newClient();
  const promise = client.durableDemoteBundle("query q { }");
  mock.reply({
    durableDemoteBundleResult: {
      requestId: "r",
      ok: true,
      demoted: [{ kind: "query", name: "q" }],
      outcomes: [{ kind: "query", name: "q", outcome: "removed" }],
    },
  });
  const outcome = (await promise).outcomes[0];
  assert.ok(outcome);
  // Empty string, not undefined: a consumer branches on `conceptId !== ""` to
  // decide whether the row-count story applies at all.
  assert.equal(outcome.conceptId, "");
  assert.equal(outcome.rowCount, 0);
});

test("durableDemoteBundle -- an outcome value this build does not know passes through verbatim", async () => {
  const { mock, client } = newClient();
  const promise = client.durableDemoteBundle("concept invoice { }");
  mock.reply({
    durableDemoteBundleResult: {
      requestId: "r",
      ok: true,
      outcomes: [{ kind: "concept", name: "invoice", outcome: "quarantined" }],
    },
  });
  // An outcome the SDK has never heard of is a fact about the CLUSTER, and
  // normalising it to "" or to one of the two known values would hide a version
  // skew behind a plausible-looking answer.
  assert.equal((await promise).outcomes[0]?.outcome, "quarantined");
});

test("durableDemoteBundle -- a rejection reports the error with nothing demoted", async () => {
  const { mock, client } = newClient();
  const promise = client.durableDemoteBundle("query q { }");
  mock.reply({
    durableDemoteBundleResult: {
      requestId: "r",
      ok: false,
      error: "durable_demote_bundle: no demotable constructs in source",
    },
  });
  const result = await promise;
  assert.equal(result.ok, false);
  assert.match(result.error, /no demotable constructs/);
  assert.deepEqual(result.demoted, []);
  assert.deepEqual(result.outcomes, []);
});

test("durableDemoteBundle -- a successful demote reports error as the empty string", async () => {
  const { mock, client } = newClient();
  const promise = client.durableDemoteBundle("query q { }");
  mock.reply({ durableDemoteBundleResult: { requestId: "r", ok: true } });
  assert.equal((await promise).error, "");
});

// -----------------------------------------------------------------------------
// Durable failure paths
// -----------------------------------------------------------------------------

test("durablePromoteBundle -- the owner-only refusal surfaces the engine's message", async () => {
  const { mock, client } = newClient();
  const promise = client.durablePromoteBundle("query q { }");
  // The engine gates promote on OWNER (stricter than the owner-or-developer bar
  // define clears) and refuses with a PermissionDenied queryError. The client
  // does not pre-empt that check, so this path is how a non-owner learns -- and
  // the engine's own wording, naming the role, is what has to reach them.
  mock.reply({ queryError: { requestId: "r", error: { message: "durable promotion requires the owner role" } } });
  await assert.rejects(promise, /durablePromoteBundle: durable promotion requires the owner role/);
});

test("durableDemoteBundle -- the owner-only refusal surfaces the engine's message", async () => {
  const { mock, client } = newClient();
  const promise = client.durableDemoteBundle("query q { }");
  mock.reply({ queryError: { requestId: "r", error: { message: "durable promotion requires the owner role" } } });
  await assert.rejects(promise, /durableDemoteBundle: durable promotion requires the owner role/);
});

test("durablePromoteBundle -- an unrelated reply envelope throws rather than resolving empty", async () => {
  const { mock, client } = newClient();
  const promise = client.durablePromoteBundle("query q { }");
  // Resolving an empty result would report "nothing was promoted, no concept
  // changed" for a call whose outcome is genuinely unknown -- and this call
  // durably changes every node in the cluster.
  mock.reply({ heartbeat: {} });
  await assert.rejects(promise, /durablePromoteBundle: unexpected reply envelope/);
});

test("durableDemoteBundle -- a promote reply is not accepted as a demote reply", async () => {
  const { mock, client } = newClient();
  const promise = client.durableDemoteBundle("query q { }");
  // Near-identical reply shapes for exactly inverse operations: taking one for
  // the other would report a withdrawal that never happened.
  mock.reply({ durablePromoteBundleResult: { requestId: "r", ok: true } });
  await assert.rejects(promise, /durableDemoteBundle: unexpected reply envelope/);
});
