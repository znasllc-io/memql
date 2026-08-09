// The run loop end to end, against a stub engine.
//
// This file carries the acceptance criteria that are behaviour rather than
// shape:
//
//   - a buffer edit is reflected in the run without saving or redeploying;
//   - a RECONNECT re-injects before honouring a re-run;
//   - a mutation against a non-local cluster prompts ONCE;
//   - validation diagnostics reach the Problems panel at buffer coordinates;
//   - a tool run is honestly labelled as the deployed definition.
//
// The reconnect case is the one worth reading twice. Its failure mode is a
// GREEN RESULT: after the stream drops, an un-re-injected re-run resolves
// against the deployed construct and returns rows. Nothing errors. So the
// assertion is not "the run failed" -- it is "sessionDefineBundle was called
// again, before the invoke", which is the only observable difference between
// running your buffer and running something else that happens to have the
// same name.

import test from "node:test";
import assert from "node:assert/strict";

import type { Row } from "@znasllc-io/memql-sdk-core/client";
import type {
  SessionDefineBundleResult,
  ValidateBundleResult,
} from "@znasllc-io/memql-sdk-core/authoring";

import type { RunTarget } from "../src/constructs/runnable.js";
import { assembleBundle, type WorkspaceSources } from "../src/run/bundle.js";
import type { MappedDiagnostic } from "../src/run/diagnostics.js";
import { RunOrchestrator, type RunCluster, type RunDeps, type RunEngine, type ToolContent } from "../src/run/orchestrator.js";

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

// The call log is the point of the whole fixture: every assertion about the
// run loop is an assertion about which engine calls happened, in what order.
type Call =
  | { op: "validate"; sources: string }
  | { op: "define"; sources: string }
  | { op: "executeNamed"; name: string; call: string }
  | { op: "callTool"; name: string; args: Record<string, unknown> };

class StubEngine implements RunEngine {
  readonly calls: Call[] = [];
  validateResult: ValidateBundleResult = { ok: true, diagnostics: [] };
  defineResult: SessionDefineBundleResult = { ok: true, defined: [], diagnostics: [], error: "" };
  rows: Row[] = [{ id: "r1", concept: "v1:cognition:space" }];
  executeError: Error | undefined;
  toolResult: { content: ToolContent[]; isError: boolean } = { content: [], isError: false };

  async validateBundle(sources: string): Promise<ValidateBundleResult> {
    this.calls.push({ op: "validate", sources });
    return this.validateResult;
  }

  async sessionDefineBundle(sources: string): Promise<SessionDefineBundleResult> {
    this.calls.push({ op: "define", sources });
    return this.defineResult;
  }

  async executeNamed(name: string, call: string): Promise<{ rows: Row[]; raw: unknown }> {
    this.calls.push({ op: "executeNamed", name, call });
    if (this.executeError !== undefined) throw this.executeError;
    return { rows: this.rows, raw: { data: this.rows } };
  }

  async callTool(
    name: string,
    args: Record<string, unknown>,
  ): Promise<{ content: ToolContent[]; isError: boolean }> {
    this.calls.push({ op: "callTool", name, args });
    return this.toolResult;
  }

  ops(): string[] {
    return this.calls.map((c) => c.op);
  }
}

interface Harness {
  engine: StubEngine;
  orchestrator: RunOrchestrator;
  published: MappedDiagnostic[][];
  confirmations: string[];
  /** The active buffer's text. Reassign it to simulate an unsaved edit. */
  setActiveText(text: string): void;
  setCluster(cluster: RunCluster | undefined): void;
  setConnected(connected: boolean): void;
  setConfirmAnswer(answer: boolean): void;
}

function emptyWorkspace(): WorkspaceSources {
  // `imports` is the language-server seam (memql#3335). An empty workspace
  // declares none, so the bundle is the active file alone.
  return { resolveImport: () => undefined, read: () => undefined, imports: () => [] };
}

