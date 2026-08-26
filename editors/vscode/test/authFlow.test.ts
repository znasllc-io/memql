// The end-to-end browser sign-in flow: authorize -> callback ->
// exchange.
//
// The "browser" here is the injected openExternal: it receives the real
// authorization URL, reads the redirect_uri and state out of it, and issues a
// GET at the REAL loopback listener the flow just bound. Only the identity
// service is faked. That is deliberate -- the failure this flow exists to avoid
// is a callback that never lands, and a stubbed listener cannot fail that way.

import test from "node:test";
import assert from "node:assert/strict";

import type { ClusterConfig } from "../src/clusters/model.js";
import type { HttpRequestInit, HttpResponseLike } from "../src/connection/credentials.js";
import { isAuthFlowError } from "../src/auth/errors.js";
import { runAuthorizationFlow, type AuthFlowDeps } from "../src/auth/flow.js";
import { codeChallengeS256 } from "../src/auth/pkce.js";
import { WELL_KNOWN_CLIENT_ID } from "../src/auth/wellKnownClient.js";
import { OAUTH_METADATA_PATH } from "../src/auth/discovery.js";

const ISSUER = "https://identity.memql.localhost";
const NOW_MS = 1_800_000_000_000;

function cluster(overrides: Partial<ClusterConfig> = {}): ClusterConfig {
  return {
    name: "local",
    endpoint: "api.memql.localhost:443",
    domain: "memql.localhost",
    ...overrides,
  };
}

// -----------------------------------------------------------------------------
// A fake identity service: /oauth/token redeems a
// code. Every request it saw is recorded, so a test can assert what was NOT
// sent as easily as what was.
// -----------------------------------------------------------------------------

interface FakeIdentity {
  fetch: (url: string, init: HttpRequestInit) => Promise<HttpResponseLike>;
  calls: Array<{ url: string; body: Record<string, unknown> }>;
  urls(): string[];
  /** The RFC 8414 pre-flight GETs, kept apart from `urls()` (memql#4624). */
  discoveryUrls(): string[];
}

interface FakeIdentityOptions {
  tokenStatus?: number;
  tokenBody?: unknown;
  /** Override fields of the published RFC 8414 document (memql#4624). */
  metadata?: Partial<Record<string, unknown>>;
}

/** The document a MemQL cluster publishes, matching
 *  component/identity/oauth_metadata.go. */
function oauthMetadata(overrides: Partial<Record<string, unknown>> = {}): Record<string, unknown> {
  return {
    issuer: ISSUER,
    authorization_endpoint: `${ISSUER}/authorize`,
    token_endpoint: `${ISSUER}/oauth/token`,
    device_authorization_endpoint: `${ISSUER}/device/code`,
    jwks_uri: `${ISSUER}/.well-known/jwks.json`,
    response_types_supported: ["code"],
    grant_types_supported: [
      "authorization_code",
      "refresh_token",
      "urn:ietf:params:oauth:grant-type:device_code",
    ],
    code_challenge_methods_supported: ["S256"],
    token_endpoint_auth_methods_supported: ["none"],
    ...overrides,
  };
}

