// Tests for the automation-run surface (memql#3310) -- the only invoke path
// an automation has on any surface.
//
// Three things matter here beyond the usual envelope/unwrap coverage.
//
// First, the STREAM. A run is many frames correlated by request_id, not a
// round-trip. The client has to reassemble accepted -> steps -> complete,
// call the caller's hooks as they land, and resolve once -- and it has to do
// that over the REAL Dispatcher, whose routing depends on the run frames being
// recognised by streamRequestId. That routing is the piece most likely to break
// silently: a run whose frames fall through to the event listeners hangs
// forever rather than failing.
//
// Second, the REFUSAL / FAILURE SPLIT. A run the engine refused (unknown name,
// wrong role, no node picked it up) never started and throws. A run that
// started and FAILED resolves normally, with its step trace intact -- because
// a developer asking "what does this automation do" got their answer, and it
// has a timeline worth rendering.
//
// Third, the BANNER. Session-define does not cover automations, so a run always
// exercises the deployed definition. The engine says so in a field, and the
// client must surface it rather than swallow it.

import test from "node:test";
import assert from "node:assert/strict";

import {
  AutomationClient,
  AutomationRunError,
  CODE_NOT_FOUND,
  CODE_PERMISSION_DENIED,
  CODE_UNAVAILABLE,
} from "../src/automation/automationRun.js";
import { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

// ---------------------------------------------------------------------
// MockDispatcher -- only the methods this surface touches, kept local so
// each test file stays self-contained (mirrors deploy.test.ts).
// ---------------------------------------------------------------------

class MockDispatcher {
  readonly sent: ClientMessage[] = [];
  private streams = new Map<string, (msg: ServerMessage) => void>();
  private nextId = 0;
  sendThrows: Error | null = null;

  send(msg: ClientMessage): string {
    if (this.sendThrows) throw this.sendThrows;
    const id = msg.messageId ?? `mock-${this.nextId++}`;
    this.sent.push({ ...msg, messageId: id });
    return id;
  }

  async sendAndWait(): Promise<ServerMessage> {
    throw new Error("the run surface streams; it must not use sendAndWait");
  }

  registerStream(requestId: string, handler: (msg: ServerMessage) => void): () => void {
    this.streams.set(requestId, handler);
    return () => {
      if (this.streams.get(requestId) === handler) this.streams.delete(requestId);
    };
  }

  /** True once the client has released its stream registration. */
  hasStream(requestId: string): boolean {
    return this.streams.has(requestId);
  }

  lastRun(): Record<string, unknown> {
    const msg = this.sent.at(-1) as unknown as { runAutomation?: Record<string, unknown> };
    if (!msg?.runAutomation) throw new Error("last message was not a runAutomation");
    return msg.runAutomation;
  }

  lastRequestId(): string {
    return this.lastRun().requestId as string;
  }

  /** Push one run frame at the registered listener for the in-flight run. */
  push(frame: Record<string, unknown>): void {
    const requestId = this.lastRequestId();
    this.pushRaw({ automationRunEvent: { requestId, ...frame } });
  }

  /** Push an arbitrary server envelope at the in-flight run's listener. */
  pushRaw(msg: Record<string, unknown>): void {
    const requestId = this.lastRequestId();
    const handler = this.streams.get(requestId);
    if (!handler) throw new Error(`MockDispatcher.pushRaw: no stream registered for ${requestId}`);
    handler(msg as unknown as ServerMessage);
  }

  asDispatcher(): Dispatcher {
    return this as unknown as Dispatcher;
  }
}

function acceptedFrame(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    runId: "run-1",
    accepted: {
      automation: "onParticipantCreated",
      ranDeployedDefinition: true,
      definitionNote: "the DEPLOYED definition on the cluster ran, not your editor buffer",
      triggerKind: "event",
      triggerTopic: "graph.node.created.v1:cognition:participant",
      requestedOnNodeId: "bff-1",
      requestedOnNodeType: "bff",
      targetNodeType: "cognition",
      ...over,
    },
  };
}

