// The device-code fallback: when it fires, when it must NOT, and how it polls.
//
// The fake identity service below answers all three endpoints the two flows
// touch (/register, /device/code, /oauth/token) and records every request, so a
// case can assert what was NOT sent -- "no device authorization was requested"
// is the whole point of half the cases here.
//
// The loopback half is driven through an injected `startListener` rather than a
// real socket: these cases are about the DECISION taken on each distinguishable
// failure, and the cheapest way to produce a `bindFailed` on a machine that can
// happily bind is to say so.

import test from "node:test";
import assert from "node:assert/strict";

import type { ClusterConfig } from "../src/clusters/model.js";
import type { HttpRequestInit, HttpResponseLike } from "../src/connection/credentials.js";
import {
  DEVICE_GRANT_TYPE,
  MAX_POLL_INTERVAL_SECONDS,
  runDeviceCodeFlow,
  shouldFallBackToDeviceCode,
  signInWithDeviceCodeFallback,
  type DeviceAuthorization,
  type SleepFn,
} from "../src/auth/deviceCode.js";
import { AuthFlowError, isAuthFlowError } from "../src/auth/errors.js";
import type { AuthFlowDeps } from "../src/auth/flow.js";
import type { LoopbackListener } from "../src/auth/loopback.js";
import { persistSignIn, refreshTokenSecretKey, type SecretStore } from "../src/auth/store.js";

const ISSUER = "https://identity.local.znas.io";
const NOW_MS = 1_800_000_000_000;

function cluster(overrides: Partial<ClusterConfig> = {}): ClusterConfig {
  return {
    name: "local",
    endpoint: "cockpit.local.znas.io:443",
    domain: "local.znas.io",
    clientId: "mcp_existing",
    ...overrides,
  };
}

// -----------------------------------------------------------------------------
// A fake identity service.
//
// /oauth/token answers from a QUEUE of scripted replies, so a case can spell
// out "pending, pending, slow_down, then tokens" as data rather than as
// bookkeeping.
// -----------------------------------------------------------------------------

interface TokenReply {
  status: number;
  body: unknown;
}

const PENDING: TokenReply = { status: 400, body: { error: "authorization_pending" } };
function slowDown(interval?: number): TokenReply {
  return {
    status: 400,
    body: interval === undefined ? { error: "slow_down" } : { error: "slow_down", interval },
  };
}
const TOKENS: TokenReply = {
  status: 200,
  body: { access_token: "ACCESS", refresh_token: "REFRESH", token_type: "Bearer", expires_in: 900 },
};

interface FakeIdentity {
  fetch: (url: string, init: HttpRequestInit) => Promise<HttpResponseLike>;
  calls: Array<{ url: string; body: Record<string, unknown> }>;
  urls(): string[];
  tokenCalls(): Array<Record<string, unknown>>;
  deviceRequests(): Array<Record<string, unknown>>;
}

interface FakeIdentityOptions {
  /** Replies for /oauth/token, in order. The last one repeats once exhausted. */
  token?: TokenReply[];
  /** POST /device/code overrides. */
  deviceStatus?: number;
  deviceBody?: unknown;
  /** Called before each /oauth/token reply is produced -- a seam for cancelling mid-poll. */
  onTokenCall?: (index: number) => void;
}

function identity(options: FakeIdentityOptions = {}): FakeIdentity {
  const calls: FakeIdentity["calls"] = [];
  const token = options.token ?? [TOKENS];
  let tokenIndex = 0;

  return {
    calls,
    urls: () => calls.map((c) => c.url),
    tokenCalls: () => calls.filter((c) => c.url.endsWith("/oauth/token")).map((c) => c.body),
    deviceRequests: () => calls.filter((c) => c.url.endsWith("/device/code")).map((c) => c.body),
    fetch: async (url, init) => {
      calls.push({ url, body: JSON.parse(init.body) as Record<string, unknown> });

      if (url.endsWith("/register")) {
        return reply(201, { client_id: "mcp_minted" });
      }
      if (url.endsWith("/device/code")) {
        return reply(
          options.deviceStatus ?? 200,
          options.deviceBody ?? {
            device_code: "mql_dvc_abc",
            user_code: "BCDF-GHJK",
            verification_uri: `${ISSUER}/device`,
            verification_uri_complete: `${ISSUER}/device?user_code=BCDF-GHJK`,
            expires_in: 600,
            interval: 5,
          },
        );
      }
      const index = tokenIndex;
      options.onTokenCall?.(index);
      const scripted = token[Math.min(index, token.length - 1)] ?? TOKENS;
      tokenIndex += 1;
      return reply(scripted.status, scripted.body);
    },
  };
}

