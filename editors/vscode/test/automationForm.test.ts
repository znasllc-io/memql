// The automation form's model (memql#3310).
//
// The thing worth pinning here is the MODE DECISION, because it is where the
// form can be wrong in a way nobody notices in review: a scheduled automation
// shown a row picker over a concept that does not exist, or a glob-triggered
// one shown a picker over a concept it never named. Both would look like
// working UI and produce a run the engine refuses.
//
// The payload parse is the other half. It runs BEFORE anything is sent, so a
// typo is a form error rather than a failed run against a cluster -- which
// means the "not an object" and "not JSON" cases have to be caught here and
// not left to the engine.

import test from "node:test";
import assert from "node:assert/strict";

import {
  DEPLOYED_DEFINITION_FALLBACK,
  automationConfirmationMessage,
  automationFormPlan,
  definitionBanner,
  parsePayloadText,
  payloadTextForRow,
} from "../src/state/automationForm.js";

// -----------------------------------------------------------------------------
// automationFormPlan
// -----------------------------------------------------------------------------

test("automationFormPlan -- a scheduled automation collapses to a single confirm", () => {
  // The acceptance criterion, verbatim: @trigger(schedule=...) automations
  // fire now with an empty event. No row picker, no payload box, no concept.
  const plan = automationFormPlan("reapStaleSessions", { schedule: "0 */10 * * * *" });
  assert.deepEqual(plan.modes, ["schedule"]);
  assert.equal(plan.defaultMode, "schedule");
  assert.equal(plan.conceptId, "");
  assert.equal(plan.triggerKind, "schedule");
  assert.match(plan.explanation, /empty event/);
});

test("automationFormPlan -- an event trigger naming a concept offers the row picker first", () => {
  const plan = automationFormPlan("autoJoinSI", {
    event: "node.created",
    concept: "v1:cognition:participant",
  });
  assert.deepEqual(plan.modes, ["row", "json"]);
  // Row FIRST and by default: it is the option that makes an automation
  // genuinely testable, and it reuses a browser that already exists.
  assert.equal(plan.defaultMode, "row");
  assert.equal(plan.conceptId, "v1:cognition:participant");
  assert.equal(plan.triggerKind, "event");
});

test("automationFormPlan -- an event trigger naming no concept offers JSON only", () => {
  // There is no row set to pick from, so offering a picker would be offering
  // a browser over nothing.
  const plan = automationFormPlan("onAnything", { event: "graph.node.created.*" });
  assert.deepEqual(plan.modes, ["json"]);
  assert.equal(plan.conceptId, "");
  assert.equal(plan.triggerKind, "event");
});

test("automationFormPlan -- no reported trigger degrades to a manual JSON run", () => {
  // A trigger this build cannot read is an ordinary situation (an older or
  // newer language server), and it must not leave the form inert.
  const plan = automationFormPlan("mystery", undefined);
  assert.deepEqual(plan.modes, ["json"]);
  assert.equal(plan.triggerKind, "manual");
});

test("automationFormPlan -- a trigger carrying both an event and a schedule is treated as event-driven", () => {
  // Matches the engine, which asks IsScheduled() only after
  // IsEventTriggered() has said no. Disagreeing would build a form for a run
  // the engine will not perform.
  const plan = automationFormPlan("both", {
    event: "node.created",
    concept: "v1:cognition:space",
    schedule: "0 * * * * *",
  });
  assert.equal(plan.triggerKind, "event");
  assert.deepEqual(plan.modes, ["row", "json"]);
});

// -----------------------------------------------------------------------------
// parsePayloadText
// -----------------------------------------------------------------------------

test("parsePayloadText -- empty text OMITS the payload rather than sending {}", () => {
  // The SDK documents "omit it for a fire-now run", and an omitted field and
  // an empty object are not the same request even where the engine happens to
  // treat them alike.
  const parsed = parsePayloadText("   \n  ");
  assert.equal(parsed.ok, true);
  assert.equal(parsed.ok && parsed.payload, undefined);
});

