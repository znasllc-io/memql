// Mock-dispatcher tests for the deploy-control surface (memql#3311) --
// the nine DeployControlService RPCs bridged onto MemqlService.Stream so
// a WebSocket client can reach them at all.
//
// Two things matter here beyond the usual envelope/unwrap coverage.
//
// First, the ENVELOPE SHAPE. Every method must land in its own inner
// oneof field: the engine dispatches on that discriminant, so a method
// wired to the wrong field would silently invoke a different RPC -- a
// promote firing a rollback is not a bug you want to find in staging.
//
// Second, the REFUSAL. The gate is server-side and authoritative; the
// client's whole job on a denial is to surface it faithfully as a
// DeployControlError carrying the gRPC code, so a UI can tell "you may
// not do this" apart from "this failed". Nothing in the SDK checks a
// role, and these tests assume nothing does.

import test from "node:test";
import assert from "node:assert/strict";

import {
  DeployControlClient,
  DeployControlError,
  CODE_PERMISSION_DENIED,
  CODE_UNIMPLEMENTED,
} from "../src/deploy/deployControl.js";
import type { Dispatcher } from "../src/client/dispatcher.js";
import type { ClientMessage, ServerMessage } from "../src/client/wire.js";

// MockDispatcher mirrors the stand-in used by identity.test.ts: only the
// methods this surface touches, kept local so each test file stays
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

function newClient(): { mock: MockDispatcher; client: DeployControlClient } {
  const mock = new MockDispatcher();
  return { mock, client: new DeployControlClient(mock as unknown as Dispatcher) };
}

// deployControlPayload pulls the outbound envelope's deployControl block
// so a test can assert on the discriminant and its arguments.
function deployControlPayload(mock: MockDispatcher): Record<string, unknown> {
  const sent = mock.lastSent() as unknown as Record<string, unknown>;
  const dc = sent.deployControl as Record<string, unknown> | undefined;
  assert.ok(dc, "envelope must carry a deployControl payload");
  return dc;
}

// okReply wraps a successful result the way the engine does: ok=true and
// no errorCode (protojson omits the zero).
function okReply(result: Record<string, unknown>): Record<string, unknown> {
  return { deployControlResult: { requestId: "r", ok: true, ...result } };
}

// -----------------------------------------------------------------------------
// Envelope shape -- one case per RPC
// -----------------------------------------------------------------------------

// Every method must set its OWN inner oneof field. The engine routes on
// that discriminant, so this table is what keeps a method from invoking a
// different RPC than its name promises.
const envelopeCases: Array<{
  name: string;
  call: (c: DeployControlClient) => Promise<unknown>;
  field: string;
  args: Record<string, unknown>;
}> = [
  {
    name: "getDeploymentStatus",
    call: (c) => c.getDeploymentStatus("staging"),
    field: "getDeploymentStatus",
    args: { env: "staging" },
  },
  {
    name: "suggestNextVersion",
    call: (c) => c.suggestNextVersion("prod"),
    field: "suggestNextVersion",
    args: { env: "prod" },
  },
  {
    name: "deployStaging",
    call: (c) => c.deployStaging("1.2.3"),
    field: "deployStaging",
    args: { version: "1.2.3" },
  },
  {
    name: "promote",
    call: (c) => c.promote("1.2.3"),
    field: "promote",
    args: { version: "1.2.3" },
  },
  {
    name: "rollback",
    call: (c) => c.rollback("prod", "abc1234"),
    field: "rollback",
    args: { env: "prod", commitSha: "abc1234" },
  },
  {
    name: "rolloutAction",
    call: (c) => c.rolloutAction("staging", "bff", "abort"),
    field: "rolloutAction",
    args: { env: "staging", rollout: "bff", action: "abort" },
  },
  {
    name: "cutVersion",
    call: (c) => c.cutVersion("prod", "minor"),
    field: "cutVersion",
    args: { env: "prod", bump: "minor" },
  },
  {
    name: "deploy",
    call: (c) => c.deploy("dep-1"),
    field: "deploy",
    args: { deploymentId: "dep-1" },
  },
  {
    name: "rollbackDeployment",
    call: (c) => c.rollbackDeployment("dep-9"),
    field: "rollbackDeployment",
    args: { toDeploymentId: "dep-9" },
  },
];

