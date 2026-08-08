// The `memql/runnableConstructs` contract, from the consumer's side.
//
// The language server is a SEPARATE PROCESS and possibly an older build --
// memql.lsp.serverPath points at a user-chosen binary and the PATH fallback is
// whatever happens to be installed. So its reply is untrusted in the ordinary
// sense, and every degradation below is a real situation rather than a
// hypothetical: a kind this build does not know, a null where the contract
// promises an array, a type name that has since been added.
//
// The other half of this file is what the lens set may contain. "CodeLens
// appears on runnable signatures and nowhere else" is an acceptance criterion,
// and the one way to violate it that would not be obvious in review is
// putting an automation on the ORDINARY run path (memql#3310): that path
// session-defines the buffer and renders rows, and an automation can do
// neither -- it would quietly invoke the DEPLOYED automation and present the
// result as the buffer's.

import test from "node:test";
import assert from "node:assert/strict";

import {
  COMMAND_RUN,
  COMMAND_RUN_AUTOMATION,
  COMMAND_RUN_WITH,
  usesArgForm,
  isSessionDefinable,
  isWriteKind,
  lensPlansFor,
  parseRunnableConstructs,
} from "../src/constructs/runnable.js";

const RANGE = { start: { line: 3, character: 0 }, end: { line: 3, character: 24 } };

function wireConstruct(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    kind: "query",
    name: "spaceParticipants",
    signatureRange: RANGE,
    args: [],
    ...overrides,
  };
}

// -----------------------------------------------------------------------------
// parseRunnableConstructs
// -----------------------------------------------------------------------------

test("parseRunnableConstructs -- decodes a full construct", () => {
  const out = parseRunnableConstructs({
    constructs: [
      wireConstruct({
        concept: "participant",
        args: [
          { name: "spaceId", type: "string", required: true, description: "The space." },
          { name: "status", type: "string", required: false, enum: ["active", "left"] },
        ],
      }),
    ],
  });
  assert.equal(out.length, 1);
  const c = out[0];
  assert.ok(c);
  assert.equal(c.kind, "query");
  assert.equal(c.name, "spaceParticipants");
  assert.equal(c.concept, "participant");
  assert.deepEqual(c.signatureRange, RANGE);
  assert.equal(c.args.length, 2);
  assert.equal(c.args[0]?.required, true);
  assert.equal(c.args[0]?.description, "The space.");
  assert.deepEqual(c.args[1]?.enum, ["active", "left"]);
  assert.equal(c.args[1]?.required, false);
});

test("parseRunnableConstructs -- a non-object result yields an empty list", () => {
  // The LSP client resolves to null for a server that answered nothing. That
  // is the ordinary state while the server is still starting, not an error.
  assert.deepEqual(parseRunnableConstructs(null), []);
  assert.deepEqual(parseRunnableConstructs(undefined), []);
  assert.deepEqual(parseRunnableConstructs("constructs"), []);
  assert.deepEqual(parseRunnableConstructs({ constructs: null }), []);
});

test("parseRunnableConstructs -- an unknown kind is dropped, the rest survive", () => {
  // A future server adding a sixth runnable kind must not take this build's
  // lenses down with it.
  const out = parseRunnableConstructs({
    constructs: [wireConstruct({ kind: "spec", name: "isActive" }), wireConstruct()],
  });
  assert.equal(out.length, 1);
  assert.equal(out[0]?.name, "spaceParticipants");
});

test("parseRunnableConstructs -- a construct with no usable range is dropped", () => {
  // A lens with no anchor has nowhere to render; emitting it would either
  // throw inside the provider or park the affordance at the top of the file
  // pointing at an unrelated construct.
  const out = parseRunnableConstructs({
    constructs: [
      wireConstruct({ signatureRange: undefined }),
      wireConstruct({ signatureRange: { start: { line: 1, character: 0 } } }),
    ],
  });
  assert.deepEqual(out, []);
});