function reply(status: number, payload: unknown): HttpResponseLike {
  return {
    ok: status >= 200 && status < 300,
    status,
    text: async () => (typeof payload === "string" ? payload : JSON.stringify(payload)),
  };
}

/** A sleep that never waits but records every interval it was asked for. */
function recordingSleep(): { sleep: SleepFn; seconds: number[] } {
  const seconds: number[] = [];
  return {
    seconds,
    sleep: async (ms, signal) => {
      if (signal?.aborted === true) {
        throw new AuthFlowError("cancelled", "Device sign-in was cancelled.");
      }
      seconds.push(ms / 1000);
    },
  };
}

// -----------------------------------------------------------------------------
// A loopback listener that fails however the case needs it to.
// -----------------------------------------------------------------------------

function listenerThatFails(err: AuthFlowError): AuthFlowDeps["startListener"] {
  return async () => stubListener(() => Promise.reject(err));
}

function stubListener(wait: () => Promise<never> | Promise<Record<string, string>>): LoopbackListener {
  return {
    host: "127.0.0.1",
    port: 54321,
    redirectUri: "http://127.0.0.1:54321/callback",
    waitForCallback: wait as LoopbackListener["waitForCallback"],
    close: () => undefined,
  };
}

function loopbackDeps(fake: FakeIdentity, overrides: Partial<AuthFlowDeps> = {}): AuthFlowDeps {
  return {
    resolveExternalUri: (url) => url,
    openExternal: () => undefined,
    fetch: fake.fetch,
    now: () => NOW_MS,
    ...overrides,
  };
}

async function rejection(promise: Promise<unknown>): Promise<AuthFlowError> {
  try {
    await promise;
  } catch (err) {
    assert.ok(isAuthFlowError(err), `expected an AuthFlowError, got ${String(err)}`);
    return err;
  }
  throw new Error("expected a rejection, but the call resolved");
}

// -----------------------------------------------------------------------------
// The rule itself
// -----------------------------------------------------------------------------

test("the fallback triggers on exactly the environment limitations", () => {
  for (const kind of ["bindFailed", "timeout", "browserUnavailable"] as const) {
    assert.equal(
      shouldFallBackToDeviceCode(new AuthFlowError(kind, "x")),
      true,
      `${kind} must trigger the fallback`,
    );
  }
  for (const kind of [
    "cancelled",
    "stateMismatch",
    "exchangeRejected",
    "authorizationDenied",
    "invalidCallback",
    "misconfigured",
    "registrationFailed",
  ] as const) {
    assert.equal(
      shouldFallBackToDeviceCode(new AuthFlowError(kind, "x")),
      false,
      `${kind} must NOT trigger the fallback`,
    );
  }
  // Not an AuthFlowError at all: no evidence of an environment limitation, so
  // no fallback. Guessing here is the over-eager half of the failure.
  assert.equal(shouldFallBackToDeviceCode(new Error("boom")), false);
});

// -----------------------------------------------------------------------------
// The fallback FIRES
// -----------------------------------------------------------------------------

