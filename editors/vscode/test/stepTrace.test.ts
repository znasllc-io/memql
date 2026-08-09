// The step trace's model (memql#3310).
//
// Two properties carry the weight, and neither is visible by looking at a
// screenshot of a working run:
//
//  - ORDER IS `sequence`, NOT ARRIVAL. The trace rides the event bus on the
//    cross-node path, so out-of-order and duplicate frames are properties of
//    at-least-once delivery rather than bugs. A timeline that rendered arrival
//    order would be right on a laptop and wrong in the mesh.
//  - A REFUSAL IS NOT A FAILED RUN. Folding them together tells a developer
//    their automation is broken when it never started.

import test from "node:test";
import assert from "node:assert/strict";

import type {
  AutomationRunAccepted,
  AutomationRunComplete,
  AutomationRunStep,
} from "@znasllc-io/memql-sdk-core/automation";

import {
  StepTraceModel,
  describeRefusal,
  formatDuration,
} from "../src/state/stepTrace.js";

function step(overrides: Partial<AutomationRunStep> = {}): AutomationRunStep {
  return {
    sequence: 0,
    stepId: "s",
    status: "success",
    durationMs: 5,
    error: "",
    ...overrides,
  };
}

function accepted(overrides: Partial<AutomationRunAccepted> = {}): AutomationRunAccepted {
  return {
    automation: "autoJoinSI",
    ranDeployedDefinition: true,
    definitionNote: "the DEPLOYED definition ran",
    triggerKind: "event",
    triggerTopic: "node.created.v1:cognition:participant",
    requestedOnNodeId: "bff-0",
    requestedOnNodeType: "bff",
    targetNodeType: "",
    ...overrides,
  };
}

function complete(overrides: Partial<AutomationRunComplete> = {}): AutomationRunComplete {
  return {
    status: "completed",
    durationMs: 42,
    stepCount: 2,
    error: "",
    executedOnNodeId: "bff-0",
    executedOnNodeType: "bff",
    ...overrides,
  };
}

// -----------------------------------------------------------------------------
// Ordering
// -----------------------------------------------------------------------------

test("StepTraceModel -- steps render in `sequence` order regardless of arrival", () => {
  const trace = new StepTraceModel();
  trace.noteStep(step({ sequence: 2, stepId: "third" }));
  trace.noteStep(step({ sequence: 0, stepId: "first" }));
  trace.noteStep(step({ sequence: 1, stepId: "second" }));
  assert.deepEqual(
    trace.steps.map((s) => s.stepId),
    ["first", "second", "third"],
  );
});

test("StepTraceModel -- a redelivered step is not a duplicate row", () => {
  // At-least-once delivery on the relay path: the same sequence arriving twice
  // is expected, and appending would draw the step twice on the timeline.
  const trace = new StepTraceModel();
  trace.noteStep(step({ sequence: 0, stepId: "join", status: "success" }));
  trace.noteStep(step({ sequence: 0, stepId: "join", status: "success" }));
  assert.equal(trace.steps.length, 1);
});

// -----------------------------------------------------------------------------
// Status
// -----------------------------------------------------------------------------

test("StepTraceModel -- a fresh trace is running, so the panel can open before the first step", () => {
  // `onAccepted` fires ahead of any step. A model that could only be built
  // from a finished result would make the live timeline impossible.
  const trace = new StepTraceModel();
  assert.equal(trace.status, "running");
  assert.equal(trace.settled, false);
  trace.noteAccepted(accepted());
  assert.equal(trace.status, "running");
  assert.equal(trace.accepted?.definitionNote, "the DEPLOYED definition ran");
});

test("StepTraceModel -- a run that STARTED AND FAILED keeps its trace", () => {
  // The distinction the engine draws and the UI must preserve: this is not a
  // refusal, and the steps it managed are the whole point of looking.
  const trace = new StepTraceModel();
  trace.noteAccepted(accepted());
  trace.noteStep(step({ sequence: 0, stepId: "load", status: "success" }));
  trace.noteStep(step({ sequence: 1, stepId: "write", status: "failed", error: "boom" }));
  trace.noteComplete(complete({ status: "failed", error: "step write failed" }));
  assert.equal(trace.status, "failed");
  assert.equal(trace.settled, true);
  assert.equal(trace.refusal, undefined);
  assert.equal(trace.steps.length, 2);
  assert.deepEqual(trace.counts, { success: 1, failed: 1, skipped: 0, other: 0 });
});