// ---------------------------------------------------------------------
// Request envelope
// ---------------------------------------------------------------------

test("run sends a runAutomation envelope carrying every requested field", async () => {
  const mock = new MockDispatcher();
  const client = new AutomationClient(mock.asDispatcher());

  const p = client.run({
    automation: "onParticipantCreated",
    payload: { id: "v1:cognition:participant:abc", status: "active" },
    concept: "v1:cognition:participant",
    targetNodeType: "cognition",
    timeoutMs: 15000,
    includeStepOutput: true,
  });

  const req = mock.lastRun();
  assert.equal(req.automation, "onParticipantCreated");
  assert.deepEqual(req.payload, { id: "v1:cognition:participant:abc", status: "active" });
  assert.equal(req.concept, "v1:cognition:participant");
  assert.equal(req.targetNodeType, "cognition");
  assert.equal(req.timeoutMs, 15000);
  assert.equal(req.includeStepOutput, true);
  assert.ok(typeof req.requestId === "string" && req.requestId.length > 0);

  mock.push(acceptedFrame());
  mock.push({ runId: "run-1", complete: { status: "completed", durationMs: "4", stepCount: 0 } });
  await p;
});

test("a schedule-kind run omits payload entirely -- it fires with an EMPTY event", async () => {
  const mock = new MockDispatcher();
  const client = new AutomationClient(mock.asDispatcher());

  const p = client.run({ automation: "accountDeletionSweep" });

  const req = mock.lastRun();
  assert.equal("payload" in req, false, "an empty payload must not be sent as {}");
  assert.equal("concept" in req, false);
  assert.equal("targetNodeType" in req, false);

  mock.push(acceptedFrame({ triggerKind: "schedule", triggerTopic: "", targetNodeType: "" }));
  mock.push({ runId: "run-1", complete: { status: "completed", durationMs: 9, stepCount: 1 } });

  const result = await p;
  assert.equal(result.accepted.triggerKind, "schedule");
  assert.equal(result.accepted.triggerTopic, "");
});

test("run rejects an empty automation name before touching the wire", async () => {
  const mock = new MockDispatcher();
  const client = new AutomationClient(mock.asDispatcher());
  await assert.rejects(() => client.run({ automation: "   " }), /automation is required/);
  assert.equal(mock.sent.length, 0);
});

// ---------------------------------------------------------------------
// The streamed trace
// ---------------------------------------------------------------------

test("run reassembles accepted -> steps -> complete and fires the hooks live", async () => {
  const mock = new MockDispatcher();
  const client = new AutomationClient(mock.asDispatcher());

  const seenAccepted: string[] = [];
  const seenSteps: string[] = [];

  const p = client.run({
    automation: "onParticipantCreated",
    includeStepOutput: true,
    onAccepted: (a) => seenAccepted.push(a.automation),
    onStep: (s) => seenSteps.push(`${s.stepId}:${s.status}`),
  });

  mock.push(acceptedFrame());
  mock.push({
    runId: "run-1",
    step: { sequence: 0, stepId: "loadRow", status: "success", durationMs: "3", output: { rows: 1 } },
  });
  mock.push({ runId: "run-1", step: { sequence: 1, stepId: "notify", status: "skipped" } });
  mock.push({
    runId: "run-1",
    complete: {
      status: "completed",
      durationMs: "12",
      stepCount: 2,
      executedOnNodeId: "cognition-1",
      executedOnNodeType: "cognition",
    },
  });

  const result = await p;

  // The hooks fired as frames landed, not after the run resolved.
  assert.deepEqual(seenAccepted, ["onParticipantCreated"]);
  assert.deepEqual(seenSteps, ["loadRow:success", "notify:skipped"]);

  assert.equal(result.runId, "run-1");
  assert.equal(result.steps.length, 2);
  assert.equal(result.steps[0]?.stepId, "loadRow");
  assert.equal(result.steps[0]?.durationMs, 3, "protojson renders int64 as a string; it must arrive as a number");
  assert.deepEqual(result.steps[0]?.output, { rows: 1 });
  assert.equal(result.steps[1]?.status, "skipped");
  assert.equal(result.steps[1]?.output, undefined, "a step with no output must not fabricate one");

  assert.equal(result.complete.status, "completed");
  assert.equal(result.complete.durationMs, 12);
  assert.equal(
    result.complete.executedOnNodeType,
    "cognition",
    "the UI has to be able to name the node -- and therefore the cluster -- a run executed against",
  );

  // The banner the UI is required to render.
  assert.equal(result.accepted.ranDeployedDefinition, true);
  assert.match(result.accepted.definitionNote, /DEPLOYED/);
  assert.notEqual(
    result.accepted.requestedOnNodeId,
    result.complete.executedOnNodeId,
    "this fixture models a cross-node run",
  );

  assert.equal(mock.hasStream(mock.lastRequestId()), false, "the stream registration must be released");
});

