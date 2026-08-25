// The device-code fallback: when it fires, when it must NOT, and how it polls.
//
// The fake identity service below answers all three endpoints the two flows
// touch (/device/code, /oauth/token) and records every request, so a
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
  deviceCodeActionMessage,
  deviceCodeOpenTarget,
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
import { WELL_KNOWN_CLIENT_ID } from "../src/auth/wellKnownClient.js";

const ISSUER = "https://identity.memql.localhost";
const NOW_MS = 1_800_000_000_000;

function cluster(overrides: Partial<ClusterConfig> = {}): ClusterConfig {
  return {
    name: "local",
    endpoint: "api.memql.localhost:443",
    domain: "memql.localhost",
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

      // /register has no branch here on purpose: it is not a request this
      // extension may make any more (memql#4517). Every call is recorded above
      // regardless of which branch answers it, so the `urls()` assertions are
      // what would catch one coming back.
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
  for (const kind of ["bindFailed", "browserUnavailable"] as const) {
    assert.equal(
      shouldFallBackToDeviceCode(new AuthFlowError(kind, "x")),
      true,
      `${kind} must trigger the fallback`,
    );
  }
  for (const kind of [
    // timeout is deliberately NOT a trigger (memql#4594). Both remaining
    // triggers are knowable before or at browser-open, so no live tab can
    // exist when the device flow starts. A timeout means a browser WAS
    // opened and nothing came back -- overwhelmingly a person still mid
    // magic-link round trip, and switching flows under them closes the
    // listener their tab is about to redirect to.
    "timeout",
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
    "the fallback resolves one client_id and hands it to both grants",
  );
  assert.equal(codes[0]?.userCode, "BCDF-GHJK");
  assert.equal(codes[0]?.verificationUri, `${ISSUER}/device`);
  assert.equal(fallbacks[0]?.kind, "bindFailed", "the switch is announced, not silent");
});

test("a callback that never arrives does NOT fall back -- the browser attempt was live", async () => {
  // The 2026-08-25 field failure (memql#4594): a magic-link round trip took
  // longer than the deadline, the fallback fired under a live browser tab,
  // the tab's redirect then hit a closed port, and the person signed in a
  // second time on /device. A timeout now propagates instead; the advice
  // (signin.ts) names `MemQL: Sign In With a Device Code` for the host that truly
  // cannot receive the callback.
  const fake = identity();

  const err = await rejection(
    signInWithDeviceCodeFallback(cluster(), {
      ...loopbackDeps(fake, {
        startListener: listenerThatFails(new AuthFlowError("timeout", "nothing arrived")),
      }),
      sleep: recordingSleep().sleep,
    }),
  );

  assert.equal(err.kind, "timeout");
  assert.deepEqual(fake.deviceRequests(), [], "no device authorization may be requested");
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

test("a cluster with no client_id resolves the well-known id once across the fallback", async () => {
  const fake = identity();

  const tokens = await signInWithDeviceCodeFallback(cluster({ clientId: undefined }), {
    ...loopbackDeps(fake, {
      startListener: listenerThatFails(new AuthFlowError("bindFailed", "port in use")),
    }),
    sleep: recordingSleep().sleep,
  });

  assert.equal(
    fake.urls().filter((u) => u.endsWith("/register")).length,
    0,
    "the extension no longer registers: identity carries this client compiled in (memql#4515)",
  );
  assert.equal(tokens.clientId, WELL_KNOWN_CLIENT_ID);
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

test("cancelling during the device-authorization request never shows a code", async () => {
  // The cancel lands while POST /device/code is in flight. Without an abort
  // check between the response and onUserCode, the flow would still put the
  // code on screen -- and the UI half would open a browser tab -- for a
  // sign-in the person just cancelled; approving that page mints a session
  // nothing is polling for.
  const fake = identity();
  const aborter = new AbortController();
  const codes: DeviceAuthorization[] = [];
  const fetchThatLosesTheRace: typeof fake.fetch = async (url, init) => {
    const response = await fake.fetch(url, init);
    if (String(url).endsWith("/device/code")) aborter.abort();
    return response;
  };

  const err = await rejection(
    runDeviceCodeFlow(cluster(), {
      fetch: fetchThatLosesTheRace,
      signal: aborter.signal,
      sleep: recordingSleep().sleep,
      onUserCode: (authorization) => codes.push(authorization),
    }),
  );

  assert.equal(err.kind, "cancelled");
  assert.deepEqual(codes, [], "a cancelled sign-in must not put a code on screen");
});

// -----------------------------------------------------------------------------
// The one-notification UX (memql#4595): what the single action message says,
// and where "open" goes. The vscode-bound half (deviceCodeUi.ts) stays thin --
// one showing per flow, no re-summon after a click -- and these pure helpers
// carry everything worth asserting about it.
// -----------------------------------------------------------------------------

function authorizationFixture(overrides: Partial<DeviceAuthorization> = {}): DeviceAuthorization {
  return {
    deviceCode: "mql_dvc_x",
    userCode: "BCDF-GHJK",
    verificationUri: "https://identity.example.com/device",
    verificationUriComplete: "https://identity.example.com/device?user_code=BCDF-GHJK",
    expiresInSeconds: 600,
    intervalSeconds: 5,
    ...overrides,
  };
}

test("the open target prefers the pre-filled verification URI", () => {
  assert.equal(
    deviceCodeOpenTarget(authorizationFixture()),
    "https://identity.example.com/device?user_code=BCDF-GHJK",
  );
  assert.equal(
    deviceCodeOpenTarget(authorizationFixture({ verificationUriComplete: "" })),
    "https://identity.example.com/device",
    "a server that sent no complete URI still gets the bare page",
  );
});

// WHY THE TWO CASES BELOW COMPARE THE WHOLE SENTENCE
//
// The obvious assertion -- `message.includes("https://identity.example.com/
// device")` -- is a shape CodeQL rejects, and it is right to in general: a
// substring test whose needle is a URL is nearly always a trust decision ("is
// this link one of ours?"), and a substring can sit anywhere, so an
// attacker-controlled host may precede or follow it and still pass
// (js/incomplete-url-substring-sanitization). Rewriting it as an unanchored
// regex only trades that alert for js/regex/missing-regexp-anchor, which the
// same reasoning earns; both were raised against this file in turn.
//
// Nothing here is authorising anything -- the haystack is a notification
// sentence and the result decides nothing -- so both alerts were noise about
// test wording. But the fix belongs in the test rather than in the scanner's
// dismissal list: a dismissal lives in GitHub, teaches nobody, and the next
// person asserting on a toast writes the flagged shape again and re-argues it
// from scratch. An equality compare is CodeQL's own recommended remediation,
// trips neither query, and is the stronger assertion anyway -- a reworded
// sentence fails loudly here instead of passing on a coincidental substring.
//
// The narrower `assert.ok`s are kept beside it on purpose. They are the
// INVARIANTS, not the wording: whoever updates an expected string above must
// still leave a deliberate flow explaining no switch, and a fallback naming
// both the switch and where the full reason lives.
test("the deliberate action message carries the code and page, and no fallback talk", () => {
  const message = deviceCodeActionMessage(authorizationFixture(), "deliberate");
  assert.equal(
    message,
    "MemQL: enter code BCDF-GHJK at https://identity.example.com/device to finish signing in. " +
      "The approval page should have opened with the code pre-filled -- use the buttons if it did not.",
  );
  assert.ok(
    !message.toLowerCase().includes("browser sign-in"),
    `a deliberate device flow must not explain a switch nobody made: ${message}`,
  );
});

test("the fallback action message explains the switch in the same notification", () => {
  // ONE notification for the whole fallback (memql#4595): the explanation
  // that used to be its own toast rides the action message instead, and the
  // full reason stays in the MemQL Connection output.
  const message = deviceCodeActionMessage(authorizationFixture(), "fallback");
  assert.equal(
    message,
    "MemQL: a browser sign-in is not possible on this host (details in the MemQL Connection output). " +
      "Finish with a device code instead: enter code BCDF-GHJK at " +
      "https://identity.example.com/device to finish signing in -- on another device if this one " +
      "cannot open the page.",
  );
  assert.ok(
    message.toLowerCase().includes("browser sign-in"),
    `the switch must be explained where the code is shown: ${message}`,
  );
  assert.ok(
    message.includes("MemQL Connection"),
    `the message must say where the full reason lives: ${message}`,
  );
});
