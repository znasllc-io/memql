// Running a deploy action and reporting what happened (memql#3312).
//
// The load-bearing assertions:
//   - an action that RAN and FAILED is not reported as success (it resolves,
//     with ok=false, which reads as success everywhere else in the extension);
//   - a refusal NAMES the role required, and is never a silent no-op;
//   - the audit id reaches the operator on both success and failure.

import test from "node:test";
import assert from "node:assert/strict";

import {
  CODE_PERMISSION_DENIED,
  CODE_UNAUTHENTICATED,
  CODE_UNIMPLEMENTED,
  DeployControlError,
} from "@znasllc-io/memql-sdk-core/deploy";

import {
  previewNextVersion,
  readDeploymentStatus,
  runDeployAction,
  type DeployActionRequest,
  type DeployControlPort,
} from "../src/deploy/controller.js";

const OK = { ok: true, message: "accepted", auditEventId: "audit-1", correlationId: "c", details: {} };

// A port whose methods all reject unless the test overrode them, so a test
// asserting on `deploy` cannot pass by accidentally routing to `promote`.
function port(over: Partial<DeployControlPort> = {}): DeployControlPort {
  const refuse = (name: string) => async () => {
    throw new Error(`unexpected call to ${name}`);
  };
  return {
    getDeploymentStatus: refuse("getDeploymentStatus"),
    suggestNextVersion: refuse("suggestNextVersion"),
    cutVersion: refuse("cutVersion"),
    deploy: refuse("deploy"),
    promote: refuse("promote"),
    rollbackDeployment: refuse("rollbackDeployment"),
    rolloutAction: refuse("rolloutAction"),
    ...over,
  } as DeployControlPort;
}

// -----------------------------------------------------------------------------
// Routing
// -----------------------------------------------------------------------------

test("each action reaches its own RPC with its own arguments", async () => {
  const calls: string[] = [];
  const p = port({
    cutVersion: async (env, bump, version) => {
      calls.push(`cutVersion:${env}:${bump}:${version}`);
      return OK;
    },
    deploy: async (id) => {
      calls.push(`deploy:${id}`);
      return OK;
    },
    promote: async (version) => {
      calls.push(`promote:${version}`);
      return OK;
    },
    rollbackDeployment: async (id) => {
      calls.push(`rollback:${id}`);
      return OK;
    },
    rolloutAction: async (env, rollout, action) => {
      calls.push(`rollout:${env}:${rollout}:${action}`);
      return OK;
    },
  });

  const requests: DeployActionRequest[] = [
    { id: "cutVersion", env: "staging", bump: "patch", version: "" },
    { id: "deploy", deploymentId: "d-1" },
    { id: "promote", version: "2026.6.21" },
    { id: "rollback", toDeploymentId: "d-0" },
    { id: "rolloutAction", env: "prod", rollout: "bff", subAction: "abort" },
  ];
  for (const request of requests) await runDeployAction(p, request);

  assert.deepEqual(calls, [
    "cutVersion:staging:patch:",
    "deploy:d-1",
    "promote:2026.6.21",
    "rollback:d-0",
    "rollout:prod:bff:abort",
  ]);
});

// -----------------------------------------------------------------------------
// Outcomes
// -----------------------------------------------------------------------------

test("success reports SUCCESS with the audit id", async () => {
  const outcome = await runDeployAction(port({ deploy: async () => OK }), {
    id: "deploy",
    deploymentId: "d-1",
  });
  assert.equal(outcome.kind, "success");
  assert.equal(outcome.auditEventId, "audit-1");
  assert.match(outcome.line, /^SUCCESS: deploy/);
  assert.match(outcome.line, /\(audit audit-1\)/);
});

test("an action that RAN and FAILED is an error, not a success", async () => {
  // The envelope's own `ok` means "the RPC returned", not "the action worked",
  // so this resolves. Reading it as success would print SUCCESS with an audit
  // id pointing at the record of its own failure.
  const outcome = await runDeployAction(
    port({
      deploy: async () => ({
        ok: false,
        message: "overlay is dirty",
        auditEventId: "audit-2",
        correlationId: "c",
        details: {},
      }),
    }),
    { id: "deploy", deploymentId: "d-1" },
  );
  assert.equal(outcome.kind, "error");
  // The engine's own words, verbatim -- an operator may need to match them
  // against a log line.
  assert.match(outcome.line, /^ERROR: overlay is dirty/);
  assert.equal(outcome.auditEventId, "audit-2");
  assert.match(outcome.line, /\(audit audit-2\)/);
});

test("a failure with no message still says which action failed", async () => {
  const outcome = await runDeployAction(
    port({
      promote: async () => ({ ok: false, message: "", auditEventId: "", correlationId: "", details: {} }),
    }),
    { id: "promote", version: "1.0.0" },
  );
  assert.equal(outcome.line, "ERROR: promote failed");
});