test("a run that STARTED and FAILED resolves with its trace -- it is an answer, not an error", async () => {
  const mock = new MockDispatcher();
  const client = new AutomationClient(mock.asDispatcher());

  const p = client.run({ automation: "brokenAutomation" });
  mock.push(acceptedFrame({ automation: "brokenAutomation" }));
  mock.push({ runId: "run-1", step: { sequence: 0, stepId: "explode", status: "failed", error: "boom" } });
  mock.push({
    runId: "run-1",
    complete: { status: "failed", stepCount: 1, error: "step explode failed: boom" },
  });

  const result = await p;
  assert.equal(result.complete.status, "failed");
  assert.equal(result.complete.error, "step explode failed: boom");
  assert.equal(result.steps[0]?.status, "failed");
  assert.equal(result.steps[0]?.error, "boom");
});

// ---------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------

test("a refused run throws an AutomationRunError carrying the gRPC code", async () => {
  const cases: Array<[number, string, string]> = [
    [CODE_NOT_FOUND, "NOT_FOUND", 'automation "nope" is not registered on this node'],
    [CODE_PERMISSION_DENIED, "PERMISSION_DENIED", "running an automation requires a cluster owner or admin"],
    [CODE_UNAVAILABLE, "UNAVAILABLE", 'no "cognition" node in the mesh picked up the run'],
  ];

  for (const [code, codeName, message] of cases) {
    const mock = new MockDispatcher();
    const client = new AutomationClient(mock.asDispatcher());
    const p = client.run({ automation: "nope" });

    mock.push(acceptedFrame({ automation: "" }));
    mock.push({ runId: "run-1", complete: { status: "refused", errorCode: code, errorMessage: message } });

    await assert.rejects(
      () => p,
      (err: unknown) => {
        assert.ok(err instanceof AutomationRunError);
        assert.equal(err.code, code);
        assert.equal(err.codeName, codeName);
        assert.equal(err.runId, "run-1");
        assert.ok(err.message.includes(message));
        // The accepted frame rides along so a UI can still say who refused.
        assert.equal(err.accepted?.requestedOnNodeId, "bff-1");
        return true;
      },
    );
  }
});

test("isPermissionDenied and isUnavailable classify a refusal without string-matching", async () => {
  const mock = new MockDispatcher();
  const client = new AutomationClient(mock.asDispatcher());
  const p = client.run({ automation: "x", targetNodeType: "cognition" });
  mock.push(acceptedFrame());
  mock.push({
    runId: "run-1",
    complete: { status: "refused", errorCode: CODE_UNAVAILABLE, errorMessage: "nobody answered" },
  });

  await assert.rejects(p, (err: unknown) => {
    assert.ok(err instanceof AutomationRunError);
    assert.equal(err.isUnavailable, true);
    assert.equal(err.isPermissionDenied, false);
    return true;
  });
});

