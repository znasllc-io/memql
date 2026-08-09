// The automation run loop (memql#3310).
//
// Four behaviours are worth exercising here rather than trusting to review:
//
//  1. THE NON-LOCAL WRITE CONFIRMATION COVERS AUTOMATION RUNS. It is an
//     acceptance criterion, and it is satisfied by going through B2's gate
//     rather than by a second confirmation path -- so the test hands in a real
//     WriteConfirmationGate and asserts both the prompt and the "prompts once"
//     property that gate owns.
//  2. A REFUSAL RESOLVES DIFFERENTLY FROM A FAILURE. The SDK throws for one
//     and resolves for the other; the outcome union has to keep them apart or
//     the panel cannot.
//  3. THE TRACE FILLS LIVE. `onAccepted` fires before any step, so the header
//     is renderable while the run is still going.
//  4. A SUPERSEDED RUN PAINTS NOTHING. A second Run click makes the first
//     stale, including its still-arriving step frames.

import test from "node:test";
import assert from "node:assert/strict";

import {
  AutomationRunError,
  type AutomationRunResult,
} from "@znasllc-io/memql-sdk-core/automation";

import type { AutomationTarget } from "../src/constructs/runnable.js";
import {
  AutomationRunner,
  type AutomationRunDeps,
  type AutomationRunEngine,
} from "../src/run/automationRun.js";
import { WriteConfirmationGate } from "../src/run/preflight.js";
import type { RunCluster } from "../src/run/orchestrator.js";
import { StepTraceModel } from "../src/state/stepTrace.js";

const TARGET: AutomationTarget = {
  uri: "file:///dsl/cognition/automations.memql",
  name: "autoJoinSI",
  trigger: { event: "node.created", concept: "v1:cognition:participant" },
};

const LOCAL: RunCluster = { name: "local", label: "local", local: true };
const STAGING: RunCluster = { name: "staging", label: "memQL staging", local: false };

function okResult(overrides: Partial<AutomationRunResult> = {}): AutomationRunResult {
  return {
    runId: "run-1",
    accepted: {
      automation: "autoJoinSI",
      ranDeployedDefinition: true,
      definitionNote: "the DEPLOYED definition ran",
      triggerKind: "event",
      triggerTopic: "node.created.v1:cognition:participant",
      requestedOnNodeId: "bff-0",
      requestedOnNodeType: "bff",
      targetNodeType: "",
    },
    steps: [
      { sequence: 0, stepId: "join", status: "success", durationMs: 3, error: "" },
    ],
    complete: {
      status: "completed",
      durationMs: 11,
      stepCount: 1,
      error: "",
      executedOnNodeId: "bff-0",
      executedOnNodeType: "bff",
    },
    ...overrides,
  };
}

interface Harness {
  runner: AutomationRunner;
  gate: WriteConfirmationGate;
  prompts: string[];
  requests: unknown[];
}

function harness(
  opts: {
    cluster?: RunCluster;
    engine?: AutomationRunEngine | undefined;
    confirm?: boolean;
  } = {},
): Harness {
  const gate = new WriteConfirmationGate();
  const prompts: string[] = [];
  const requests: unknown[] = [];
  const engine: AutomationRunEngine | undefined =
    opts.engine === undefined && !("engine" in opts)
      ? {
          runAutomation: async (_automation, request) => {
            requests.push(request);
            return okResult();
          },
        }
      : opts.engine;
  const deps: AutomationRunDeps = {
    cluster: () => opts.cluster ?? LOCAL,
    engine: () => engine,
    confirmWrite: async (message) => {
      prompts.push(message);
      return opts.confirm ?? true;
    },
    writeGate: gate,
  };
  return { runner: new AutomationRunner(deps), gate, prompts, requests };
}

// -----------------------------------------------------------------------------
// Preflight
// -----------------------------------------------------------------------------

test("run -- no cluster selected is a preflight error, not an attempted run", () => {
  const h = harness({ cluster: undefined as unknown as RunCluster });
  const deps: AutomationRunDeps = {
    cluster: () => undefined,
    engine: () => undefined,
    confirmWrite: async () => true,
    writeGate: h.gate,
  };
  const runner = new AutomationRunner(deps);
  return runner.run(TARGET, {}, new StepTraceModel()).then((outcome) => {
    assert.equal(outcome.status, "error");
    assert.equal(outcome.status === "error" && outcome.phase, "preflight");
    assert.match(outcome.status === "error" ? outcome.message : "", /No cluster selected/);
    // The trace carries the failure too, so the panel always has one thing to
    // render rather than two sources of truth.
    assert.equal(outcome.status === "error" && outcome.trace.status, "error");
  });
});

test("run -- not connected names the cluster it is not connected to", async () => {
  const h = harness({ cluster: STAGING, engine: undefined });
  const outcome = await h.runner.run(TARGET, {}, new StepTraceModel());
  assert.equal(outcome.status, "error");
  assert.match(outcome.status === "error" ? outcome.message : "", /memQL staging/);
});

