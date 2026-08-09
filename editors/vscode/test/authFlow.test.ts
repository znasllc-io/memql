// The end-to-end browser sign-in flow: register -> authorize -> callback ->
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

const ISSUER = "https://identity.local.znas.io";
const NOW_MS = 1_800_000_000_000;

function cluster(overrides: Partial<ClusterConfig> = {}): ClusterConfig {
  return {
    name: "local",
    endpoint: "cockpit.local.znas.io:443",
    domain: "local.znas.io",
    ...overrides,
  };
}

// -----------------------------------------------------------------------------
// A fake identity service: /register mints a client_id, /oauth/token redeems a
// code. Every request it saw is recorded, so a test can assert what was NOT
// sent as easily as what was.
// -----------------------------------------------------------------------------

interface FakeIdentity {
  fetch: (url: string, init: HttpRequestInit) => Promise<HttpResponseLike>;
  calls: Array<{ url: string; body: Record<string, unknown> }>;
  urls(): string[];
}

interface FakeIdentityOptions {
  registerStatus?: number;
  registerBody?: unknown;
  tokenStatus?: number;
  tokenBody?: unknown;
}

function identity(options: FakeIdentityOptions = {}): FakeIdentity {
  const calls: FakeIdentity["calls"] = [];
  return {
    calls,
    urls: () => calls.map((c) => c.url),
    fetch: async (url, init) => {
      calls.push({ url, body: JSON.parse(init.body) as Record<string, unknown> });
      const isRegister = url.endsWith("/register");
      const status = isRegister ? (options.registerStatus ?? 201) : (options.tokenStatus ?? 200);
      const payload = isRegister
        ? (options.registerBody ?? { client_id: "mcp_minted" })
        : (options.tokenBody ?? {
            access_token: "ACCESS",
            refresh_token: "REFRESH",
            token_type: "Bearer",
            expires_in: 900,
          });
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
  openExternal: (url: string) => Promise<void>;
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

test("signs in end to end: registers, authorizes, redeems, returns the tokens", async () => {
  const net = identity();
  const ui = browser();

  const tokens = await runAuthorizationFlow(cluster(), deps(net, ui));

  assert.equal(tokens.accessToken, "ACCESS");
  assert.equal(tokens.refreshToken, "REFRESH");
  assert.equal(tokens.expiresInSeconds, 900);
  assert.equal(tokens.expiresAtEpochSeconds, Math.floor(NOW_MS / 1000) + 900);
  assert.equal(tokens.clientId, "mcp_minted");
  assert.equal(tokens.clientIdWasRegistered, true, "a freshly minted id must be flagged for persistence");

  assert.deepEqual(net.urls(), [`${ISSUER}/register`, `${ISSUER}/oauth/token`]);
});

test("the authorization URL is a PKCE S256 code request against this flow's own listener", async () => {
  const net = identity();
  const ui = browser();

  await runAuthorizationFlow(cluster(), deps(net, ui));

  assert.equal(ui.resolved.length, 1);
  const url = new URL(ui.resolved[0] ?? "");
  assert.equal(url.origin + url.pathname, `${ISSUER}/authorize`);
  assert.equal(url.searchParams.get("response_type"), "code");
  assert.equal(url.searchParams.get("client_id"), "mcp_minted");
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

test("the URL is opened THROUGH asExternalUri, so a remote host tunnels", async () => {
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
  assert.equal(exchange.body.client_id, "mcp_minted");
  assert.equal(exchange.body.redirect_uri, authorize.searchParams.get("redirect_uri"));

  const verifier = exchange.body.code_verifier;
  assert.equal(typeof verifier, "string");
  assert.equal(codeChallengeS256(verifier as string), authorize.searchParams.get("code_challenge"));
});

test("registration is SKIPPED when the cluster already carries a client_id", async () => {
  const net = identity();
  const ui = browser();

  const tokens = await runAuthorizationFlow(
    cluster({ clientId: "mcp_stored" }),
    deps(net, ui),
  );

  assert.equal(tokens.clientId, "mcp_stored");
  assert.equal(tokens.clientIdWasRegistered, false);
  assert.deepEqual(net.urls(), [`${ISSUER}/oauth/token`], "no /register call may be made");
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
    [`${ISSUER}/register`],
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
  assert.deepEqual(net.urls(), [`${ISSUER}/register`]);
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
  assert.deepEqual(net.urls(), [`${ISSUER}/register`]);
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
  assert.deepEqual(net.urls(), [`${ISSUER}/register`]);
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

  assert.deepEqual(net.urls(), [
    "https://auth.example.com/register",
    "https://auth.example.com/oauth/token",
  ]);
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