function harness(): Harness {
  const engine = new StubEngine();
  const published: MappedDiagnostic[][] = [];
  const confirmations: string[] = [];
  let activeText = "query q { }\n";
  let cluster: RunCluster | undefined = { name: "local", label: "local", local: true };
  let connected = true;
  let confirmAnswer = true;

  const deps: RunDeps = {
    cluster: () => cluster,
    engine: () => (connected ? engine : undefined),
    assemble: (target) => assembleBundle(target.uri, activeText, emptyWorkspace()),
    confirmWrite: async (message) => {
      confirmations.push(message);
      return confirmAnswer;
    },
    publishDiagnostics: (d) => published.push(d),
  };

  return {
    engine,
    orchestrator: new RunOrchestrator(deps),
    published,
    confirmations,
    setActiveText: (text) => {
      activeText = text;
    },
    setCluster: (c) => {
      cluster = c;
    },
    setConnected: (c) => {
      connected = c;
    },
    setConfirmAnswer: (a) => {
      confirmAnswer = a;
    },
  };
}

function target(overrides: Partial<RunTarget> = {}): RunTarget {
  return {
    uri: "/ws/q.memql",
    kind: "query",
    name: "spaceParticipants",
    args: [{ name: "spaceId", type: "string", required: true }],
    ...overrides,
  };
}

// -----------------------------------------------------------------------------
// The happy path
// -----------------------------------------------------------------------------

test("run -- validate, then define, then invoke, in that order", () => {
  // Validate BEFORE define is not an optimisation (define validates too): it
  // is the only place a compile error can be shown without touching the
  // cluster at all, and it keeps "your buffer does not compile" distinct from
  // "the engine refused the bundle".
  const h = harness();
  return h.orchestrator.run(target(), { spaceId: "s1" }).then((outcome) => {
    assert.equal(outcome.status, "ok");
    assert.deepEqual(h.engine.ops(), ["validate", "define", "executeNamed"]);
  });
});

test("run -- the invoke carries the rendered named call", async () => {
  const h = harness();
  await h.orchestrator.run(target(), { spaceId: "s1" });
  const invoke = h.engine.calls.at(-1);
  assert.equal(invoke?.op, "executeNamed");
  assert.equal(invoke?.op === "executeNamed" ? invoke.call : "", 'query spaceParticipants(spaceId: "s1")');
});

test("run -- a mutation renders the `mutation` keyword", async () => {
  const h = harness();
  await h.orchestrator.run(target({ kind: "mutate", name: "createSpace", args: [] }), {});
  const invoke = h.engine.calls.at(-1);
  assert.equal(invoke?.op === "executeNamed" ? invoke.call : "", "mutation createSpace()");
});

test("run -- a successful run reports it ran the buffer, and clears diagnostics", async () => {
  const h = harness();
  const outcome = await h.orchestrator.run(target(), { spaceId: "s1" });
  assert.ok(outcome.status === "ok");
  assert.equal(outcome.ranDeployedDefinition, false);
  assert.equal(outcome.injected, true);
  // An empty publish is what removes a previous failure's entries from the
  // Problems panel; without it a fixed error lingers until the next failure.
  assert.deepEqual(h.published.at(-1), []);
});

// -----------------------------------------------------------------------------
// Buffer edits, without saving
// -----------------------------------------------------------------------------

test("run -- an UNSAVED buffer edit is re-injected on the next run", async () => {
  const h = harness();
  await h.orchestrator.run(target(), { spaceId: "s1" });
  h.setActiveText("query q { filter true }\n");
  await h.orchestrator.run(target(), { spaceId: "s1" });

  const defines = h.engine.calls.filter((c) => c.op === "define");
  assert.equal(defines.length, 2, "the changed buffer must be injected again");
  assert.equal(defines[1]?.op === "define" ? defines[1].sources : "", "query q { filter true }\n");
});

test("run -- an UNCHANGED buffer is not re-injected", async () => {
  // Not just an optimisation: it is what makes the reconnect test below
  // meaningful. If every run re-injected, "did it re-inject after a
  // reconnect" would be unobservable.
  const h = harness();
  await h.orchestrator.run(target(), { spaceId: "s1" });
  await h.orchestrator.run(target(), { spaceId: "s1" });
  assert.equal(h.engine.calls.filter((c) => c.op === "define").length, 1);
});

test("run -- a run reusing a live injection still reports it ran the buffer", async () => {
  const h = harness();
  await h.orchestrator.run(target(), { spaceId: "s1" });
  const outcome = await h.orchestrator.run(target(), { spaceId: "s1" });
  assert.ok(outcome.status === "ok");
  assert.equal(outcome.injected, false);
  assert.equal(outcome.ranDeployedDefinition, false);
});

// -----------------------------------------------------------------------------
// Reconnect re-injection
// -----------------------------------------------------------------------------