test("StepTraceModel -- a REFUSAL is its own status, with no timeline", () => {
  const trace = new StepTraceModel();
  trace.noteRefusal({
    code: 7,
    codeName: "PERMISSION_DENIED",
    message: "requires cluster owner or admin",
    runId: "run-1",
  });
  assert.equal(trace.status, "refused");
  assert.equal(trace.settled, true);
  assert.equal(trace.steps.length, 0);
  assert.equal(trace.runId, "run-1");
});

test("StepTraceModel -- a refusal after the accepted frame keeps the accepted frame", () => {
  // The engine sends `accepted` and can then refuse on the @filter, and the
  // accepted frame is what names the node that refused.
  const trace = new StepTraceModel();
  trace.noteAccepted(accepted({ requestedOnNodeId: "bff-1" }));
  trace.noteRefusal({ code: 9, codeName: "FAILED_PRECONDITION", message: "@filter", runId: "" });
  assert.equal(trace.accepted?.requestedOnNodeId, "bff-1");
});

test("StepTraceModel -- a refusal carrying its own accepted frame supplies one", () => {
  const trace = new StepTraceModel();
  trace.noteRefusal({
    code: 14,
    codeName: "UNAVAILABLE",
    message: "no cognition node",
    runId: "",
    accepted: accepted({ targetNodeType: "cognition" }),
  });
  assert.equal(trace.accepted?.targetNodeType, "cognition");
});

test("StepTraceModel -- a transport failure is neither a refusal nor a failed run", () => {
  const trace = new StepTraceModel();
  trace.noteError("Not connected to staging.");
  assert.equal(trace.status, "error");
  assert.equal(trace.settled, true);
});

test("StepTraceModel -- an unknown terminal status is not guessed at", () => {
  // A status this build does not know belongs to a newer engine. Inventing a
  // mapping would report an outcome nobody asserted.
  const trace = new StepTraceModel();
  trace.noteComplete(complete({ status: "quiesced" }));
  assert.equal(trace.status, "running");
});

// -----------------------------------------------------------------------------
// Presentation helpers
// -----------------------------------------------------------------------------

test("formatDuration -- sub-second durations keep their milliseconds", () => {
  // Most automation steps are sub-second, and rendering all of them as "0s"
  // would make the column useless.
  assert.equal(formatDuration(0), "0ms");
  assert.equal(formatDuration(7), "7ms");
  assert.equal(formatDuration(999), "999ms");
  assert.equal(formatDuration(1500), "1.50s");
  assert.equal(formatDuration(65000), "65.0s");
  assert.equal(formatDuration(-1), "-");
  assert.equal(formatDuration(Number.NaN), "-");
});

test("describeRefusal -- PERMISSION_DENIED names the role it requires", () => {
  // The spec's error-handling table: an insufficient role names the role,
  // never a silent no-op.
  const text = describeRefusal({
    code: 7,
    codeName: "PERMISSION_DENIED",
    message: "operator run requires owner or admin",
    runId: "",
  });
  assert.match(text, /CLUSTER OWNER/);
  assert.match(text, /ADMIN/);
  // The engine's own sentence is appended rather than replaced -- it carries
  // the specifics.
  assert.match(text, /operator run requires owner or admin/);
});

test("describeRefusal -- UNAVAILABLE points at the mesh, which is what it means", () => {
  const text = describeRefusal({ code: 14, codeName: "UNAVAILABLE", message: "", runId: "" });
  assert.match(text, /node type is not running/);
  assert.match(text, /forwarded across the mesh/);
});

test("describeRefusal -- FAILED_PRECONDITION explains @disabled and the @filter miss", () => {
  const text = describeRefusal({ code: 9, codeName: "FAILED_PRECONDITION", message: "", runId: "" });
  assert.match(text, /@disabled/);
  assert.match(text, /@filter/);
});

test("describeRefusal -- an unrecognised code still says the run was refused", () => {
  const text = describeRefusal({ code: 13, codeName: "INTERNAL", message: "kaboom", runId: "" });
  assert.match(text, /refused/);
  assert.match(text, /INTERNAL/);
  assert.match(text, /kaboom/);
});