test("a refusal names the role required and is flagged as such", async () => {
  const outcome = await runDeployAction(
    port({
      rollbackDeployment: async () => {
        throw new DeployControlError("rollback_deployment", CODE_PERMISSION_DENIED, "requires owner");
      },
    }),
    { id: "rollback", toDeploymentId: "d-0" },
  );
  assert.equal(outcome.kind, "error");
  assert.equal(outcome.permissionDenied, true);
  assert.match(outcome.line, /^ERROR: Roll back requires the owner cluster role/);
  // The engine's message is appended, not replaced -- nothing it said is lost.
  assert.match(outcome.line, /requires owner/);
});

test("a promote refusal names owner or admin, not owner alone", async () => {
  const outcome = await runDeployAction(
    port({
      promote: async () => {
        throw new DeployControlError("promote", CODE_PERMISSION_DENIED, "requires owner or admin");
      },
    }),
    { id: "promote", version: "1.0.0" },
  );
  assert.match(outcome.line, /requires the owner or admin cluster role/);
});

test("a refusal carries no audit id -- there genuinely is none to show", async () => {
  // A denial writes a BLOCKED audit event server-side, but DeployControlResult
  // populates `action` only when the call was permitted, so the id is not on
  // the wire. Inventing a placeholder would be worse than the gap.
  const outcome = await runDeployAction(
    port({
      deploy: async () => {
        throw new DeployControlError("deploy", CODE_PERMISSION_DENIED, "no");
      },
    }),
    { id: "deploy", deploymentId: "d-1" },
  );
  assert.equal(outcome.auditEventId, "");
});

test("an unimplemented service is explained, not reported as a refusal", async () => {
  const outcome = await runDeployAction(
    port({
      deploy: async () => {
        throw new DeployControlError("deploy", CODE_UNIMPLEMENTED, "not hosted");
      },
    }),
    { id: "deploy", deploymentId: "d-1" },
  );
  assert.equal(outcome.permissionDenied, false);
  assert.match(outcome.line, /does not host the deploy-control service/);
});

test("an unauthenticated stream points at the credential, not the role", async () => {
  const outcome = await runDeployAction(
    port({
      deploy: async () => {
        throw new DeployControlError("deploy", CODE_UNAUTHENTICATED, "no actor");
      },
    }),
    { id: "deploy", deploymentId: "d-1" },
  );
  assert.match(outcome.line, /Check the cluster's PAT/);
});

test("a non-DeployControlError still becomes an outcome, never a throw", async () => {
  // A thrown error at a button's call site turns into VS Code's generic
  // command-failure toast and loses both the audit id and the role hint.
  const outcome = await runDeployAction(
    port({
      deploy: async () => {
        throw new Error("socket closed");
      },
    }),
    { id: "deploy", deploymentId: "d-1" },
  );
  assert.equal(outcome.kind, "error");
  assert.equal(outcome.line, "ERROR: socket closed");
});

// -----------------------------------------------------------------------------
// Reads
// -----------------------------------------------------------------------------

test("a refused status read explains the owner/admin gate and says what still works", async () => {
  // The discrepancy an operator hits first: the status read is owner/admin
  // (#728) while topology and history are ordinary concept rows.
  const result = await readDeploymentStatus(
    port({
      getDeploymentStatus: async () => {
        throw new DeployControlError("get_status", CODE_PERMISSION_DENIED, "requires owner or admin");
      },
    }),
    "staging",
  );
  assert.equal(result.status, null);
  assert.match(result.message, /requires the owner or admin cluster role/);
  assert.match(result.message, /Topology and deployment history above are ordinary concept rows/);
});

test("a status read failure never throws -- the panel must not fail to load", async () => {
  const result = await readDeploymentStatus(
    port({
      getDeploymentStatus: async () => {
        throw new Error("timeout");
      },
    }),
    "staging",
  );
  assert.equal(result.status, null);
  assert.match(result.message, /deployment status unavailable: timeout/);
});

test("a successful status read carries no message", async () => {
  const status = { env: "staging", version: "1.0.0" } as Awaited<
    ReturnType<DeployControlPort["getDeploymentStatus"]>
  >;
  const result = await readDeploymentStatus(port({ getDeploymentStatus: async () => status }), "staging");
  assert.equal(result.message, "");
  assert.equal(result.status, status);
});

test("a failed version preview degrades to a message, never a dead Cut button", async () => {
  const result = await previewNextVersion(
    port({
      suggestNextVersion: async () => {
        throw new DeployControlError("suggest_version", CODE_PERMISSION_DENIED, "no");
      },
    }),
    "staging",
  );
  assert.equal(result.suggestion, null);
  assert.match(result.message, /version suggestion requires the owner or admin cluster role/);
});