test("run -- a RECONNECT re-injects before honouring the re-run, even unchanged", async () => {
  // THE failure this whole mechanism exists for. Session-defined constructs
  // die with the stream, silently: the next call by that name resolves against
  // the DEPLOYED construct and returns a perfectly good result. So the
  // assertion is about the CALL SEQUENCE, not about a failure -- the run would
  // "work" either way.
  const h = harness();
  await h.orchestrator.run(target(), { spaceId: "s1" });
  assert.equal(h.engine.calls.filter((c) => c.op === "define").length, 1);

  // The adapter calls this on every ConnectionManager state change.
  h.orchestrator.noteStreamReset();

  await h.orchestrator.run(target(), { spaceId: "s1" });

  const ops = h.engine.ops();
  assert.equal(
    h.engine.calls.filter((c) => c.op === "define").length,
    2,
    "an unchanged buffer must still be re-injected after the stream that held it ended",
  );
  // And the re-injection must precede the invoke, not follow it.
  assert.deepEqual(ops, ["validate", "define", "executeNamed", "validate", "define", "executeNamed"]);
});

test("run -- a FAILED define does not leave a record that suppresses the next injection", async () => {
  // The same silent failure from the other direction: a stale record makes
  // the next run skip the inject and invoke against a registry that no longer
  // holds what the record claims.
  const h = harness();
  h.engine.defineResult = { ok: false, defined: [], diagnostics: [], error: "refused" };
  const first = await h.orchestrator.run(target(), { spaceId: "s1" });
  assert.equal(first.status, "error");

  h.engine.defineResult = { ok: true, defined: [], diagnostics: [], error: "" };
  await h.orchestrator.run(target(), { spaceId: "s1" });
  assert.equal(h.engine.calls.filter((c) => c.op === "define").length, 2);
});

// -----------------------------------------------------------------------------
// Preflight
// -----------------------------------------------------------------------------

test("run -- refuses with no cluster selected", async () => {
  const h = harness();
  h.setCluster(undefined);
  const outcome = await h.orchestrator.run(target(), { spaceId: "s1" });
  assert.ok(outcome.status === "error");
  assert.equal(outcome.phase, "preflight");
  assert.match(outcome.message, /No cluster selected/);
  assert.deepEqual(h.engine.ops(), []);
});

test("run -- refuses when disconnected, naming the cluster", async () => {
  const h = harness();
  h.setConnected(false);
  const outcome = await h.orchestrator.run(target(), { spaceId: "s1" });
  assert.ok(outcome.status === "error");
  assert.equal(outcome.phase, "preflight");
  assert.match(outcome.message, /Not connected to local/);
});

test("run -- a mutation against a NON-LOCAL cluster prompts once, naming both", async () => {
  const h = harness();
  h.setCluster({ name: "staging", label: "memQL Staging", local: false });
  const mutation = target({ kind: "mutate", name: "createSpace", args: [] });

  await h.orchestrator.run(mutation, {});
  assert.equal(h.confirmations.length, 1);
  assert.match(h.confirmations[0] ?? "", /createSpace/);
  assert.match(h.confirmations[0] ?? "", /memQL Staging/);

  await h.orchestrator.run(mutation, {});
  assert.equal(h.confirmations.length, 1, "the second run of the same mutation must not re-prompt");
});

test("run -- a mutation against a LOCAL cluster never prompts", async () => {
  const h = harness();
  await h.orchestrator.run(target({ kind: "mutate", name: "createSpace", args: [] }), {});
  assert.deepEqual(h.confirmations, []);
});

test("run -- a READ against a non-local cluster never prompts", async () => {
  const h = harness();
  h.setCluster({ name: "staging", label: "staging", local: false });
  await h.orchestrator.run(target(), { spaceId: "s1" });
  assert.deepEqual(h.confirmations, []);
});

test("run -- declining the confirmation stops before touching the engine", async () => {
  const h = harness();
  h.setCluster({ name: "staging", label: "staging", local: false });
  h.setConfirmAnswer(false);
  const outcome = await h.orchestrator.run(target({ kind: "mutate", name: "createSpace", args: [] }), {});
  assert.equal(outcome.status, "declined");
  assert.deepEqual(h.engine.ops(), []);
});