test("an interceptor refusal arrives as a queryError and still settles the run", async () => {
  const mock = new MockDispatcher();
  const client = new AutomationClient(mock.asDispatcher());

  const p = client.run({ automation: "x" });
  const requestId = mock.lastRequestId();

  // The badge gate and the surface-pinning interceptors reject BEFORE the
  // handler runs, so their refusal arrives as a QueryError rather than as a
  // run frame. Left unhandled it would park the caller forever.
  mock.pushRaw({ queryError: { requestId, error: { message: "badge_grant_restricted" } } });

  await assert.rejects(p, (err: unknown) => {
    assert.ok(err instanceof AutomationRunError);
    assert.equal(err.code, CODE_PERMISSION_DENIED);
    assert.match(err.message, /badge_grant_restricted/);
    return true;
  });
});

test("abort settles the run and releases the stream registration", async () => {
  const mock = new MockDispatcher();
  const client = new AutomationClient(mock.asDispatcher());
  const ctl = new AbortController();

  const p = client.run({ automation: "x", signal: ctl.signal });
  const requestId = mock.lastRequestId();
  ctl.abort();

  await assert.rejects(p, /aborted/);
  assert.equal(mock.hasStream(requestId), false);
});

// ---------------------------------------------------------------------
// Real Dispatcher routing
// ---------------------------------------------------------------------

// The frames route by request_id through streamRequestId. If a run's frames
// are not recognised there they fall through to the generic event listeners
// and the caller parks forever -- a hang, not a failure. This drives the REAL
// Dispatcher over a fake socket to pin that path.
class FakeWebSocket {
  static readonly OPEN = 1;
  readonly OPEN = 1;
  readyState = FakeWebSocket.OPEN;
  binaryType: "blob" | "arraybuffer" = "blob";
  readonly outbound: string[] = [];
  private listeners: Record<string, Set<(ev: unknown) => void>> = {};

  addEventListener(type: string, fn: (ev: unknown) => void): void {
    (this.listeners[type] ??= new Set()).add(fn);
  }
  removeEventListener(type: string, fn: (ev: unknown) => void): void {
    this.listeners[type]?.delete(fn);
  }
  send(data: string): void {
    this.outbound.push(data);
  }
  close(): void {}
  pushServer(payload: unknown): void {
    for (const fn of this.listeners.message ?? []) {
      fn({ type: "message", data: JSON.stringify(payload) });
    }
  }
}

test("run frames route through the real Dispatcher by request_id", async () => {
  const socket = new FakeWebSocket();
  const dispatcher = new Dispatcher({ socket: socket as unknown as WebSocket, logger: null });
  const client = new AutomationClient(dispatcher);

  const p = client.run({ automation: "onParticipantCreated", targetNodeType: "cognition" });

  const sent = JSON.parse(socket.outbound.at(-1)!) as { runAutomation: { requestId: string } };
  const requestId = sent.runAutomation.requestId;
  assert.ok(requestId);

  // correlateTo is set by the server on every frame. The dispatcher checks it
  // FIRST, so a run must not be routed by it -- there is no pending entry
  // (the client uses send, not sendAndWait) and every frame has to reach the
  // per-requestId stream listener instead.
  const correlateTo = (JSON.parse(socket.outbound.at(-1)!) as { messageId: string }).messageId;

  socket.pushServer({
    correlateTo,
    automationRunEvent: {
      requestId,
      runId: "run-9",
      accepted: { automation: "onParticipantCreated", ranDeployedDefinition: true, definitionNote: "n" },
    },
  });
  socket.pushServer({
    correlateTo,
    automationRunEvent: {
      requestId,
      runId: "run-9",
      step: { sequence: 0, stepId: "s1", status: "success", durationMs: "1" },
    },
  });
  socket.pushServer({
    correlateTo,
    automationRunEvent: {
      requestId,
      runId: "run-9",
      complete: { status: "completed", stepCount: 1, executedOnNodeType: "cognition" },
    },
  });

  const result = await p;
  assert.equal(result.runId, "run-9");
  assert.equal(result.steps.length, 1);
  assert.equal(result.complete.executedOnNodeType, "cognition");
  dispatcher.stop();
});
