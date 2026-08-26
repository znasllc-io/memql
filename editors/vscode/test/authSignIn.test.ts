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
  selectSignInRunner,
  signInCanRecover,
  type AuthFlowRunner,
  type PerformSignInDeps,
  type SignInCredentials,
} from "../src/auth/signin.js";
import type { ClusterConfig } from "../src/clusters/model.js";

function cluster(overrides: Partial<ClusterConfig> = {}): ClusterConfig {
  return { name: "local", endpoint: "api.memql.localhost:443", ...overrides };
}

function tokens(overrides: Partial<AuthFlowTokens> = {}): AuthFlowTokens {
  return {
    accessToken: "header.payload.signature",
    refreshToken: "refresh-1",
    expiresInSeconds: 900,
    expiresAtEpochSeconds: 1_800_000_900,
    clientId: "memql-vscode",
    ...overrides,
  };
}

interface Recorder {
  deps: PerformSignInDeps;
  persisted: Array<{ clusterName: string; credentials: SignInCredentials }>;
  signedOut: string[];
  order: string[];
}

function recorder(
  run: PerformSignInDeps["runFlow"] = async () => tokens(),
  failures: { persist?: Error } = {},
): Recorder {
  const rec: Recorder = {
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
    store: {
      persistSignIn: async (clusterName, credentials) => {
        rec.order.push("tokens");
        if (failures.persist !== undefined) throw failures.persist;
        rec.persisted.push({ clusterName, credentials });
      },
      signOut: async (clusterName) => {
        rec.signedOut.push(clusterName);
        return { kind: "nothingToRevoke" as const };
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
  assert.equal(canSignIn(cluster({ domain: "memql.localhost" })), true);
  // The endpoint convention's other half: api.<domain> implies its sibling.
  assert.equal(canSignIn(cluster()), true);
});

test("canSignIn refuses a cluster that names no identity service", () => {
  // Nothing names an issuer: no `issuer`, no `domain`, and an endpoint with no
  // `api.` prefix to imply one. Offering sign-in here would be a button
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

test("performSignIn persists the tokens", async () => {
  const rec = recorder();
  const outcome = await performSignIn(cluster(), rec.deps);

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
  assert.equal(outcome.clientId, "memql-vscode");
  assert.equal(outcome.expiresInSeconds, 900);
});

test("performSignIn does NOT write clusters.yaml (memql#4517)", async () => {
  // It used to, and the deletion is deliberate. The client_id was minted by
  // RFC 7591 registration, which made it unrecoverable state that had to be
  // committed before the tokens. Nothing is minted any more -- the id is the
  // operator's own override or a compiled-in constant -- and writing a
  // constant into the file would only turn today's default into tomorrow's
  // pin. `order` is the assertion: exactly two steps, and no third.
  const rec = recorder();
  await performSignIn(cluster(), rec.deps);
  assert.deepEqual(rec.order, ["flow", "tokens"]);
});

test("performSignIn reports an operator's clientId override unchanged", async () => {
  // An entry carrying an id from the deleted registration path must keep
  // working: it is read as an override and neither migrated nor rewritten.
  const rec = recorder(async () => tokens({ clientId: "existing" }));
  const outcome = await performSignIn(cluster({ clientId: "existing" }), rec.deps);

  assert.equal(outcome.clientId, "existing");
  assert.equal(rec.persisted.length, 1, "the tokens are still stored");
});

test("performSignIn stores nothing when the flow fails", async () => {
  const rec = recorder(async () => {
    throw new AuthFlowError("timeout", "nobody finished the page");
  });
  await assert.rejects(() => performSignIn(cluster(), rec.deps), /nobody finished the page/);
  assert.deepEqual(rec.persisted, []);
});

test("performSignIn propagates a token-store failure", async () => {
  const rec = recorder(undefined, { persist: new Error("secret storage refused") });
  await assert.rejects(() => performSignIn(cluster(), rec.deps), /secret storage refused/);
  assert.deepEqual(rec.persisted, []);
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
      /^MemQL: signing in to "local" failed\./,
      `${kind} must name the cluster`,
    );
    assert.ok(
      report.message.includes(`the ${kind} sentence`),
      `${kind} must carry the flow's own explanation through`,
    );
    assert.ok(
      report.message.length > `MemQL: signing in to "local" failed. the ${kind} sentence`.length,
      `${kind} must add a next action, not only restate the failure`,
    );
  }
});

test("describeSignInFailure reports a timeout as a warning, not an error", () => {
  const report = describeSignInFailure("local", new AuthFlowError("timeout", "no callback"));
  assert.equal(report.level, "warning");
  assert.equal(report.retryable, true);
  // Since timeout stopped triggering the device fallback (memql#4594), the
  // advice is the only remaining route to the device grant for a host that
  // cannot receive the callback -- so it must NAME the command.
  assert.ok(
    report.message.includes("MemQL: Sign In With a Device Code"),
    `the timeout advice must name the device-code command; got: ${report.message}`,
  );
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

// -----------------------------------------------------------------------------
// Which grant runs (memql#3515)
// -----------------------------------------------------------------------------
//
// The defect these pin is not a wrong result -- it is a capability that shipped
// unreachable. `MemQL: Sign In` called a PRIVATE function in extension.ts that
// ran loopback alone, while an exported function of the SAME NAME in
// auth/deviceCodeUi.ts ran the loopback-to-device-code fallback and had zero
// importers. Nothing failed for a host that could do loopback. A host that could
// not -- Remote-SSH onto a box whose browser is elsewhere, a hardened network --
// waited out the callback deadline, was told it had failed, and the code to hand
// it a device code was sitting two files away.
//
// selectSignInRunner exists so that choice is somewhere a test can reach.
// Everything around it in extension.ts needs a live editor, which is exactly why
// the one decision worth asserting was the one nothing asserted.

function stubRunner(label: string, seen: string[]): AuthFlowRunner {
  return async () => {
    seen.push(label);
    return {
      accessToken: "A",
      refreshToken: "R",
      clientId: "c",
      expiresInSeconds: 900,
      expiresAtEpochSeconds: 900,
    } as AuthFlowTokens;
  };
}

test("the default sign-in runs the fallback, not loopback alone", async () => {
  const seen: string[] = [];
  const runner = selectSignInRunner("auto", {
    loopbackWithDeviceFallback: stubRunner("fallback", seen),
    deviceCode: stubRunner("device", seen),
  });

  await runner(cluster(), undefined);

  // If this ever reads ["device"] the default silently stopped trying a browser;
  // if the runner map is ever wired to a bare loopback flow, THAT is memql#3515
  // regressing and this assertion is the only thing standing in front of it.
  assert.deepEqual(seen, ["fallback"]);
});

test("the deliberate device-code command skips loopback entirely", async () => {
  const seen: string[] = [];
  const runner = selectSignInRunner("deviceCode", {
    loopbackWithDeviceFallback: stubRunner("fallback", seen),
    deviceCode: stubRunner("device", seen),
  });

  await runner(cluster(), undefined);

  // The whole point of memql#3411's command: a user who already knows their
  // environment must not pay the callback deadline to be told so. Running the
  // fallback here would reintroduce exactly that wait.
  assert.deepEqual(seen, ["device"]);
});

test("the two flows select DIFFERENT runners", async () => {
  // The degenerate failure a pair of positive assertions cannot see on its own:
  // a selector that returned the same runner for both would satisfy each test
  // above if the map happened to hold the same function twice.
  const seen: string[] = [];
  const runners = {
    loopbackWithDeviceFallback: stubRunner("fallback", seen),
    deviceCode: stubRunner("device", seen),
  };
  assert.notEqual(selectSignInRunner("auto", runners), selectSignInRunner("deviceCode", runners));
});