function identity(options: FakeIdentityOptions = {}): FakeIdentity {
  const calls: FakeIdentity["calls"] = [];
  const discovery: string[] = [];
  return {
    calls,
    discoveryUrls: () => discovery,
    urls: () => calls.map((c) => c.url),
    fetch: async (url, init) => {
      // The RFC 8414 pre-flight (memql#4624). Answered here so the fake is a
      // CLUSTER rather than a token endpoint -- the flow asks where the
      // endpoints are before it opens a browser, which is the whole point of
      // the pre-flight, and a fake that cannot answer it is not a cluster.
      if (url.endsWith(OAUTH_METADATA_PATH)) {
        // Recorded SEPARATELY from `calls` (memql#4624). `urls()` answers "what
        // credential-bearing requests did the extension make", which is what
        // the /register assertions and the client_id assertions are about.
        // Folding an unauthenticated discovery GET into it would change the
        // meaning of every one of those without changing what they were
        // written to protect.
        discovery.push(url);
        return {
          ok: true,
          status: 200,
          text: async () => JSON.stringify(oauthMetadata(options.metadata)),
        };
      }
      calls.push({ url, body: JSON.parse(init.body) as Record<string, unknown> });
      // ONE endpoint, deliberately: /register is not among the requests this
      // extension may make any more (memql#4517), so a stray call to it shows
      // up in `urls()` rather than being quietly answered.
      const status = options.tokenStatus ?? 200;
      const payload = options.tokenBody ?? {
        access_token: "ACCESS",
        refresh_token: "REFRESH",
        token_type: "Bearer",
        expires_in: 900,
      };
      return {
        ok: status >= 200 && status < 300,
        status,
        text: async () => (typeof payload === "string" ? payload : JSON.stringify(payload)),
      };
    },
  };
}

// -----------------------------------------------------------------------------
// A fake browser. It follows the authorization URL the way a real one would --
// straight at the loopback listener -- with whatever callback params the test
// wants.
// -----------------------------------------------------------------------------

interface FakeBrowser {
  resolveExternalUri: (url: string) => Promise<string>;
  // `boolean | void`, because what this stands in for -- env.openExternal --
  // resolves a boolean, and a fake that could only resolve void cannot express
  // the way most hosts actually refuse (memql#4618).
  openExternal: (url: string) => Promise<boolean | void>;
  /** Every URL handed to asExternalUri. */
  resolved: string[];
  /** Every URL actually opened -- i.e. the asExternalUri OUTPUT. */
  opened: string[];
}

type CallbackShape = (params: URLSearchParams) => Record<string, string>;

// The honest default: echo back the state that was sent, with a code.
const goodCallback: CallbackShape = (q) => ({ code: "AUTHCODE", state: q.get("state") ?? "" });

function browser(shape: CallbackShape = goodCallback, externalPrefix = ""): FakeBrowser {
  const resolved: string[] = [];
  const opened: string[] = [];
  return {
    resolved,
    opened,
    // Models what asExternalUri does on a remote host: hands back a possibly
    // DIFFERENT URL. The prefix makes it observable that the flow opened the
    // resolved value rather than the original.
    resolveExternalUri: async (url) => {
      resolved.push(url);
      return externalPrefix === "" ? url : `${externalPrefix}${encodeURIComponent(url)}`;
    },
    openExternal: async (url) => {
      opened.push(url);
      // Recover the authorization URL the tunnel wrapped, then follow it.
      const authorizeUrl = externalPrefix === "" ? url : decodeURIComponent(url.slice(externalPrefix.length));
      const query = new URL(authorizeUrl).searchParams;
      const redirectUri = query.get("redirect_uri");
      assert.ok(redirectUri !== null, "the authorization URL carried no redirect_uri");
      const callback = new URL(redirectUri);
      for (const [key, value] of Object.entries(shape(query))) {
        callback.searchParams.set(key, value);
      }
      const res = await fetch(callback.toString());
      await res.text();
    },
  };
}

function deps(net: FakeIdentity, ui: FakeBrowser, extra: Partial<AuthFlowDeps> = {}): AuthFlowDeps {
  return {
    fetch: net.fetch,
    resolveExternalUri: ui.resolveExternalUri,
    openExternal: ui.openExternal,
    now: () => NOW_MS,
    ...extra,
  };
}

// -----------------------------------------------------------------------------

test("signs in end to end: authorizes, redeems, returns the tokens", async () => {
  const net = identity();
  const ui = browser();

  const tokens = await runAuthorizationFlow(cluster(), deps(net, ui));

  assert.equal(tokens.accessToken, "ACCESS");
  assert.equal(tokens.refreshToken, "REFRESH");
  assert.equal(tokens.expiresInSeconds, 900);
  assert.equal(tokens.expiresAtEpochSeconds, Math.floor(NOW_MS / 1000) + 900);
  // No cluster clientId, so the well-known first-party id (memql#4515). The
  // exchange is the ONLY request a browser sign-in makes now: registration used
  // to come first, and it is what failed on every DCR-off cluster.
  assert.equal(tokens.clientId, WELL_KNOWN_CLIENT_ID);

  assert.deepEqual(net.urls(), [`${ISSUER}/oauth/token`]);
});