// -----------------------------------------------------------------------------
// The non-local write confirmation
// -----------------------------------------------------------------------------

test("run -- a non-local cluster prompts before an automation run", async () => {
  // The acceptance criterion. Same gate B2's mutations use -- automations are
  // a write kind (constructs/runnable.ts), so this is the ordinary path.
  const h = harness({ cluster: STAGING });
  const outcome = await h.runner.run(TARGET, {}, new StepTraceModel());
  assert.equal(outcome.status, "ok");
  assert.equal(h.prompts.length, 1);
  assert.match(h.prompts[0] ?? "", /autoJoinSI/);
  assert.match(h.prompts[0] ?? "", /memQL staging/);
});

test("run -- a local cluster does not prompt", async () => {
  const h = harness({ cluster: LOCAL });
  await h.runner.run(TARGET, {}, new StepTraceModel());
  assert.equal(h.prompts.length, 0);
});

test("run -- the prompt fires once per (cluster, construct), through the SHARED gate", async () => {
  // The gate's property, not this runner's. Asserting it here is what proves
  // the automation surface goes THROUGH the gate rather than reimplementing
  // the decision.
  const h = harness({ cluster: STAGING });
  await h.runner.run(TARGET, {}, new StepTraceModel());
  await h.runner.run(TARGET, {}, new StepTraceModel());
  assert.equal(h.prompts.length, 1);
  // And an acknowledgement granted through the automation surface is visible
  // to the gate B2 shares.
  assert.equal(h.gate.required("mutate", false, "staging", "autoJoinSI"), false);
});

test("run -- declining the prompt runs nothing", async () => {
  const h = harness({ cluster: STAGING, confirm: false });
  const outcome = await h.runner.run(TARGET, {}, new StepTraceModel());
  assert.equal(outcome.status, "declined");
  assert.equal(h.requests.length, 0);
});

// -----------------------------------------------------------------------------
// The request
// -----------------------------------------------------------------------------

test("run -- the request is forwarded verbatim", async () => {
  const h = harness();
  await h.runner.run(
    TARGET,
    {
      payload: { id: "v1:cognition:participant:abc" },
      concept: "v1:cognition:participant",
      targetNodeType: "cognition",
      includeStepOutput: true,
    },
    new StepTraceModel(),
  );
  assert.deepEqual(h.requests[0], {
    payload: { id: "v1:cognition:participant:abc" },
    concept: "v1:cognition:participant",
    targetNodeType: "cognition",
    includeStepOutput: true,
  });
});

// -----------------------------------------------------------------------------
// The trace
// -----------------------------------------------------------------------------

test("run -- the trace fills LIVE, accepted before any step", async () => {
  const seen: string[] = [];
  const engine: AutomationRunEngine = {
    runAutomation: async (_automation, _request, hooks) => {
      hooks.onAccepted(okResult().accepted);
      seen.push("accepted");
      hooks.onStep({ sequence: 0, stepId: "join", status: "success", durationMs: 3, error: "" });
      seen.push("step");
      return okResult();
    },
  };
  const h = harness({ engine });
  const trace = new StepTraceModel();
  const progress: string[] = [];
  const outcome = await h.runner.run(TARGET, {}, trace, () => {
    // Snapshot what is renderable at each callback. The first one must already
    // carry the banner, with no steps yet -- that is what "renders before the
    // trace has anything in it" means concretely.
    progress.push(`${trace.accepted === undefined ? "-" : "accepted"}:${trace.steps.length}`);
  });
  assert.equal(outcome.status, "ok");
  assert.deepEqual(seen, ["accepted", "step"]);
  assert.equal(progress[0], "accepted:0");
  assert.equal(progress[1], "accepted:1");
  assert.equal(trace.complete?.status, "completed");
});

test("run -- a run that started and FAILED resolves ok, with its trace intact", async () => {
  // Not an error outcome. The engine ran the automation; the timeline is the
  // answer to the developer's question.
  const engine: AutomationRunEngine = {
    runAutomation: async () =>
      okResult({
        steps: [
          { sequence: 0, stepId: "load", status: "success", durationMs: 2, error: "" },
          { sequence: 1, stepId: "write", status: "failed", durationMs: 9, error: "boom" },
        ],
        complete: {
          status: "failed",
          durationMs: 12,
          stepCount: 2,
          error: "step write failed",
          executedOnNodeId: "bff-0",
          executedOnNodeType: "bff",
        },
      }),
  };
  const h = harness({ engine });
  const trace = new StepTraceModel();
  const outcome = await h.runner.run(TARGET, {}, trace);
  assert.equal(outcome.status, "ok");
  assert.equal(trace.status, "failed");
  assert.equal(trace.steps.length, 2);
});

