// Mock-dispatcher tests for the deploy console surface (memql#3311):
// the nine DeployControlService RPCs bridged onto MemqlService.Stream.
//
// Two things matter here and nothing else does. First, each method must emit
// the RIGHT arm of the DeployControlMsg request oneof -- a client that sends
// `promote` when the caller asked for `deployStaging` ships a release to the
// wrong environment. Second, a server denial must surface as a
// DeployControlError carrying the gRPC code verbatim, so a portal can tell
// "you are not allowed" from "wrong node" without string matching. The gate
// itself is server-side and tested in component/grpc's parity suite; this
// suite only proves the client does not swallow or mistranslate its verdict.

import test from "node:test";
import assert from "node:assert/strict";

import {
  getDeploymentStatus,
  suggestNextVersion,
  deployStaging,
  promote,
  rollback,
  rolloutAction,
  cutVersion,
  deploy,
  rollbackDeployment,
  DeployControlError,
} from "../src/deploy/deployControl.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

// MockDispatcher mirrors the stand-in used by identity.test.ts. Kept local
// rather than shared so each suite stays self-contained.
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

  reply(payload: Record<string, unknown>): void {
    const last = this.sent.at(-1);
    if (!last) throw new Error("MockDispatcher.reply: nothing sent yet");
    const resolver = this.pendingReplies.get(last.messageId);
    if (!resolver) throw new Error(`MockDispatcher.reply: no pending entry for ${last.messageId}`);
    this.pendingReplies.delete(last.messageId);
    resolver({ correlateTo: last.messageId, ...payload } as ServerMessage);
  }

  lastDeployControl(): Record<string, unknown> {
    const last = this.sent.at(-1);
    if (!last) throw new Error("MockDispatcher.lastDeployControl: nothing sent yet");
    const env = last.msg as unknown as { deployControl?: Record<string, unknown> };
    if (!env.deployControl) throw new Error("last envelope is not a deployControl");
    return env.deployControl;
  }

  lastRequestId(): string {
    const rid = this.lastDeployControl().requestId;
    if (typeof rid !== "string" || !rid) throw new Error("no requestId on the envelope");
    return rid;
  }

  asDispatcher(): Dispatcher {
    return this as unknown as Dispatcher;
  }
}

// ---------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------

test("getDeploymentStatus -- sends the read arm and flattens the reply", async () => {
  const mock = new MockDispatcher();
  const promise = getDeploymentStatus(mock.asDispatcher(), "prod");

  assert.deepEqual(mock.lastDeployControl().getDeploymentStatus, { env: "prod" });

  mock.reply({
    deployControlResult: {
      requestId: mock.lastRequestId(),
      rpc: "GetDeploymentStatus",
      deploymentStatus: {
        env: "prod",
        version: "1.4.2",
        engineVersion: "1.4.0",
        components: [{ name: "acrmemql.azurecr.io/memql-bff", digest: "sha256:abc" }],
        argocd: { syncStatus: "Synced", outOfSync: false },
        rollouts: [{ name: "bff", kind: "bluegreen", phase: "Healthy" }],
        gateResult: { result: "pass", legs: [{ name: "readyz", passed: true }] },
      },
    },
  });

  const s = await promise;
  assert.equal(s.env, "prod");
  assert.equal(s.version, "1.4.2");
  assert.equal(s.components.length, 1);
  assert.equal(s.components[0]?.digest, "sha256:abc");
  // Absent protojson fields default rather than surfacing as undefined.
  assert.equal(s.components[0]?.repo, "");
  assert.equal(s.argocd.syncStatus, "Synced");
  assert.equal(s.argocd.outOfSync, false);
  assert.equal(s.rollouts[0]?.canaryWeight, 0);
  assert.equal(s.gateResult.result, "pass");
  assert.equal(s.gateResult.legs[0]?.passed, true);
});

test("suggestNextVersion -- sends the read arm and flattens the proposals", async () => {
  const mock = new MockDispatcher();
  const promise = suggestNextVersion(mock.asDispatcher(), "staging");

  assert.deepEqual(mock.lastDeployControl().suggestNextVersion, { env: "staging" });

  mock.reply({
    deployControlResult: {
      requestId: mock.lastRequestId(),
      rpc: "SuggestNextVersion",
      nextVersion: {
        currentVersion: "1.2.3",
        nextMajor: "2.0.0",
        nextMinor: "1.3.0",
        nextPatch: "1.2.4",
        source: "deployment",
      },
    },
  });

  const n = await promise;
  assert.equal(n.currentVersion, "1.2.3");
  assert.equal(n.nextPatch, "1.2.4");
  assert.equal(n.source, "deployment");
});

// ---------------------------------------------------------------------
// Actions -- each must emit its OWN request arm
// ---------------------------------------------------------------------

