// The decisions src/auth/signin.ts makes on the way from a command to a stored
// credential: whether a cluster can be signed into at all, what gets persisted
// and in what order, and which of the ten failure kinds an operator should
// actually be shown.
//
// The browser flow itself is covered by test/authFlow.test.ts; this drives a
// stub runner, because what is under test here is the wiring around it.

import test from "node:test";
import assert from "node:assert/strict";

import { AuthFlowError, type AuthFlowErrorKind } from "../src/auth/errors.js";
import type { AuthFlowTokens } from "../src/auth/flow.js";
import {
  canSignIn,
  describeSignInFailure,
  performSignIn,
  signInCanRecover,
  type PerformSignInDeps,
  type SignInCredentials,
} from "../src/auth/signin.js";
import type { ClusterConfig } from "../src/clusters/model.js";

function cluster(overrides: Partial<ClusterConfig> = {}): ClusterConfig {
  return { name: "local", endpoint: "cockpit.local.znas.io:443", ...overrides };
}

function tokens(overrides: Partial<AuthFlowTokens> = {}): AuthFlowTokens {
  return {
    accessToken: "header.payload.signature",
    refreshToken: "refresh-1",
    expiresInSeconds: 900,
    expiresAtEpochSeconds: 1_800_000_900,
    clientId: "mcp_minted",
    clientIdWasRegistered: true,
    ...overrides,
  };
}

interface Recorder {
  deps: PerformSignInDeps;
  clientIds: Array<{ clusterName: string; clientId: string }>;
  persisted: Array<{ clusterName: string; credentials: SignInCredentials }>;
  signedOut: string[];
  order: string[];
}

function recorder(
  run: PerformSignInDeps["runFlow"] = async () => tokens(),
  failures: { clientId?: Error; persist?: Error } = {},
): Recorder {
  const rec: Recorder = {
    clientIds: [],
    persisted: [],
    signedOut: [],
    order: [],
    deps: undefined as unknown as PerformSignInDeps,
  };
  rec.deps = {
    runFlow: async (c, signal) => {
      rec.order.push("flow");
      return run(c, signal);
    },
    persistClientId: async (clusterName, clientId) => {
      rec.order.push("clientId");
      if (failures.clientId !== undefined) throw failures.clientId;
      rec.clientIds.push({ clusterName, clientId });
    },
    store: {
      persistSignIn: async (clusterName, credentials) => {
        rec.order.push("tokens");
        if (failures.persist !== undefined) throw failures.persist;
        rec.persisted.push({ clusterName, credentials });
      },
      signOut: async (clusterName) => {
        rec.signedOut.push(clusterName);
      },
    },
  };
  return rec;
}

// -----------------------------------------------------------------------------
// canSignIn
// -----------------------------------------------------------------------------

test("canSignIn accepts a cluster whose identity service is derivable", () => {
  assert.equal(canSignIn(cluster({ issuer: "https://identity.example.com" })), true);
  assert.equal(canSignIn(cluster({ domain: "local.znas.io" })), true);
  // The endpoint convention's other half: cockpit.<domain> implies its sibling.
  assert.equal(canSignIn(cluster()), true);
});

test("canSignIn refuses a cluster that names no identity service", () => {
  // Nothing names an issuer: no `issuer`, no `domain`, and an endpoint with no
  // `cockpit.` prefix to imply one. Offering sign-in here would be a button
  // whose only outcome is a misconfigured error.
  assert.equal(canSignIn(cluster({ endpoint: "10.0.0.4:50051" })), false);
  assert.equal(canSignIn(cluster({ endpoint: "" })), false);
});

// -----------------------------------------------------------------------------
// signInCanRecover
// -----------------------------------------------------------------------------

test("signInCanRecover covers exactly the credential failures", () => {
  assert.equal(signInCanRecover("missingCredential"), true);
  assert.equal(signInCanRecover("credentialExpired"), true);
  assert.equal(signInCanRecover("wrongTokenClass"), true);
  // A fresh token does not make an endpoint appear, nor a cluster reachable.
  assert.equal(signInCanRecover("notConfigured"), false);
  assert.equal(signInCanRecover("unreachable"), false);
  assert.equal(signInCanRecover("lost"), false);
});

// -----------------------------------------------------------------------------
// performSignIn
// -----------------------------------------------------------------------------

test("performSignIn persists a freshly minted client_id and the tokens", async () => {
  const rec = recorder();
  const outcome = await performSignIn(cluster(), rec.deps);

  assert.deepEqual(rec.clientIds, [{ clusterName: "local", clientId: "mcp_minted" }]);
  assert.deepEqual(rec.persisted, [
    {
      clusterName: "local",
      credentials: {
        accessToken: "header.payload.signature",
        refreshToken: "refresh-1",
        expiresInSeconds: 900,
        expiresAtEpochSeconds: 1_800_000_900,
      },
    },
  ]);
  assert.equal(outcome.clientId, "mcp_minted");
  assert.equal(outcome.clientIdPersisted, true);
  assert.equal(outcome.expiresInSeconds, 900);
});

test("performSignIn writes the client_id BEFORE the tokens", async () => {
  // The ordering is the point: tokens can be re-earned by signing in again, a
  // client_id that is minted and then dropped cannot be recovered at all.
  const rec = recorder();
  await performSignIn(cluster(), rec.deps);
  assert.deepEqual(rec.order, ["flow", "clientId", "tokens"]);
});