test("a loopback listener that cannot bind falls back to the device code", async () => {
  const fake = identity();
  const codes: DeviceAuthorization[] = [];
  const fallbacks: AuthFlowError[] = [];

  const tokens = await signInWithDeviceCodeFallback(cluster(), {
    ...loopbackDeps(fake, {
      startListener: listenerThatFails(new AuthFlowError("bindFailed", "port in use")),
    }),
    sleep: recordingSleep().sleep,
    onUserCode: (authorization) => codes.push(authorization),
    onFallback: (reason) => fallbacks.push(reason),
  });

  assert.equal(tokens.accessToken, "ACCESS");
  assert.equal(tokens.refreshToken, "REFRESH");
  assert.deepEqual(
    fake.urls(),
    [`${ISSUER}/device/code`, `${ISSUER}/oauth/token`],
    "the fallback must not re-register: the client_id was already on the cluster",
  );
  assert.equal(codes[0]?.userCode, "BCDF-GHJK");
  assert.equal(codes[0]?.verificationUri, `${ISSUER}/device`);
  assert.equal(fallbacks[0]?.kind, "bindFailed", "the switch is announced, not silent");
});

test("a callback that never arrives falls back to the device code", async () => {
  const fake = identity();

  const tokens = await signInWithDeviceCodeFallback(cluster(), {
    ...loopbackDeps(fake, {
      startListener: listenerThatFails(new AuthFlowError("timeout", "nothing arrived")),
    }),
    sleep: recordingSleep().sleep,
  });

  assert.equal(tokens.accessToken, "ACCESS");
  assert.equal(fake.deviceRequests().length, 1);
});

test("a host that cannot open a browser at all falls back to the device code", async () => {
  // browserUnavailable is a trigger BY DESIGN (errors.ts: "This kind is a
  // FALLBACK TRIGGER"). It is the headless box the device grant exists for.
  const fake = identity();

  const tokens = await signInWithDeviceCodeFallback(cluster(), {
    ...loopbackDeps(fake, {
      startListener: async () => stubListener(() => new Promise(() => {})),
      openExternal: () => {
        throw new Error("no display");
      },
    }),
    sleep: recordingSleep().sleep,
  });

  assert.equal(tokens.accessToken, "ACCESS");
  assert.equal(fake.deviceRequests().length, 1);
});

// -----------------------------------------------------------------------------
// The fallback does NOT fire
// -----------------------------------------------------------------------------

test("a user cancellation is not retried through the device code", async () => {
  const fake = identity();

  const err = await rejection(
    signInWithDeviceCodeFallback(cluster(), {
      ...loopbackDeps(fake, {
        startListener: listenerThatFails(new AuthFlowError("cancelled", "user cancelled")),
      }),
      sleep: recordingSleep().sleep,
    }),
  );

  assert.equal(err.kind, "cancelled");
  assert.deepEqual(fake.deviceRequests(), [], "no device authorization may be requested");
});

test("a state mismatch is not retried through the device code", async () => {
  // A security refusal. Reaching for a second channel would be working around
  // it, which is precisely the bug the rule exists to prevent.
  const fake = identity();

  const err = await rejection(
    signInWithDeviceCodeFallback(cluster(), {
      ...loopbackDeps(fake, {
        startListener: async () =>
          stubListener(async () => ({ code: "AUTHCODE", state: "not-the-state-we-sent" })),
      }),
      sleep: recordingSleep().sleep,
    }),
  );

  assert.equal(err.kind, "stateMismatch");
  assert.deepEqual(fake.deviceRequests(), [], "no device authorization may be requested");
  assert.deepEqual(fake.tokenCalls(), [], "and the code must never be redeemed");
});

test("a rejected token exchange is not retried through the device code", async () => {
  const fake = identity({ token: [{ status: 400, body: { error: "invalid_grant" } }] });
  let sentState = "";

  const err = await rejection(
    signInWithDeviceCodeFallback(cluster(), {
      ...loopbackDeps(fake, {
        // Read the state out of the authorize URL so the callback is accepted
        // and the flow reaches the exchange, which is the step under test.
        openExternal: (url) => {
          sentState = new URL(String(url)).searchParams.get("state") ?? "";
        },
        startListener: async () =>
          stubListener(async () => ({ code: "AUTHCODE", state: sentState })),
      }),
      sleep: recordingSleep().sleep,
    }),
  );

  assert.equal(err.kind, "exchangeRejected");
  assert.deepEqual(fake.deviceRequests(), [], "no device authorization may be requested");
});

// -----------------------------------------------------------------------------
// Polling
// -----------------------------------------------------------------------------