test("the authorization URL is a PKCE S256 code request against this flow's own listener", async () => {
  const net = identity();
  const ui = browser();

  await runAuthorizationFlow(cluster(), deps(net, ui));

  assert.equal(ui.resolved.length, 1);
  const url = new URL(ui.resolved[0] ?? "");
  assert.equal(url.origin + url.pathname, `${ISSUER}/authorize`);
  assert.equal(url.searchParams.get("response_type"), "code");
  assert.equal(url.searchParams.get("client_id"), WELL_KNOWN_CLIENT_ID);
  assert.equal(url.searchParams.get("code_challenge_method"), "S256");
  assert.ok((url.searchParams.get("code_challenge") ?? "").length >= 43);
  assert.ok((url.searchParams.get("state") ?? "").length > 0);

  // The redirect_uri sent to /authorize carries the EPHEMERAL PORT; only the
  // registered one is portless (RFC 8252 §7.3 reconciles the two).
  const redirect = new URL(url.searchParams.get("redirect_uri") ?? "");
  assert.equal(redirect.hostname, "127.0.0.1");
  assert.equal(redirect.pathname, "/callback");
  assert.notEqual(redirect.port, "");
});

// THE NAME OF THIS TEST WAS WRONG, and the wrongness was the whole of
// memql#4623. asExternalUri tunnels LOOPBACK authorities, and it is applied
// here to the AUTHORIZE url -- an `https://identity...` URL, which comes back
// unchanged. No tunnel is created for the callback port by this, and the
// redirect_uri never passes through it at all. So "so a remote host tunnels"
// pinned an invariant that did not hold, in the one place somebody checking
// would have looked.
//
// What it actually protects is real and worth keeping: the flow opens the URL
// asExternalUri RETURNED rather than the raw one. The remote case is covered by
// its own tests below, against the vscode:// callback that does work.
test("the URL is opened THROUGH asExternalUri rather than raw", async () => {
  const net = identity();
  const ui = browser(goodCallback, "https://tunnel.example.dev/open?target=");

  await runAuthorizationFlow(cluster(), deps(net, ui));

  assert.equal(ui.resolved.length, 1);
  assert.equal(ui.opened.length, 1);
  assert.ok(
    ui.opened[0]?.startsWith("https://tunnel.example.dev/open?target="),
    "the flow opened the raw URL instead of the one asExternalUri returned",
  );
});

test("the code_verifier redeemed matches the code_challenge that was authorized", async () => {
  const net = identity();
  const ui = browser();

  await runAuthorizationFlow(cluster(), deps(net, ui));

  const authorize = new URL(ui.resolved[0] ?? "");
  const exchange = net.calls.find((c) => c.url.endsWith("/oauth/token"));
  assert.ok(exchange !== undefined);
  assert.equal(exchange.body.grant_type, "authorization_code");
  assert.equal(exchange.body.code, "AUTHCODE");
  assert.equal(exchange.body.client_id, WELL_KNOWN_CLIENT_ID);
  assert.equal(exchange.body.redirect_uri, authorize.searchParams.get("redirect_uri"));

  const verifier = exchange.body.code_verifier;
  assert.equal(typeof verifier, "string");
  assert.equal(codeChallengeS256(verifier as string), authorize.searchParams.get("code_challenge"));
});