test("each action emits the matching request arm", async () => {
  const cases: Array<{
    name: string;
    run: (d: Dispatcher) => Promise<unknown>;
    expect: Record<string, unknown>;
  }> = [
    {
      name: "deployStaging",
      run: (d) => deployStaging(d, "1.2.3"),
      expect: { deployStaging: { version: "1.2.3" } },
    },
    {
      name: "promote",
      run: (d) => promote(d, "1.2.3"),
      expect: { promote: { version: "1.2.3" } },
    },
    {
      name: "rollback",
      run: (d) => rollback(d, "prod", "deadbeef"),
      expect: { rollback: { env: "prod", commitSha: "deadbeef" } },
    },
    {
      name: "rolloutAction",
      run: (d) => rolloutAction(d, "prod", "bff", "abort"),
      expect: { rolloutAction: { env: "prod", rollout: "bff", action: "abort" } },
    },
    {
      name: "cutVersion",
      run: (d) => cutVersion(d, { env: "prod", bump: "minor" }),
      expect: { cutVersion: { env: "prod", bump: "minor" } },
    },
    {
      name: "deploy",
      run: (d) => deploy(d, "v1:cluster:deployment.d1"),
      expect: { deploy: { deploymentId: "v1:cluster:deployment.d1" } },
    },
    {
      name: "rollbackDeployment",
      run: (d) => rollbackDeployment(d, "v1:cluster:deployment.d0"),
      expect: { rollbackDeployment: { toDeploymentId: "v1:cluster:deployment.d0" } },
    },
  ];

  for (const c of cases) {
    const mock = new MockDispatcher();
    const promise = c.run(mock.asDispatcher());
    const sent = mock.lastDeployControl();
    for (const [arm, body] of Object.entries(c.expect)) {
      assert.deepEqual(sent[arm], body, `${c.name} must send the ${arm} arm`);
    }
    mock.reply({
      deployControlResult: {
        requestId: mock.lastRequestId(),
        action: { ok: true, message: "done", auditEventId: "aud-1", correlationId: "aud-1" },
      },
    });
    await promise;
  }
});

test("an action result carries the audit event id, as the unary path does", async () => {
  const mock = new MockDispatcher();
  const promise = cutVersion(mock.asDispatcher(), { env: "prod", bump: "patch" });
  mock.reply({
    deployControlResult: {
      requestId: mock.lastRequestId(),
      rpc: "CutVersion",
      action: {
        ok: true,
        message: "cut 1.2.4",
        auditEventId: "aud-xyz",
        correlationId: "aud-xyz",
        details: { env: "prod", version: "1.2.4" },
      },
    },
  });
  const r = await promise;
  assert.equal(r.ok, true);
  assert.equal(r.auditEventId, "aud-xyz");
  assert.equal(r.details.version, "1.2.4");
});

test("cutVersion -- an explicit version omits bump", async () => {
  const mock = new MockDispatcher();
  const promise = cutVersion(mock.asDispatcher(), { env: "prod", version: "2.0.0" });
  assert.deepEqual(mock.lastDeployControl().cutVersion, { env: "prod", version: "2.0.0" });
  mock.reply({
    deployControlResult: { requestId: mock.lastRequestId(), action: { ok: true } },
  });
  await promise;
});

// ---------------------------------------------------------------------
// Denials
// ---------------------------------------------------------------------

test("a PermissionDenied denial raises DeployControlError with the code intact", async () => {
  const mock = new MockDispatcher();
  const promise = rollbackDeployment(mock.asDispatcher(), "d0");
  mock.reply({
    queryError: {
      requestId: mock.lastRequestId(),
      error: {
        code: "PermissionDenied",
        message: "deploy console: rollback_deployment requires owner role (have \"admin\")",
      },
    },
  });
  await assert.rejects(promise, (err: unknown) => {
    assert.ok(err instanceof DeployControlError);
    // The code must round-trip verbatim: a portal decides whether to show
    // "ask an owner" vs "wrong node" off this, never off the message text.
    assert.equal(err.code, "PermissionDenied");
    return true;
  });
});

test("Unimplemented (wrong node) is distinguishable from a denial", async () => {
  const mock = new MockDispatcher();
  const promise = getDeploymentStatus(mock.asDispatcher(), "prod");
  mock.reply({
    queryError: {
      requestId: mock.lastRequestId(),
      error: {
        code: "Unimplemented",
        message: "deploy console: this node does not host DeployControlService",
      },
    },
  });
  await assert.rejects(promise, (err: unknown) => {
    assert.ok(err instanceof DeployControlError);
    assert.equal(err.code, "Unimplemented");
    return true;
  });
});

test("an unexpected reply envelope is rejected rather than silently coerced", async () => {
  const mock = new MockDispatcher();
  const promise = deploy(mock.asDispatcher(), "d1");
  mock.reply({ myAccessResult: { requestId: mock.lastRequestId() } });
  await assert.rejects(promise, /unexpected reply envelope/);
});

test("an AbortSignal cancels an in-flight call", async () => {
  const mock = new MockDispatcher();
  const ac = new AbortController();
  const promise = promote(mock.asDispatcher(), "1.2.3", ac.signal);
  ac.abort();
  await assert.rejects(promise, /aborted/);
});