test("polling waits the server's interval before the first poll and between polls", async () => {
  const fake = identity({ token: [PENDING, PENDING, TOKENS] });
  const clock = recordingSleep();

  const tokens = await runDeviceCodeFlow(cluster(), {
    fetch: fake.fetch,
    sleep: clock.sleep,
    now: () => NOW_MS,
  });

  assert.equal(tokens.accessToken, "ACCESS");
  // Three polls, three waits -- including one BEFORE the first poll, which the
  // server's own clock would otherwise answer with slow_down.
  assert.deepEqual(clock.seconds, [5, 5, 5]);
  assert.equal(fake.tokenCalls().length, 3);
});

test("slow_down raises the interval to the value the server sent", async () => {
  // token_device.go raises the PERSISTED interval and judges every later poll
  // against the raised value, so ignoring the number it returns ratchets the
  // client to the ceiling.
  const fake = identity({ token: [PENDING, slowDown(10), PENDING, TOKENS] });
  const clock = recordingSleep();

  const tokens = await runDeviceCodeFlow(cluster(), {
    fetch: fake.fetch,
    sleep: clock.sleep,
    now: () => NOW_MS,
  });

  assert.equal(tokens.accessToken, "ACCESS");
  assert.deepEqual(clock.seconds, [5, 5, 10, 10], "the raise must stick for the rest of the flow");
});

test("slow_down with no interval raises by the RFC increment, capped", async () => {
  const fake = identity({
    token: [slowDown(), slowDown(), slowDown(999), TOKENS],
  });
  const clock = recordingSleep();

  await runDeviceCodeFlow(cluster(), {
    fetch: fake.fetch,
    sleep: clock.sleep,
    now: () => NOW_MS,
  });

  assert.deepEqual(clock.seconds, [5, 10, 15, MAX_POLL_INTERVAL_SECONDS]);
});

test("an expired authorization is reported as an instruction, not as expired_token", async () => {
  const fake = identity({ token: [{ status: 400, body: { error: "expired_token" } }] });

  const err = await rejection(
    runDeviceCodeFlow(cluster(), {
      fetch: fake.fetch,
      sleep: recordingSleep().sleep,
      now: () => NOW_MS,
    }),
  );

  assert.equal(err.kind, "timeout");
  assert.doesNotMatch(err.message, /expired_token/, "the raw code is the server's vocabulary");
  assert.match(err.message, /BCDF-GHJK/, "it names the code that expired");
  assert.match(err.message, /start the sign-in again/i, "and what to do about it");
  assert.match(err.message, /10 minutes/, "and how long a fresh one lasts");
});

test("a declined approval is reported as authorizationDenied and stops polling", async () => {
  const fake = identity({ token: [PENDING, { status: 400, body: { error: "access_denied" } }] });

  const err = await rejection(
    runDeviceCodeFlow(cluster(), {
      fetch: fake.fetch,
      sleep: recordingSleep().sleep,
      now: () => NOW_MS,
    }),
  );

  assert.equal(err.kind, "authorizationDenied");
  assert.equal(fake.tokenCalls().length, 2, "no poll after a terminal answer");
});

// -----------------------------------------------------------------------------
// Cancellation
// -----------------------------------------------------------------------------

test("cancelling mid-poll stops the loop before the next request", async () => {
  const controller = new AbortController();
  const fake = identity({
    token: [PENDING, TOKENS],
    // Abort while the FIRST poll is being answered. The loop must not issue
    // the second one, even though a token reply is queued and waiting.
    onTokenCall: (index) => {
      if (index === 0) controller.abort();
    },
  });

  const err = await rejection(
    runDeviceCodeFlow(cluster(), {
      fetch: fake.fetch,
      sleep: recordingSleep().sleep,
      now: () => NOW_MS,
      signal: controller.signal,
    }),
  );

  assert.equal(err.kind, "cancelled");
  assert.equal(fake.tokenCalls().length, 1, "the queued success reply must never be fetched");
});