for (const c of envelopeCases) {
  test(`deployControl -- ${c.name} sends its own request field`, async () => {
    const { mock, client } = newClient();
    const pending = c.call(client);

    const dc = deployControlPayload(mock);
    assert.equal(typeof dc.requestId, "string");
    assert.ok((dc.requestId as string).length > 0, "requestId must be set for correlation");
    assert.deepEqual(dc[c.field], c.args);

    // Exactly one request field, so the engine's oneof is unambiguous.
    const requestFields = Object.keys(dc).filter((k) => k !== "requestId");
    assert.deepEqual(requestFields, [c.field]);

    mock.reply(okReply({ action: { ok: true }, deploymentStatus: undefined, nextVersion: {} }));
    await pending;
  });
}

// -----------------------------------------------------------------------------
// Result unwrapping
// -----------------------------------------------------------------------------

test("deployControl -- getDeploymentStatus maps the full status", async () => {
  const { mock, client } = newClient();
  const pending = client.getDeploymentStatus("prod");
  mock.reply(
    okReply({
      deploymentStatus: {
        env: "prod",
        version: "1.4.2",
        engineVersion: "0.9.40",
        validatedAt: "2026-08-01T00:00:00Z",
        gate: "pass:gate-7",
        components: [{ name: "acrmemql.azurecr.io/memql-bff", digest: "sha256:abc", repo: "memql" }],
        argocd: { syncStatus: "Synced", healthStatus: "Healthy", outOfSync: false },
        rollouts: [{ name: "bff", kind: "bluegreen", phase: "Healthy", currentStep: 2 }],
        gateResult: { result: "pass", legs: [{ name: "readyz", passed: true }], ranAt: "2026-08-01T00:01:00Z" },
      },
    }),
  );

  const status = await pending;
  assert.equal(status.env, "prod");
  assert.equal(status.version, "1.4.2");
  assert.equal(status.engineVersion, "0.9.40");
  assert.equal(status.components.length, 1);
  assert.equal(status.components[0]?.digest, "sha256:abc");
  assert.equal(status.argocd.syncStatus, "Synced");
  assert.equal(status.argocd.outOfSync, false);
  assert.equal(status.rollouts[0]?.kind, "bluegreen");
  // protojson omits zero values, so an absent canaryWeight must read 0,
  // not undefined -- a UI rendering it would otherwise print "undefined".
  assert.equal(status.rollouts[0]?.canaryWeight, 0);
  assert.equal(status.rollouts[0]?.currentStep, 2);
  assert.equal(status.gateResult.result, "pass");
  assert.equal(status.gateResult.legs[0]?.passed, true);
});

test("deployControl -- an empty status reply reads as empty, not undefined", async () => {
  const { mock, client } = newClient();
  const pending = client.getDeploymentStatus("staging");
  // The engine returns a DeploymentStatus with only env set for a staging
  // overlay carrying no promotion provenance; protojson drops the rest.
  mock.reply(okReply({ deploymentStatus: { env: "staging" } }));

  const status = await pending;
  assert.equal(status.version, "");
  assert.deepEqual(status.components, []);
  assert.deepEqual(status.rollouts, []);
  assert.equal(status.argocd.outOfSync, false);
  assert.deepEqual(status.gateResult.legs, []);
});