test("run -- a declined confirmation is NOT remembered as an acknowledgement", async () => {
  const h = harness();
  h.setCluster({ name: "staging", label: "staging", local: false });
  h.setConfirmAnswer(false);
  const mutation = target({ kind: "mutate", name: "createSpace", args: [] });
  await h.orchestrator.run(mutation, {});
  await h.orchestrator.run(mutation, {});
  assert.equal(h.confirmations.length, 2, "saying no must not pre-authorise the next attempt");
});

// -----------------------------------------------------------------------------
// Validation failures
// -----------------------------------------------------------------------------

test("run -- a failing validation publishes diagnostics and never defines or invokes", async () => {
  const h = harness();
  h.setActiveText("query q {\n  filter broken ==\n}\n");
  h.engine.validateResult = {
    ok: false,
    diagnostics: [
      { name: "q", kind: "query", ok: false, skipped: false, error: "unexpected token", line: 2, column: 3, endLine: 0, endColumn: 0 },
    ],
  };
  const outcome = await h.orchestrator.run(target(), { spaceId: "s1" });

  assert.ok(outcome.status === "invalid");
  assert.equal(outcome.phase, "validate");
  assert.equal(outcome.diagnostics.length, 1);
  // Mapped to BUFFER coordinates: bundle line 2 (1-based) in a single-file
  // bundle is buffer line 1.
  assert.equal(outcome.diagnostics[0]?.start.line, 1);
  assert.equal(outcome.diagnostics[0]?.path, "/ws/q.memql");
  assert.deepEqual(h.published.at(-1)?.length, 1);
  // Gate-1 held: nothing touched the cluster's registry, and nothing ran.
  assert.deepEqual(h.engine.ops(), ["validate"]);
});

test("run -- a SKIPPED construct does not fail the run", async () => {
  // A bundle routinely carries a shape or a concept, each reporting ok=false
  // with skipped=true. Reading `!ok` alone would fail every run of a file that
  // declares one.
  const h = harness();
  h.engine.validateResult = {
    ok: true,
    diagnostics: [
      { name: "s", kind: "shape", ok: false, skipped: true, error: "kind not compiled", line: 1, column: 1, endLine: 0, endColumn: 0 },
    ],
  };
  const outcome = await h.orchestrator.run(target(), { spaceId: "s1" });
  assert.equal(outcome.status, "ok");
});

test("run -- a bundle refused with no per-construct diagnostic becomes an ordinary error", async () => {
  // Reporting "invalid" with an empty diagnostic list would leave the
  // developer with a failed run and an empty Problems panel.
  const h = harness();
  h.engine.validateResult = { ok: false, diagnostics: [] };
  const outcome = await h.orchestrator.run(target(), { spaceId: "s1" });
  assert.ok(outcome.status === "error");
  assert.equal(outcome.phase, "validate");
  assert.match(outcome.message, /without a per-construct diagnostic/);
});

test("run -- a failing DEFINE surfaces its diagnostics too", async () => {
  const h = harness();
  h.setActiveText("query q {\n  filter x\n}\n");
  h.engine.defineResult = {
    ok: false,
    defined: [],
    diagnostics: [
      { name: "q", kind: "query", ok: false, skipped: false, error: "bind failed", line: 2, column: 3, endLine: 0, endColumn: 0 },
    ],
    error: "",
  };
  const outcome = await h.orchestrator.run(target(), { spaceId: "s1" });
  assert.ok(outcome.status === "invalid");
  assert.equal(outcome.phase, "define");
  assert.deepEqual(h.engine.ops(), ["validate", "define"]);
});

// -----------------------------------------------------------------------------
// Tools
// -----------------------------------------------------------------------------

test("run -- a tool goes through callTool and is NEVER session-defined", async () => {
  // A tool is bound to a Go-backed handler; there is nothing in the buffer to
  // inject, and pretending otherwise would make the result view claim the
  // buffer ran.
  const h = harness();
  const outcome = await h.orchestrator.run(
    target({ kind: "tool", name: "searchUsers", args: [{ name: "limit", type: "number", required: false }] }),
    { limit: 10 },
  );
  assert.ok(outcome.status === "ok");
  assert.equal(outcome.ranDeployedDefinition, true);
  assert.deepEqual(h.engine.ops(), ["validate", "callTool"]);
  const call = h.engine.calls.at(-1);
  assert.deepEqual(call?.op === "callTool" ? call.args : {}, { limit: 10 });
});

