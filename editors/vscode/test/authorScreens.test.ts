// The authoring screens as pure renderers: what each state says, once, and
// which acts it offers.
//
// The gallery shows what these look like; these cases pin what they SAY and
// DO -- that a refusal never claims the automation ran, that a failed step's
// error is said once, that a disconnected read offers the act that fixes it,
// that loading is never a sentence, and that every bar holds at most one
// button.

import test from "node:test";
import assert from "node:assert/strict";

import type { AutomationRunStep } from "@znasllc-io/memql-sdk-core/automation";

import { automationFormPlan } from "../src/state/automationForm.js";
import { buildFields } from "../src/state/argForm.js";
import type { TraceStatus } from "../src/state/stepTrace.js";
import {
  automationFormParts,
  traceMeta,
  traceParts,
  triggerMeta,
  type AutomationFormInput,
  type TraceView,
} from "../src/webview/automationScreens.js";
import { conceptPageParts, type ConceptPageInput } from "../src/webview/conceptScreens.js";
import { resultParts, runFormParts, withSwitchDefaults } from "../src/webview/runScreens.js";
import type { RegionParts } from "../src/webview/ui/liveView.js";

function all(parts: RegionParts): string {
  return parts.head + parts.body + parts.actions;
}

/** Visible words: tags and screen-reader-only text removed, entities decoded. */
function words(html: string): string {
  return html
    .replace(/<span class="mq-sr">[^<]*<\/span>/g, "")
    .replace(/<[^>]+>/g, " ")
    .replace(/&#39;/g, "'")
    .replace(/&quot;/g, '"')
    .replace(/&amp;/g, "&");
}

/** A trace's page without its raw-frames disclosure, which carries every word verbatim. */
function withoutJson(html: string): string {
  return html.split('data-disclosure="trace-json"')[0] ?? html;
}

function buttons(html: string): number {
  return (html.match(/class="mq-btn"/g) ?? []).length;
}

// ---------------------------------------------------------------------------
// The trace
// ---------------------------------------------------------------------------

const ACCEPTED = {
  automation: "autoJoinSI",
  ranDeployedDefinition: true,
  definitionNote: "The deployed version ran.",
  triggerKind: "event",
  triggerTopic: "graph.node.created.v1:cognition:participant",
  requestedOnNodeId: "bff-0",
  requestedOnNodeType: "bff",
  targetNodeType: "",
};

function trace(status: TraceStatus, steps: AutomationRunStep[], over: Partial<TraceView> = {}): TraceView {
  return {
    accepted: ACCEPTED,
    complete:
      status === "running" || status === "refused" || status === "error"
        ? undefined
        : { status, durationMs: 1240, stepCount: steps.length, error: "boom", executedOnNodeId: "cog-0", executedOnNodeType: "cognition" },
    refusal: undefined,
    error: "",
    runId: "run_1",
    steps,
    status,
    settled: status !== "running",
    ...over,
  };
}

const step = (sequence: number, status: string, error = ""): AutomationRunStep => ({
  sequence,
  stepId: `step${sequence}`,
  status,
  durationMs: 10,
  error,
});

test("a refused run never says the deployed version ran, and names the fix before the engine's words", () => {
  const html = withoutJson(all(
    traceParts({
      name: "autoJoinSI",
      trace: trace("refused", [], {
        refusal: { code: 7, codeName: "PERMISSION_DENIED", message: "operator run requires owner or admin", runId: "run_1" },
      }),
      detailsOpen: true,
      jsonOpen: false,
    }),
  ));
  assert.doesNotMatch(words(html), /deployed version ran/);
  assert.match(words(html), /Didn't start/);
  assert.match(words(html), /Only a cluster owner or an admin can run automations\./);
  assert.match(words(html), /operator run requires owner or admin/);
  // The code is a detail, not the headline.
  assert.match(html, /Refusal code<\/dt><dd class="mq-mono">PERMISSION_DENIED/);
  assert.doesNotMatch(words(html), /REFUSED/);
});

test("a running trace does not speak in the past tense, and waits in the shape of a step", () => {
  const html = all(traceParts({ name: "autoJoinSI", trace: trace("running", [step(1, "success")]), detailsOpen: false, jsonOpen: false }));
  assert.doesNotMatch(words(html), /ran\./);
  assert.match(html, /mq-skeleton/);
  assert.equal(traceMeta(trace("running", [step(1, "success")])), "Running · 1 step");
  // The raw frames are offered only once there is a whole run to show.
  assert.doesNotMatch(html, /Show JSON/);
});

test("a failed step's error is said once, under the step", () => {
  const html = withoutJson(all(
    traceParts({
      name: "autoJoinSI",
      trace: trace("failed", [step(1, "success"), step(2, "failed", "agent Faye is not in the space")]),
      detailsOpen: false,
      jsonOpen: false,
    }),
  ));
  assert.match(words(html), /Failed at step 2\./);
  assert.equal(words(html).split("agent Faye is not in the space").length - 1, 1, "the error is said twice");
  assert.equal(traceMeta(trace("failed", [step(1, "success")])), "Failed · 1 step · 1.24s");
  // Statuses are words, not the wire's uppercase.
  assert.match(words(html), /Succeeded/);
  assert.doesNotMatch(words(html), /SUCCESS|FAILED/);
});

test("the trigger reads as words, not its annotation", () => {
  assert.equal(triggerMeta({ event: "node.created", concept: "v1:cognition:participant" }), "On node.created · participant");
  assert.equal(triggerMeta({ schedule: "0 */10 * * * *" }), "On schedule · 0 */10 * * * *");
  assert.equal(triggerMeta(undefined), "Run by hand");
});

// ---------------------------------------------------------------------------
// The automation form
// ---------------------------------------------------------------------------

function form(over: Partial<AutomationFormInput> = {}): string {
  const trigger = { event: "node.created", concept: "v1:cognition:participant" };
  return all(
    automationFormParts({
      name: "autoJoinSI",
      trigger,
      plan: automationFormPlan("autoJoinSI", trigger),
      picker: { open: true, settled: true, loading: false, rows: [], concept: { id: "c", entity: "participant" }, more: false, error: "" },
      payloadText: "",
      payloadError: "",
      targetNodeType: "",
      includeStepOutput: false,
      optionsOpen: false,
      busy: false,
      ...over,
    }),
  );
}

test("the form says the deployed version runs, in the bar, before the click -- and one button", () => {
  const html = form();
  assert.match(words(html), /Runs the deployed version/);
  assert.doesNotMatch(words(html), /DEPLOYED|session-defined|bus subscription/);
  assert.equal(buttons(html), 1);
  // While running there is nothing to press.
  assert.doesNotMatch(form({ busy: true }), /data-act="run"/);
});

test("the picker loads as a skeleton, fails with Try again, and says an empty concept is empty", () => {
  const picker = { open: true, settled: false, loading: true, rows: [], concept: { id: "c", entity: "participant" }, more: false, error: "" };
  assert.match(form({ picker }), /mq-skeleton/);
  assert.doesNotMatch(words(form({ picker })), /Loading rows/);
  assert.match(words(form({ picker: { ...picker, settled: true, loading: false, error: "stream closed" } })), /Couldn't load rows\./);
  assert.match(words(form()), /No participant rows yet/);
});

test("a scheduled automation has no payload box and says what it fires with", () => {
  const trigger = { schedule: "0 */10 * * * *" };
  const html = all(
    automationFormParts({
      name: "reap",
      trigger,
      plan: automationFormPlan("reap", trigger),
      picker: { open: false, settled: false, loading: false, rows: [], concept: { id: "", entity: "" }, more: false, error: "" },
      payloadText: "",
      payloadError: "",
      targetNodeType: "",
      includeStepOutput: true,
      optionsOpen: true,
      busy: false,
    }),
  );
  assert.doesNotMatch(html, /data-field="payload"/);
  assert.doesNotMatch(html, /Pick a row/);
  assert.match(words(html), /Runs now with an empty event\./);
  assert.match(html, /role="switch"[^>]*checked/);
});

// ---------------------------------------------------------------------------
// The run form and the result
// ---------------------------------------------------------------------------

test("the run form has one button, and Save as asks for the name when pressed", () => {
  const parts = runFormParts({
    target: { kind: "query", name: "q" },
    fields: withSwitchDefaults(buildFields([{ name: "a", type: "string", required: true }])),
    errors: {},
    orphans: [],
    busy: false,
  });
  assert.equal(buttons(parts.actions), 1);
  assert.match(parts.actions, /data-act="saveAs"/);
  // No standing name field for the saved run.
  assert.doesNotMatch(parts.body, /Name this run configuration/);
});

test("an optional boolean is a select with an empty choice; a required one is a switch that starts off", () => {
  const fields = withSwitchDefaults(
    buildFields([
      { name: "maybe", type: "boolean", required: false },
      { name: "must", type: "boolean", required: true },
    ]),
  );
  assert.equal(fields[1]?.text, "false");
  const body = runFormParts({ target: { kind: "query", name: "q" }, fields, errors: {}, orphans: [], busy: false }).body;
  assert.match(body, /<select class="mq-input" id="arg-maybe" data-field="maybe"/);
  assert.match(body, /role="switch"[^>]*data-field="must"/);
});

const TARGET = { uri: "file:///q.memql", kind: "query" as const, name: "q", args: [] };

test("a run refused for want of a connection offers the fix, and a failed one its error ID", () => {
  const disconnected = all(
    resultParts({
      state: "settled",
      concepts: new Map(),
      jsonOpen: false,
      outcome: { status: "error", target: TARGET, phase: "preflight", message: "Not connected to local.", errorId: "" },
    }),
  );
  assert.match(disconnected, /data-act="selectCluster"/);
  const failed = all(
    resultParts({
      state: "settled",
      concepts: new Map(),
      jsonOpen: false,
      outcome: { status: "error", target: TARGET, phase: "invoke", message: "boom ERR-1a2b3c", errorId: "ERR-1a2b3c" },
    }),
  );
  assert.match(failed, /data-act="copyErrorId"/);
  // The id rides inside the message it was lifted from: said once, not twice.
  assert.equal(words(failed).split("ERR-1a2b3c").length - 1, 1, "the error ID is on the page twice");
  assert.doesNotMatch(words(failed), /ERROR \(invoke\)/, "the internal phase is on the page");
});

test("a result says what ran in its meta line, and a tool says its edits don't apply", () => {
  const ok = resultParts({
    state: "settled",
    concepts: new Map(),
    jsonOpen: false,
    outcome: { status: "ok", target: TARGET, rows: [{ id: "a" }], raw: [], ranDeployedDefinition: true, injected: false },
  });
  assert.match(ok.head, /1 row · Deployed version/);
  // No rows: the body says so, and the meta does not say "0 rows" over it.
  const none = resultParts({
    state: "settled",
    concepts: new Map(),
    jsonOpen: false,
    outcome: { status: "ok", target: TARGET, rows: [], raw: [], ranDeployedDefinition: true, injected: false },
  });
  assert.doesNotMatch(none.head, /0 rows/);
  assert.match(words(none.body), /No rows\./);
  const tool = resultParts({
    state: "settled",
    concepts: new Map(),
    jsonOpen: false,
    outcome: {
      status: "ok",
      target: { ...TARGET, kind: "tool" },
      rows: [],
      raw: [],
      toolContent: [],
      ranDeployedDefinition: true,
      injected: false,
    },
  });
  assert.match(tool.head, /Deployed tool · edits here don&#39;t apply/);
});

// ---------------------------------------------------------------------------
// The concept page
// ---------------------------------------------------------------------------

function concept(over: Partial<ConceptPageInput>): string {
  return all(
    conceptPageParts({
      concept: { id: "v1:cognition:space", entity: "space" },
      connection: "connected",
      rows: [],
      settled: true,
      loading: false,
      listError: "",
      more: false,
      detail: { state: "none" },
      liveOff: "",
      ...over,
    }),
  );
}

test("each missing session is one sentence and its act -- not three banners", () => {
  const cases: [ConceptPageInput["connection"], string, string][] = [
    ["none", "Not connected.", "selectCluster"],
    ["signIn", "Sign in to see these rows.", "signIn"],
    ["unreachable", "The cluster isn't answering.", "reconnect"],
    ["notConfigured", "This cluster has no address.", "editCluster"],
  ];
  for (const [connection, line, act] of cases) {
    const html = concept({ connection });
    assert.match(words(html), new RegExp(line.replace(/[.?]/g, "\\$&")), connection);
    assert.match(html, new RegExp(`data-act="${act}"`), connection);
    assert.doesNotMatch(words(html), /ERROR|WARNING|live updates/, connection);
  }
  // Dialing is the shape of the list, with no sentence.
  assert.match(concept({ connection: "connecting" }), /mq-skeleton/);
});

test("live updates are said only when they are off, with Reload beside it", () => {
  const live = concept({ rows: [{ id: "a" }] });
  assert.doesNotMatch(live, /Not live|data-act="reload"/);
  const off = concept({ rows: [{ id: "a" }], liveOff: "subscriptions are off" });
  assert.match(words(off), /Not live/);
  assert.match(off, /data-act="reload"[^>]*title="Live updates are off: subscriptions are off"/);
});