test("parseRunnableConstructs -- an unknown arg type degrades to 'any', it does not drop the arg", () => {
  // Dropping the arg would generate a form the compiler does not agree with,
  // which is exactly the failure the single-parser rule exists to prevent. A
  // widened box merely asks the developer to type the value.
  const out = parseRunnableConstructs({
    constructs: [wireConstruct({ args: [{ name: "when", type: "duration", required: true }] })],
  });
  assert.equal(out[0]?.args[0]?.type, "any");
  assert.equal(out[0]?.args[0]?.required, true);
});

test("parseRunnableConstructs -- a missing args array reads as no arguments", () => {
  const out = parseRunnableConstructs({ constructs: [wireConstruct({ args: undefined })] });
  assert.deepEqual(out[0]?.args, []);
});

test("parseRunnableConstructs -- an anonymous arg is dropped without dropping the construct", () => {
  const out = parseRunnableConstructs({
    constructs: [
      wireConstruct({
        args: [{ type: "string", required: true }, { name: "ok", type: "string", required: false }],
      }),
    ],
  });
  assert.equal(out.length, 1);
  assert.equal(out[0]?.args.length, 1);
  assert.equal(out[0]?.args[0]?.name, "ok");
});

test("parseRunnableConstructs -- an automation carries its trigger", () => {
  const out = parseRunnableConstructs({
    constructs: [
      wireConstruct({
        kind: "automation",
        name: "autoJoin",
        trigger: { concept: "v1:cognition:participant", event: "node.created" },
      }),
    ],
  });
  assert.deepEqual(out[0]?.trigger, {
    concept: "v1:cognition:participant",
    event: "node.created",
  });
});

test("parseRunnableConstructs -- an empty trigger object is not carried", () => {
  // `trigger: {}` on the wire means every field was omitted, which is
  // indistinguishable from no trigger. Keeping the empty object would make
  // `construct.trigger !== undefined` a lie for the consumer.
  const out = parseRunnableConstructs({
    constructs: [wireConstruct({ kind: "automation", trigger: {} })],
  });
  assert.equal(out[0]?.trigger, undefined);
});

// -----------------------------------------------------------------------------
// Kind classification
// -----------------------------------------------------------------------------

test("isSessionDefinable -- only the plain construct family", () => {
  assert.equal(isSessionDefinable("query"), true);
  assert.equal(isSessionDefinable("mutate"), true);
  assert.equal(isSessionDefinable("logic"), true);
  // A tool is bound to a Go handler and an automation is event-triggered;
  // neither can be injected from a buffer, and treating either as definable
  // would make the result view claim the buffer ran when it did not.
  assert.equal(isSessionDefinable("tool"), false);
  assert.equal(isSessionDefinable("automation"), false);
});

test("isWriteKind -- mutations and automations", () => {
  assert.equal(isWriteKind("mutate"), true);
  // memql#3310: an automation run executes the automation's whole action
  // chain -- writes, LLM calls, downstream automations -- so it earns the
  // non-local confirmation several times over.
  assert.equal(isWriteKind("automation"), true);
  assert.equal(isWriteKind("query"), false);
  // Deliberate: a logic body can call a mutation, but prompting on every
  // logic run trains the developer to dismiss the dialog unread, which is how
  // the mutation prompt stops working.
  assert.equal(isWriteKind("logic"), false);
  assert.equal(isWriteKind("tool"), false);
});

test("usesArgForm -- every kind but automation generates its form from `args`", () => {
  assert.equal(usesArgForm("query"), true);
  assert.equal(usesArgForm("tool"), true);
  // An automation binds its whole triggering event as `args`, so the LSP
  // reports args: [] for every one of them and a generated form would be an
  // empty form. Its surface is state/automationForm.ts instead.
  assert.equal(usesArgForm("automation"), false);
});

// -----------------------------------------------------------------------------
// lensPlansFor
// -----------------------------------------------------------------------------