test("a cluster clientId OVERRIDES the well-known id", async () => {
  // The override is what keeps two cases working: an operator's own static
  // client, and an entry still carrying an id the deleted registration path
  // minted. Neither is migrated or rewritten -- the value is simply read.
  const net = identity();
  const ui = browser();

  const tokens = await runAuthorizationFlow(
    cluster({ clientId: "mcp_stored" }),
    deps(net, ui),
  );

  assert.equal(tokens.clientId, "mcp_stored");
  assert.deepEqual(net.urls(), [`${ISSUER}/oauth/token`]);
  assert.equal(new URL(ui.resolved[0] ?? "").searchParams.get("client_id"), "mcp_stored");
});

test("a state mismatch is refused and the code is NEVER exchanged", async () => {
  const net = identity();
  const ui = browser(() => ({ code: "ATTACKER_CODE", state: "not-the-state-we-sent" }));

  await assert.rejects(
    () => runAuthorizationFlow(cluster(), deps(net, ui)),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "stateMismatch");
      return true;
    },
  );

  assert.deepEqual(
    net.urls(),
    [],
    "the token endpoint was called with a code that failed the state check",
  );
});

test("a missing state is a mismatch, not a pass", async () => {
  const net = identity();
  const ui = browser(() => ({ code: "AUTHCODE" }));

  await assert.rejects(
    () => runAuthorizationFlow(cluster(), deps(net, ui)),
    (err: unknown) => isAuthFlowError(err) && err.kind === "stateMismatch",
  );
  assert.deepEqual(net.urls(), [], "a refused flow makes no request at all");
});

test("an OAuth error envelope on the callback reports authorizationDenied", async () => {
  const net = identity();
  const ui = browser((q) => ({
    error: "access_denied",
    error_description: "the user declined",
    state: q.get("state") ?? "",
  }));

  await assert.rejects(
    () => runAuthorizationFlow(cluster(), deps(net, ui)),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "authorizationDenied");
      assert.match(err.message, /access_denied/);
      assert.match(err.message, /declined/);
      return true;
    },
  );
  assert.deepEqual(net.urls(), [], "a refused flow makes no request at all");
});

test("a callback with neither code nor error reports invalidCallback", async () => {
  const net = identity();
  const ui = browser((q) => ({ state: q.get("state") ?? "" }));

  await assert.rejects(
    () => runAuthorizationFlow(cluster(), deps(net, ui)),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "invalidCallback");
      return true;
    },
  );
});

test("a callback that never arrives fails as timeout, distinguishably", async () => {
  const net = identity();
  const ui: FakeBrowser = {
    resolved: [],
    opened: [],
    resolveExternalUri: async (url) => url,
    // The person closed the tab, or never saw it.
    openExternal: async () => {},
  };

  await assert.rejects(
    () => runAuthorizationFlow(cluster(), deps(net, ui, { timeoutMs: 60 })),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "timeout");
      return true;
    },
  );
  assert.deepEqual(net.urls(), [], "a refused flow makes no request at all");
});

test("an abort during the wait fails as cancelled, not as timeout", async () => {
  const net = identity();
  const controller = new AbortController();
  const ui: FakeBrowser = {
    resolved: [],
    opened: [],
    resolveExternalUri: async (url) => url,
    openExternal: async () => {
      setTimeout(() => controller.abort(), 10);
    },
  };

  await assert.rejects(
    () =>
      runAuthorizationFlow(
        cluster(),
        deps(net, ui, { signal: controller.signal, timeoutMs: 5_000 }),
      ),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "cancelled");
      return true;
    },
  );
});

// A host that cannot open a browser is an ENVIRONMENT limitation, and the two
// ways it shows up must both be distinguishable from someone declining. The
// device-code fallback (memql#3411) triggers on the former and explicitly not
// on the latter, so a `cancelled` here would strand every headless user.

test("a host whose asExternalUri fails reports browserUnavailable, not cancelled", async () => {
  const net = identity();
  const ui: FakeBrowser = {
    resolved: [],
    opened: [],
    resolveExternalUri: async () => {
      throw new Error("no external URI resolver on this host");
    },
    openExternal: async () => {
      assert.fail("openExternal must not be reached when the URL could not be resolved");
    },
  };

  await assert.rejects(
    () => runAuthorizationFlow(cluster(), deps(net, ui, { timeoutMs: 5_000 })),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "browserUnavailable");
      assert.notEqual(err.kind, "cancelled");
      assert.match(err.message, /no external URI resolver on this host/);
      return true;
    },
  );
});