test("deployControl -- suggestNextVersion maps the proposals", async () => {
  const { mock, client } = newClient();
  const pending = client.suggestNextVersion("prod");
  mock.reply(
    okReply({
      nextVersion: {
        currentVersion: "1.2.3",
        nextMajor: "2.0.0",
        nextMinor: "1.3.0",
        nextPatch: "1.2.4",
        source: "deployment",
      },
    }),
  );

  const next = await pending;
  assert.equal(next.currentVersion, "1.2.3");
  assert.equal(next.nextMajor, "2.0.0");
  assert.equal(next.nextMinor, "1.3.0");
  assert.equal(next.nextPatch, "1.2.4");
  assert.equal(next.source, "deployment");
});

test("deployControl -- an action returns its audit event id", async () => {
  const { mock, client } = newClient();
  const pending = client.promote("1.2.3");
  mock.reply(
    okReply({
      action: {
        ok: true,
        message: "SUCCESS: promoted",
        auditEventId: "aud-42",
        correlationId: "aud-42",
        details: { env: "prod", version: "1.2.3" },
      },
    }),
  );

  const res = await pending;
  assert.equal(res.ok, true);
  assert.equal(res.auditEventId, "aud-42", "the audit id must reach the operator");
  assert.equal(res.correlationId, "aud-42");
  assert.deepEqual(res.details, { env: "prod", version: "1.2.3" });
});

test("deployControl -- an action that RAN and FAILED resolves with ok=false", async () => {
  // The call was permitted, so it is not an error; the action itself
  // failed. Collapsing these two would leave a UI unable to say whether
  // the operator lacked permission or the deploy broke.
  const { mock, client } = newClient();
  const pending = client.deployStaging("1.2.3");
  mock.reply(okReply({ action: { ok: false, message: "promote.sh: exit 1", auditEventId: "aud-9" } }));

  const res = await pending;
  assert.equal(res.ok, false);
  assert.equal(res.auditEventId, "aud-9", "a failed action is still audited");
  assert.match(res.message, /exit 1/);
});

// -----------------------------------------------------------------------------
// Refusals
// -----------------------------------------------------------------------------

test("deployControl -- PERMISSION_DENIED surfaces as a typed error", async () => {
  const { mock, client } = newClient();
  const pending = client.rollbackDeployment("dep-9");
  mock.reply({
    deployControlResult: {
      requestId: "r",
      errorCode: CODE_PERMISSION_DENIED,
      errorMessage: "deploy console: rollback_deployment requires owner role (have \"admin\")",
    },
  });

  await assert.rejects(pending, (err: unknown) => {
    assert.ok(err instanceof DeployControlError);
    assert.equal(err.code, CODE_PERMISSION_DENIED);
    assert.equal(err.codeName, "PERMISSION_DENIED");
    assert.equal(err.isPermissionDenied, true);
    assert.match(err.message, /requires owner role/);
    return true;
  });
});

test("deployControl -- UNIMPLEMENTED (node has no deploy service) is distinguishable", async () => {
  const { mock, client } = newClient();
  const pending = client.promote("1.0.0");
  mock.reply({
    deployControlResult: { requestId: "r", errorCode: CODE_UNIMPLEMENTED, errorMessage: "no deploy-control service" },
  });

  await assert.rejects(pending, (err: unknown) => {
    assert.ok(err instanceof DeployControlError);
    assert.equal(err.code, CODE_UNIMPLEMENTED);
    assert.equal(err.isPermissionDenied, false, "a missing service is not a permission problem");
    return true;
  });
});

test("deployControl -- a reply with neither ok nor an error code is rejected", async () => {
  // Trusting `ok` alone would read a malformed / truncated reply as a
  // successful action. A missing ok is not a success.
  const { mock, client } = newClient();
  const pending = client.deploy("dep-1");
  mock.reply({ deployControlResult: { requestId: "r" } });

  await assert.rejects(pending, (err: unknown) => {
    assert.ok(err instanceof DeployControlError);
    return true;
  });
});

test("deployControl -- a queryError reply surfaces as an error", async () => {
  const { mock, client } = newClient();
  const pending = client.cutVersion("prod", "patch");
  mock.reply({ queryError: { requestId: "r", error: { code: "internal", message: "boom" } } });

  await assert.rejects(pending, /deploy console: cut_version: boom/);
});