test("lensPlansFor -- a runnable construct gets Run and Run with...", () => {
  const constructs = parseRunnableConstructs({ constructs: [wireConstruct()] });
  const plans = lensPlansFor("file:///q.memql", constructs);
  assert.equal(plans.length, 2);
  assert.equal(plans[0]?.title, "Run");
  assert.equal(plans[0]?.command, COMMAND_RUN);
  assert.equal(plans[1]?.title, "Run with...");
  assert.equal(plans[1]?.command, COMMAND_RUN_WITH);
  // Both anchor to the SIGNATURE, not the body -- that is what "appears on
  // runnable signatures" means concretely.
  assert.deepEqual(plans[0]?.range, RANGE);
  assert.deepEqual(plans[1]?.range, RANGE);
});

test("lensPlansFor -- the target carries the uri, kind, name and declared args", () => {
  const constructs = parseRunnableConstructs({
    constructs: [wireConstruct({ args: [{ name: "spaceId", type: "string", required: true }] })],
  });
  const plans = lensPlansFor("file:///q.memql", constructs);
  assert.deepEqual(plans[0]?.target, {
    uri: "file:///q.memql",
    kind: "query",
    name: "spaceParticipants",
    args: [{ name: "spaceId", type: "string", required: true }],
  });
});

test("lensPlansFor -- an automation gets ONE lens, on the automation command", () => {
  // The point of this test: an automation must never end up on COMMAND_RUN.
  // That path session-defines the buffer and renders rows; an automation can
  // be neither injected nor rendered as rows, so it would silently invoke the
  // DEPLOYED automation and present the result as the buffer's.
  //
  // ONE lens, not two: there is no "Run" / "Run with..." split, because there
  // is no such thing as running an automation without first building a trigger
  // event -- the form is the only entry point.
  const constructs = parseRunnableConstructs({
    constructs: [
      wireConstruct({
        kind: "automation",
        name: "autoJoinSI",
        trigger: { event: "node.created", concept: "v1:cognition:participant" },
      }),
    ],
  });
  const plans = lensPlansFor("file:///a.memql", constructs);
  assert.equal(plans.length, 1);
  assert.equal(plans[0]?.command, COMMAND_RUN_AUTOMATION);
  // The AUTOMATION target, not a RunTarget: the trigger is what decides the
  // form, and `args` is always empty.
  assert.equal(plans[0]?.target, undefined);
  assert.deepEqual(plans[0]?.automationTarget, {
    uri: "file:///a.memql",
    name: "autoJoinSI",
    trigger: { event: "node.created", concept: "v1:cognition:participant" },
  });
  // Said BEFORE the click, as the tool lens says it: by the time a banner is
  // on the results surface the developer has already run the thing.
  assert.match(plans[0]?.tooltip ?? "", /DEPLOYED/);
});

test("lensPlansFor -- a scheduled automation's tooltip says it fires now with an empty event", () => {
  const constructs = parseRunnableConstructs({
    constructs: [
      wireConstruct({
        kind: "automation",
        name: "reapStale",
        trigger: { schedule: "0 */10 * * * *" },
      }),
    ],
  });
  const plans = lensPlansFor("file:///a.memql", constructs);
  assert.match(plans[0]?.tooltip ?? "", /empty event/);
});

test("lensPlansFor -- no constructs means no lenses", () => {
  // A half-typed buffer yields an empty list by design (the server omits a
  // construct whose own text does not parse), so the absence of lenses is
  // normal rather than an error state to render.
  assert.deepEqual(lensPlansFor("file:///q.memql", []), []);
});

test("lensPlansFor -- a tool's Run tooltip says the deployed definition runs", () => {
  const constructs = parseRunnableConstructs({
    constructs: [wireConstruct({ kind: "tool", name: "searchUsers" })],
  });
  const plans = lensPlansFor("file:///t.memql", constructs);
  assert.match(plans[0]?.tooltip ?? "", /DEPLOYED/);
});