test("a host whose openExternal fails reports browserUnavailable, not cancelled", async () => {
  const net = identity();
  const ui: FakeBrowser = {
    resolved: [],
    opened: [],
    resolveExternalUri: async (url) => url,
    openExternal: async () => {
      // What a genuinely headless machine does: there is nothing to launch.
      throw new Error("spawn xdg-open ENOENT");
    },
  };

  await assert.rejects(
    () => runAuthorizationFlow(cluster(), deps(net, ui, { timeoutMs: 5_000 })),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "browserUnavailable");
      assert.notEqual(err.kind, "cancelled");
      assert.match(err.message, /ENOENT/);
      return true;
    },
  );
});

// The OTHER way a host says no (memql#4618).
//
// env.openExternal returns Thenable<boolean> and signals failure BOTH ways --
// by rejecting, and by resolving false -- and the false is the way most hosts
// actually answer. A handler for only the rejection left this flow believing a
// browser had opened: it went on to wait out the full callback deadline, which
// is precisely the headless host the device-code fallback exists to rescue,
// stranded for ten minutes and then told it had timed out. deviceCodeUi.ts
// already handles both (98002a9f); this is the sibling that was left behind.
test("an openExternal that RESOLVES false reports browserUnavailable, not a wait", async () => {
  const net = identity();
  const ui: FakeBrowser = {
    resolved: [],
    opened: [],
    resolveExternalUri: async (url) => url,
    // No throw. Just "no" -- which is what a host with nothing to launch
    // returns, and what the old code read as success.
    openExternal: async () => false,
  };

  const started = Date.now();
  await assert.rejects(
    () => runAuthorizationFlow(cluster(), deps(net, ui, { timeoutMs: 30_000 })),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "browserUnavailable");
      // NOT `cancelled`: nobody declined anything, and the fallback triggers on
      // one of those kinds and deliberately not on the other.
      assert.notEqual(err.kind, "cancelled");
      return true;
    },
  );
  assert.ok(
    Date.now() - started < 2_000,
    "the flow sat on a callback that can never arrive instead of reporting the refusal",
  );
  assert.equal(net.calls.length, 0, "nothing was redeemed for a sign-in that never opened");
});

test("only an explicit false is a refusal: true, and void, are browsers that opened", async () => {
  // VS Code's own opener resolves true; every other binding in this tree
  // resolves undefined. Treating either as a refusal would divert a working
  // browser sign-in to a device code, which is the same defect pointing the
  // other way.
  const voidNet = identity();
  const voidUi = browser();
  assert.equal((await runAuthorizationFlow(cluster(), deps(voidNet, voidUi))).accessToken, "ACCESS");

  const trueNet = identity();
  const trueUi = browser();
  const follow = trueUi.openExternal;
  const trueTokens = await runAuthorizationFlow(
    cluster(),
    deps(trueNet, {
      ...trueUi,
      openExternal: async (url) => {
        await follow(url);
        return true;
      },
    }),
  );
  assert.equal(trueTokens.accessToken, "ACCESS");
});

test("browserUnavailable fails fast rather than sitting out the callback deadline", async () => {
  const net = identity();
  const ui: FakeBrowser = {
    resolved: [],
    opened: [],
    resolveExternalUri: async (url) => url,
    openExternal: async () => {
      throw new Error("spawn xdg-open ENOENT");
    },
  };

  // The deadline is 30s; a flow that waited it out instead of surfacing the
  // open failure would make the fallback unusable in practice.
  const started = Date.now();
  await assert.rejects(() => runAuthorizationFlow(cluster(), deps(net, ui, { timeoutMs: 30_000 })));
  assert.ok(Date.now() - started < 2_000, "the flow waited for a callback that can never arrive");
});