test("run -- a tool's buffer is still VALIDATED", async () => {
  // The tool itself cannot be injected, but a buffer whose other constructs do
  // not compile is something the developer wants to know before reading a
  // result.
  const h = harness();
  h.engine.validateResult = {
    ok: false,
    diagnostics: [
      { name: "other", kind: "query", ok: false, skipped: false, error: "boom", line: 1, column: 1, endLine: 0, endColumn: 0 },
    ],
  };
  const outcome = await h.orchestrator.run(target({ kind: "tool", name: "searchUsers", args: [] }), {});
  assert.equal(outcome.status, "invalid");
});

test("run -- a tool reporting isError in band becomes an error outcome", async () => {
  // A tool signals failure IN BAND rather than by rejecting, so without
  // lifting the text out the result view shows a successful-looking empty
  // panel for a failed call.
  const h = harness();
  h.engine.toolResult = {
    isError: true,
    content: [{ type: "text", text: "no such user", mimeType: "", data: "", uri: "" }],
  };
  const outcome = await h.orchestrator.run(target({ kind: "tool", name: "searchUsers", args: [] }), {});
  assert.ok(outcome.status === "error");
  assert.equal(outcome.phase, "invoke");
  assert.match(outcome.message, /no such user/);
});

// -----------------------------------------------------------------------------
// Runtime errors
// -----------------------------------------------------------------------------

test("run -- an engine error surfaces its ERR- id separately", async () => {
  // The id is the only handle a developer has on the server-side log entry
  // for their failure; burying it in prose is the difference between a
  // support thread that resolves and one that does not.
  const h = harness();
  h.engine.executeError = new Error("spaceParticipants: internal failure ERR-a1b2c3");
  const outcome = await h.orchestrator.run(target(), { spaceId: "s1" });
  assert.ok(outcome.status === "error");
  assert.equal(outcome.phase, "invoke");
  assert.equal(outcome.errorId, "ERR-a1b2c3");
});

test("run -- an insufficient-role refusal is surfaced verbatim, never swallowed", async () => {
  const h = harness();
  h.engine.executeError = new Error("permission denied: this action requires the owner role");
  const outcome = await h.orchestrator.run(target(), { spaceId: "s1" });
  assert.ok(outcome.status === "error");
  assert.match(outcome.message, /requires the owner role/);
});

// -----------------------------------------------------------------------------
// Supersession
// -----------------------------------------------------------------------------

test("run -- a second run supersedes the first, which reports rather than landing", async () => {
  // Validate, define and invoke are three awaits in sequence; a second Run
  // click during any of them makes the first run's remaining steps stale. One
  // Latest token, checked after every await, is what keeps a superseded run
  // from publishing diagnostics over a newer one's or landing an older result.
  let releaseFirst: (() => void) | undefined;
  const gate = new Promise<void>((resolve) => {
    releaseFirst = resolve;
  });
  // Signals that the FIRST run has actually reached validate and parked there.
  //
  // Needed since memql#3335 made bundle assembly awaitable: `run` now suspends
  // before validate, so simply calling it no longer guarantees it has got that
  // far. Without this the second run reaches validate first, consumes the gate,
  // and the two deadlock -- an artefact of the harness, not of supersession.
  let noteFirstParked: (() => void) | undefined;
  const firstParked = new Promise<void>((resolve) => {
    noteFirstParked = resolve;
  });

  const engine = new StubEngine();
  let firstValidate = true;
  const slowEngine: RunEngine = {
    ...engine,
    validateBundle: async (sources: string) => {
      if (firstValidate) {
        firstValidate = false;
        noteFirstParked?.();
        await gate;
      }
      return engine.validateBundle(sources);
    },
    sessionDefineBundle: (s) => engine.sessionDefineBundle(s),
    executeNamed: (n, c) => engine.executeNamed(n, c),
    callTool: (n, a) => engine.callTool(n, a),
  };

  const deps: RunDeps = {
    cluster: () => ({ name: "local", label: "local", local: true }),
    engine: () => slowEngine,
    assemble: (t) => assembleBundle(t.uri, "query q { }\n", emptyWorkspace()),
    confirmWrite: async () => true,
    publishDiagnostics: () => {},
  };
  const orchestrator = new RunOrchestrator(deps);

  const first = orchestrator.run(target(), { spaceId: "s1" });
  await firstParked;
  const second = await orchestrator.run(target(), { spaceId: "s2" });
  releaseFirst?.();
  const firstOutcome = await first;

  assert.equal(second.status, "ok");
  assert.equal(firstOutcome.status, "superseded");
});