test("parsePayloadText -- a JSON object is the payload", () => {
  const parsed = parsePayloadText('{"id": "v1:cognition:participant:abc", "payload": {"role": "human"}}');
  assert.equal(parsed.ok, true);
  assert.deepEqual(parsed.ok && parsed.payload, {
    id: "v1:cognition:participant:abc",
    payload: { role: "human" },
  });
});

test("parsePayloadText -- a parse error is reported inline, not as a run failure", () => {
  const parsed = parsePayloadText("{ nope");
  assert.equal(parsed.ok, false);
  assert.match(parsed.ok ? "" : parsed.error, /not valid JSON/);
});

test("parsePayloadText -- an array or a scalar is refused", () => {
  // The wire field is a struct, so neither can be carried as an event payload
  // at all. Catching it here names the box; the engine's refusal would name
  // the automation.
  for (const text of ["[1, 2]", '"a string"', "42", "null", "true"]) {
    const parsed = parsePayloadText(text);
    assert.equal(parsed.ok, false, `expected ${text} to be refused`);
    assert.match(parsed.ok ? "" : parsed.error, /JSON object/);
  }
});

// -----------------------------------------------------------------------------
// payloadTextForRow
// -----------------------------------------------------------------------------

test("payloadTextForRow -- the row goes in WHOLE, nesting intact", () => {
  // Not flattened. A real graph event carries the row envelope with `payload`
  // nested inside it, and an automation body reads
  // args.payload.payload.<field> accordingly -- so a flattened projection
  // would be a payload no real firing ever delivers, and an automation tested
  // against it would pass here and fail in production.
  const text = payloadTextForRow({
    id: "v1:cognition:participant:abc",
    concept: "v1:cognition:participant",
    payload: { role: "human", spaceId: "v1:cognition:space:xyz" },
  });
  const round = JSON.parse(text) as Record<string, unknown>;
  assert.equal(round.id, "v1:cognition:participant:abc");
  assert.deepEqual(round.payload, { role: "human", spaceId: "v1:cognition:space:xyz" });
});

// -----------------------------------------------------------------------------
// The deployed-definition banner
// -----------------------------------------------------------------------------

test("definitionBanner -- renders the ENGINE's note when there is one", () => {
  // The note is ready-to-render text owned by the side that knows what
  // actually ran; the client must not paraphrase it.
  const banner = definitionBanner({
    ranDeployedDefinition: true,
    definitionNote: "Automations are not session-definable: the DEPLOYED definition ran.",
  });
  assert.equal(banner, "Automations are not session-definable: the DEPLOYED definition ran.");
});

test("definitionBanner -- falls back rather than going silent on an empty note", () => {
  // "The UI states that the deployed definition ran" is an acceptance
  // criterion, so it must not disappear because a frame arrived without text.
  assert.equal(
    definitionBanner({ ranDeployedDefinition: true, definitionNote: "  " }),
    DEPLOYED_DEFINITION_FALLBACK,
  );
});

test("definitionBanner -- is GATED on ranDeployedDefinition", () => {
  // Rendering it unconditionally would make the flag decorative, and a future
  // run that genuinely did execute a buffer would carry a banner saying the
  // opposite.
  assert.equal(definitionBanner({ ranDeployedDefinition: false, definitionNote: "x" }), "");
});

// -----------------------------------------------------------------------------
// The non-local write confirmation
// -----------------------------------------------------------------------------

test("automationConfirmationMessage -- names the automation, the cluster, and the blast radius", () => {
  const message = automationConfirmationMessage("autoJoinSI", "staging");
  assert.match(message, /autoJoinSI/);
  assert.match(message, /staging/);
  // The difference from the mutation prompt, and the reason automations count
  // as a write kind at all: a mutation writes the rows it declares, an
  // automation runs its whole action chain.
  assert.match(message, /whole action chain/);
});