// memql#4619: an unreachable identity service is the OTHER half of a failed
// exchange, and until this test existed nothing in src/auth/ was exercised
// against a throwing fetch at all. undici's TypeError carries the real reason
// in `.cause`, so the assertion is that the reason reaches the sentence an
// operator reads -- not merely that the flow rejected.
//
// THE KIND CHANGED IN memql#4624, deliberately. The pre-flight now runs before
// the browser opens, so an unreachable host is caught there rather than at the
// token exchange -- which is the improvement, not a regression: the user
// previously waited out a 600-second callback deadline first. `registrationFailed`
// is the kind deviceCode.ts already uses for the same situation (a network
// failure before any credential exists, retryable the moment the server is
// willing). What this test was WRITTEN to protect is unchanged and still
// asserted: the cause in `.cause` reaches the sentence.
test("a transport failure names the cause, not just \"fetch failed\"", async () => {
  const ui = browser();
  const net: FakeIdentity = {
    calls: [],
    urls: () => [],
    discoveryUrls: () => [],
    fetch: async () => {
      const cause = new Error("getaddrinfo ENOTFOUND identity.memql.localhost");
      (cause as { code?: string }).code = "ENOTFOUND";
      const wrapper = new TypeError("fetch failed");
      (wrapper as { cause?: unknown }).cause = cause;
      throw wrapper;
    },
  };

  await assert.rejects(
    () => runAuthorizationFlow(cluster(), deps(net, ui)),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "registrationFailed");
      assert.match(err.message, /ENOTFOUND/);
      assert.match(err.message, /identity\.memql\.localhost/);
      return true;
    },
  );
});

test("a refused token exchange reports exchangeRejected with the server's sentence", async () => {
  const net = identity({
    tokenStatus: 400,
    tokenBody: {
      error: "invalid_grant",
      message: "auth code has expired",
      errorId: "ERR-1845ef",
    },
  });
  const ui = browser();

  await assert.rejects(
    () => runAuthorizationFlow(cluster(), deps(net, ui)),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "exchangeRejected");
      assert.match(err.message, /400/);
      assert.match(err.message, /invalid_grant/);
      assert.match(err.message, /ERR-1845ef/);
      return true;
    },
  );
});

test("a 200 carrying no access_token is a failure, not an empty success", async () => {
  const net = identity({ tokenBody: { token_type: "Bearer", expires_in: 900 } });
  const ui = browser();

  await assert.rejects(
    () => runAuthorizationFlow(cluster(), deps(net, ui)),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "exchangeRejected");
      assert.match(err.message, /no access_token/);
      return true;
    },
  );
});

test("a cluster naming no identity service is misconfigured, and nothing is attempted", async () => {
  const net = identity();
  const ui = browser();

  await assert.rejects(
    () => runAuthorizationFlow({ name: "bare", endpoint: "10.0.0.5:50051" }, deps(net, ui)),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "misconfigured");
      assert.match(err.message, /issuer/);
      return true;
    },
  );
  assert.equal(net.calls.length, 0);
  assert.equal(ui.opened.length, 0);
});

test("an explicit issuer wins over the domain convention", async () => {
  const net = identity();
  const ui = browser();

  await runAuthorizationFlow(
    cluster({ issuer: "https://auth.example.com/" }),
    deps(net, ui),
  );

  assert.deepEqual(net.urls(), ["https://auth.example.com/oauth/token"]);
});

test("a server that reports no expires_in leaves the expiry unknown rather than expired", async () => {
  const net = identity({
    tokenBody: { access_token: "ACCESS", refresh_token: "REFRESH", token_type: "Bearer" },
  });
  const ui = browser();

  const tokens = await runAuthorizationFlow(cluster(), deps(net, ui));
  assert.equal(tokens.expiresInSeconds, 0);
  assert.equal(tokens.expiresAtEpochSeconds, 0);
});