test("run -- a REFUSAL is its own outcome, carrying the code", async () => {
  const engine: AutomationRunEngine = {
    runAutomation: async () => {
      throw new AutomationRunError("autoJoinSI", 7, "requires owner or admin", "run-9");
    },
  };
  const h = harness({ engine });
  const trace = new StepTraceModel();
  const outcome = await h.runner.run(TARGET, {}, trace);
  assert.equal(outcome.status, "refused");
  assert.equal(trace.status, "refused");
  assert.equal(trace.refusal?.code, 7);
  assert.equal(trace.refusal?.codeName, "PERMISSION_DENIED");
  // The SDK's `run automation X: CODE: ` prefix is stripped: the panel already
  // renders the name and the code in their own fields.
  assert.equal(trace.refusal?.message, "requires owner or admin");
  assert.equal(trace.runId, "run-9");
});

test("run -- a refusal with no message leaves the message empty rather than literal", async () => {
  // AutomationRunError formats an absent message as "(no message)", which is
  // right for a thrown Error's text and wrong to paste into a UI sentence.
  const engine: AutomationRunEngine = {
    runAutomation: async () => {
      throw new AutomationRunError("autoJoinSI", 5, "", "");
    },
  };
  const h = harness({ engine });
  const trace = new StepTraceModel();
  await h.runner.run(TARGET, {}, trace);
  assert.equal(trace.refusal?.message, "");
});

test("run -- a transport failure is an error outcome, not a refusal", async () => {
  // "The automation could not be attempted for a reason the engine chose" and
  // "the request never got an answer" are different, and only the first has a
  // code worth explaining.
  const engine: AutomationRunEngine = {
    runAutomation: async () => {
      throw new Error("socket closed");
    },
  };
  const h = harness({ engine });
  const trace = new StepTraceModel();
  const outcome = await h.runner.run(TARGET, {}, trace);
  assert.equal(outcome.status, "error");
  assert.equal(outcome.status === "error" && outcome.phase, "invoke");
  assert.equal(trace.refusal, undefined);
  assert.equal(trace.status, "error");
});

test("run -- an ERR- id is lifted out of the message for separate rendering", async () => {
  const engine: AutomationRunEngine = {
    runAutomation: async () => {
      throw new Error("the run failed (ERR-a1b2c3)");
    },
  };
  const h = harness({ engine });
  const outcome = await h.runner.run(TARGET, {}, new StepTraceModel());
  assert.equal(outcome.status === "error" && outcome.errorId, "ERR-a1b2c3");
});

// -----------------------------------------------------------------------------
// Supersession
// -----------------------------------------------------------------------------

test("run -- a second run supersedes the first, including its in-flight step frames", async () => {
  // The first run's frames are still arriving when the second starts. Letting
  // them write would paint an older run's timeline over a newer one's.
  let release: (() => void) | undefined;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  let firstHooks: Parameters<AutomationRunEngine["runAutomation"]>[2] | undefined;
  let call = 0;
  const engine: AutomationRunEngine = {
    runAutomation: async (_automation, _request, hooks) => {
      call++;
      if (call === 1) {
        firstHooks = hooks;
        await gate;
        return okResult();
      }
      return okResult({ runId: "run-2" });
    },
  };
  const h = harness({ engine });
  const firstTrace = new StepTraceModel();
  const first = h.runner.run(TARGET, {}, firstTrace);
  const second = await h.runner.run(TARGET, {}, new StepTraceModel());
  assert.equal(second.status, "ok");

  // Now let the FIRST run deliver a frame and finish. Neither may land.
  firstHooks?.onStep({ sequence: 0, stepId: "late", status: "success", durationMs: 1, error: "" });
  release?.();
  const firstOutcome = await first;
  assert.equal(firstOutcome.status, "superseded");
  assert.equal(firstTrace.steps.length, 0);
  assert.equal(firstTrace.complete, undefined);
});

// The @disabled flag reaches the refusal from the LENS, not from the reply.
// The engine answers @disabled and a @filter miss with the same
// FAILED_PRECONDITION and nothing in the reply separates them, so what the
// language server said about the construct is the only thing that can
// (memql#3333).
test("run -- a @disabled target marks its refusal so the cause can be named", async () => {
  const engine: AutomationRunEngine = {
    runAutomation: async () => {
      throw new AutomationRunError("autoJoinSI", 9, "automation is disabled", "run-11");
    },
  };
  const h = harness({ engine });
  const trace = new StepTraceModel();
  const outcome = await h.runner.run({ ...TARGET, disabled: true }, {}, trace);
  assert.equal(outcome.status, "refused");
  assert.equal(trace.refusal?.disabled, true);
});

// An enabled target must NOT claim the flag: asserting "@disabled" over what
// was really a filter miss sends the developer to re-enable a construct that
// was never disabled.
test("run -- an enabled target leaves the disabled flag off its refusal", async () => {
  const engine: AutomationRunEngine = {
    runAutomation: async () => {
      throw new AutomationRunError("autoJoinSI", 9, "filter rejected the event", "run-12");
    },
  };
  const h = harness({ engine });
  const trace = new StepTraceModel();
  await h.runner.run(TARGET, {}, trace);
  assert.equal(trace.refusal?.disabled, undefined);
});