test("cancelling during the wait rejects at once rather than at the end of the interval", async () => {
  // The REAL sleep, and a real five-second interval. If cancellation only took
  // effect between polls this case would take five seconds; the point of the
  // abort-aware delay is that it takes none.
  const controller = new AbortController();
  const fake = identity({ token: [TOKENS] });

  const startedAt = Date.now();
  const flow = runDeviceCodeFlow(cluster(), {
    fetch: fake.fetch,
    now: () => NOW_MS,
    signal: controller.signal,
    onUserCode: () => controller.abort(),
  });

  const err = await rejection(flow);
  const elapsed = Date.now() - startedAt;

  assert.equal(err.kind, "cancelled");
  assert.ok(elapsed < 1_000, `cancellation took ${elapsed}ms; it must not wait out the interval`);
  assert.deepEqual(fake.tokenCalls(), [], "nothing was ever polled");
});

// -----------------------------------------------------------------------------
// The wire contract, and where the tokens land
// -----------------------------------------------------------------------------

test("the poll presents the RFC 8628 grant with the device code and the PKCE verifier", async () => {
  const fake = identity({ token: [TOKENS] });

  await runDeviceCodeFlow(cluster(), {
    fetch: fake.fetch,
    sleep: recordingSleep().sleep,
    now: () => NOW_MS,
  });

  const request = fake.deviceRequests()[0] ?? {};
  assert.equal(request.client_id, "mcp_existing");
  assert.equal(request.code_challenge_method, "S256");
  assert.ok(typeof request.code_challenge === "string" && request.code_challenge !== "");

  const poll = fake.tokenCalls()[0] ?? {};
  assert.equal(poll.grant_type, DEVICE_GRANT_TYPE);
  assert.equal(poll.grant_type, "urn:ietf:params:oauth:grant-type:device_code");
  assert.equal(poll.device_code, "mql_dvc_abc");
  assert.equal(poll.client_id, "mcp_existing");
  assert.ok(typeof poll.code_verifier === "string" && poll.code_verifier !== "");
});

test("a cluster with no client_id registers exactly once across the fallback", async () => {
  const fake = identity();

  const tokens = await signInWithDeviceCodeFallback(cluster({ clientId: undefined }), {
    ...loopbackDeps(fake, {
      startListener: listenerThatFails(new AuthFlowError("bindFailed", "port in use")),
    }),
    sleep: recordingSleep().sleep,
  });

  assert.equal(
    fake.urls().filter((u) => u.endsWith("/register")).length,
    1,
    "registering once per flow would orphan an OAuth client on every fallback",
  );
  assert.equal(tokens.clientId, "mcp_minted");
  assert.equal(tokens.clientIdWasRegistered, true, "so the caller persists the minted id");
});

test("device tokens go into the same store the loopback path uses", async () => {
  // The acceptance criterion is "one code path for persistence, not two": the
  // device flow returns the SAME AuthFlowTokens, so persistSignIn (memql#3404)
  // takes it unchanged.
  const fake = identity({ token: [TOKENS] });
  const tokens = await runDeviceCodeFlow(cluster(), {
    fetch: fake.fetch,
    sleep: recordingSleep().sleep,
    now: () => NOW_MS,
  });

  const secrets = new Map<string, string>();
  const store: SecretStore = {
    get: async (key) => secrets.get(key),
    store: async (key, value) => void secrets.set(key, value),
    delete: async (key) => void secrets.delete(key),
  };
  const writes: Array<Record<string, unknown>> = [];

  await persistSignIn(
    { secrets: store, writeCluster: async (update) => void writes.push(update) },
    "local",
    tokens,
  );

  assert.equal(secrets.get(refreshTokenSecretKey("local")), "REFRESH");
  assert.equal(writes[0]?.token, "ACCESS");
  assert.equal(writes[0]?.refreshToken, "", "the plaintext copy is cleared once custody is taken");
  assert.equal(writes[0]?.clientId, "mcp_existing");
  assert.equal(tokens.expiresAtEpochSeconds, Math.floor(NOW_MS / 1000) + 900);
});

test("a cluster with no identity service is misconfigured, not device-signable", async () => {
  const err = await rejection(
    runDeviceCodeFlow({ name: "bare", endpoint: "host:443" }, { fetch: identity().fetch }),
  );
  assert.equal(err.kind, "misconfigured");
});