test("performSignIn leaves an already-registered client_id alone", async () => {
  const rec = recorder(async () =>
    tokens({ clientId: "existing", clientIdWasRegistered: false }),
  );
  const outcome = await performSignIn(cluster({ clientId: "existing" }), rec.deps);

  assert.deepEqual(rec.clientIds, [], "an unchanged client_id must not be rewritten");
  assert.equal(outcome.clientIdPersisted, false);
  assert.equal(rec.persisted.length, 1, "the tokens are still stored");
});

test("performSignIn writes a client_id that disagrees with the file", async () => {
  const rec = recorder(async () =>
    tokens({ clientId: "reconciled", clientIdWasRegistered: false }),
  );
  await performSignIn(cluster({ clientId: "stale" }), rec.deps);
  assert.deepEqual(rec.clientIds, [{ clusterName: "local", clientId: "reconciled" }]);
});

test("performSignIn stores nothing when the flow fails", async () => {
  const rec = recorder(async () => {
    throw new AuthFlowError("timeout", "nobody finished the page");
  });
  await assert.rejects(() => performSignIn(cluster(), rec.deps), /nobody finished the page/);
  assert.deepEqual(rec.clientIds, []);
  assert.deepEqual(rec.persisted, []);
});

test("performSignIn propagates a token-store failure with the client_id already safe", async () => {
  const rec = recorder(undefined, { persist: new Error("secret storage refused") });
  await assert.rejects(() => performSignIn(cluster(), rec.deps), /secret storage refused/);
  assert.deepEqual(
    rec.clientIds,
    [{ clusterName: "local", clientId: "mcp_minted" }],
    "the registration is committed first precisely so it survives this",
  );
});

test("performSignIn hands its AbortSignal to the flow", async () => {
  const controller = new AbortController();
  let seen: AbortSignal | undefined;
  const rec = recorder(async (_c, signal) => {
    seen = signal;
    return tokens();
  });
  await performSignIn(cluster(), { ...rec.deps, signal: controller.signal });
  assert.equal(seen, controller.signal, "cancellation must reach the flow, not stop at the UI");
});

// -----------------------------------------------------------------------------
// describeSignInFailure
// -----------------------------------------------------------------------------

const ALL_KINDS: AuthFlowErrorKind[] = [
  "misconfigured",
  "registrationFailed",
  "bindFailed",
  "timeout",
  "cancelled",
  "browserUnavailable",
  "authorizationDenied",
  "stateMismatch",
  "invalidCallback",
  "exchangeRejected",
];

test("describeSignInFailure handles every kind in the taxonomy", () => {
  // Exhaustive by construction: a kind added to errors.ts and forgotten here
  // would fall off the switch and produce an undefined advice string.
  for (const kind of ALL_KINDS) {
    const report = describeSignInFailure("local", new AuthFlowError(kind, `the ${kind} sentence`));
    if (kind === "cancelled") {
      assert.equal(report.level, "silent", "a user who cancelled already knows");
      assert.equal(report.message, "");
      continue;
    }
    assert.notEqual(report.message, "", `${kind} produced no message`);
    assert.match(
      report.message,
      /^memQL: signing in to "local" failed\./,
      `${kind} must name the cluster`,
    );
    assert.ok(
      report.message.includes(`the ${kind} sentence`),
      `${kind} must carry the flow's own explanation through`,
    );
    assert.ok(
      report.message.length > `memQL: signing in to "local" failed. the ${kind} sentence`.length,
      `${kind} must add a next action, not only restate the failure`,
    );
  }
});

test("describeSignInFailure reports a timeout as a warning, not an error", () => {
  const report = describeSignInFailure("local", new AuthFlowError("timeout", "no callback"));
  assert.equal(report.level, "warning");
  assert.equal(report.retryable, true);
});

test("describeSignInFailure refuses to mark a security refusal retryable", () => {
  const report = describeSignInFailure(
    "local",
    new AuthFlowError("stateMismatch", "the callback carried the wrong state"),
  );
  assert.equal(report.level, "error");
  assert.equal(
    report.retryable,
    false,
    "a forged or replayed callback wants a human looking at it, not a retry button",
  );
});

test("describeSignInFailure marks environment limitations not-retryable", () => {
  for (const kind of ["misconfigured", "browserUnavailable"] as AuthFlowErrorKind[]) {
    assert.equal(describeSignInFailure("local", new AuthFlowError(kind, "x")).retryable, false);
  }
  for (const kind of [
    "registrationFailed",
    "bindFailed",
    "authorizationDenied",
    "invalidCallback",
    "exchangeRejected",
  ] as AuthFlowErrorKind[]) {
    assert.equal(describeSignInFailure("local", new AuthFlowError(kind, "x")).retryable, true);
  }
});

test("describeSignInFailure branches on kind, never on message text", () => {
  // A kind whose message reads like a cancellation is still not one. This is
  // the property the taxonomy exists to give callers.
  const report = describeSignInFailure(
    "local",
    new AuthFlowError("exchangeRejected", "cancelled by the authorization server"),
  );
  assert.equal(report.level, "error");
});

test("describeSignInFailure survives a rejection that is not an AuthFlowError", () => {
  const report = describeSignInFailure("local", new TypeError("fetch is not a function"));
  assert.equal(report.level, "error");
  assert.match(report.message, /fetch is not a function/);
  assert.equal(report.retryable, false);
});