test("deployControl -- an unexpected reply envelope is rejected", async () => {
  const { mock, client } = newClient();
  const pending = client.promote("1.0.0");
  mock.reply({ myAccessResult: { userId: "u1" } });

  await assert.rejects(pending, /unexpected reply envelope/);
});

// -----------------------------------------------------------------------------
// Input validation + cancellation
// -----------------------------------------------------------------------------

test("deployControl -- required arguments are validated before sending", async () => {
  const { mock, client } = newClient();
  await assert.rejects(client.getDeploymentStatus(""), /env is required/);
  await assert.rejects(client.suggestNextVersion(""), /env is required/);
  await assert.rejects(client.deployStaging(""), /version is required/);
  await assert.rejects(client.promote(""), /version is required/);
  await assert.rejects(client.rollback("", "sha"), /env is required/);
  await assert.rejects(client.rollback("prod", ""), /commitSha is required/);
  await assert.rejects(client.rolloutAction("", "bff", "promote"), /env is required/);
  await assert.rejects(client.rolloutAction("prod", "", "promote"), /rollout is required/);
  await assert.rejects(client.rolloutAction("prod", "bff", ""), /action is required/);
  await assert.rejects(client.cutVersion(""), /env is required/);
  await assert.rejects(client.deploy(""), /deploymentId is required/);
  await assert.rejects(client.rollbackDeployment(""), /toDeploymentId is required/);
  assert.equal(mock.sent.length, 0, "a rejected argument must not reach the wire");
});

test("deployControl -- cutVersion omits empty bump/version rather than sending blanks", async () => {
  // The engine defaults an absent bump to patch. Sending "" instead of
  // omitting it is the same on this wire, but the envelope should say
  // what the caller meant.
  const { mock, client } = newClient();
  const pending = client.cutVersion("staging");
  assert.deepEqual(deployControlPayload(mock).cutVersion, { env: "staging" });
  mock.reply(okReply({ action: { ok: true } }));
  await pending;

  const explicit = client.cutVersion("staging", "", "2.0.0");
  assert.deepEqual(deployControlPayload(mock).cutVersion, { env: "staging", version: "2.0.0" });
  mock.reply(okReply({ action: { ok: true } }));
  await explicit;
});

test("deployControl -- an AbortSignal cancels an in-flight call", async () => {
  const { mock, client } = newClient();
  const ctrl = new AbortController();
  const pending = client.getDeploymentStatus("prod", { signal: ctrl.signal });
  assert.equal(mock.sent.length, 1);
  ctrl.abort();
  await assert.rejects(pending, /aborted/);
});

test("deployControl -- a dispatcher is required", () => {
  assert.throws(
    () => new DeployControlClient(undefined as unknown as Dispatcher),
    /dispatcher is required/,
  );
});

// ---------------------------------------------------------------------
// The raw engine message (memql#3339)
// ---------------------------------------------------------------------

// DeployControlError had the same shape problem AutomationRunError did, and no
// helper to compensate with: the portal rendered `${err.code}: ${err.message}`,
// which printed the code twice and the verb once more besides.
test("DeployControlError exposes the raw engine message alongside the formatted one", () => {
  const err = new DeployControlError("rollBack", 7, "requires the owner role");
  assert.equal(err.engineMessage, "requires the owner role");
  assert.equal(err.verb, "rollBack");
  assert.equal(err.codeName, "PERMISSION_DENIED");
  // Unchanged log shape.
  assert.equal(err.message, "deploy console: rollBack: PERMISSION_DENIED: requires the owner role");
});

test("DeployControlError -- an absent engine message is empty, never the (no message) sentinel", () => {
  const err = new DeployControlError("cut", 13, "");
  assert.equal(err.engineMessage, "");
  assert.doesNotMatch(err.engineMessage, /no message/);
  assert.match(err.message, /\(no message\)/);
});
