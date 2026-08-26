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
  /**
   * Every request EXCEPT the RFC 8414 pre-flight (memql#4624).
   *
   * The pre-flight is a real request and `urls()` shows it. These assertions
   * are about which OAuth ENDPOINTS the flow drove -- "it redeemed at
   * /oauth/token", "it never called /register" -- and threading a discovery
   * URL through every one of them would bury the claim each is making.
   * `discoveryUrls()` asserts the pre-flight itself, in its own tests.
   */
  oauthUrls(): string[];
  discoveryUrls(): string[];
}

interface FakeIdentityOptions {
  tokenStatus?: number;
  tokenBody?: unknown;
  /**
   * The RFC 8414 document this cluster publishes (memql#4624). `undefined`
   * serves the ordinary one for `issuer`; `null` serves a 404, which is what a
   * host that is not a MemQL identity service looks like.
   */
  metadata?: Record<string, unknown> | null;
  /** The issuer the served metadata names. Defaults to the fetched host. */
  issuer?: string;
}

function identity(options: FakeIdentityOptions = {}): FakeIdentity {
  const calls: FakeIdentity["calls"] = [];
  return {
    calls,
    urls: () => calls.map((c) => c.url),
    oauthUrls: () =>
      calls
        .map((c) => c.url)
        .filter((u) => !u.endsWith("/.well-known/oauth-authorization-server")),
    discoveryUrls: () =>
      calls
        .map((c) => c.url)
        .filter((u) => u.endsWith("/.well-known/oauth-authorization-server")),
    fetch: async (url, init) => {
      calls.push({ url, body: JSON.parse(init.body ?? "{}") as Record<string, unknown> });

      // THE PRE-FLIGHT (memql#4624). Sign-in now asks the cluster where its
      // endpoints are before it opens a browser, so the fake has to be able to
      // answer -- and being able to answer WRONGLY is what lets the pre-flight
      // itself be tested.
      if (url.endsWith("/.well-known/oauth-authorization-server")) {
        if (options.metadata === null) {
          return { ok: false, status: 404, text: async () => "not found" };
        }
        const base = url.slice(0, -"/.well-known/oauth-authorization-server".length);
        const doc = options.metadata ?? {
          issuer: options.issuer ?? base,
          authorization_endpoint: `${base}/authorize`,
          token_endpoint: `${base}/oauth/token`,
          device_authorization_endpoint: `${base}/device/code`,
          jwks_uri: `${base}/.well-known/jwks.json`,
        };
        return { ok: true, status: 200, text: async () => JSON.stringify(doc) };
      }

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

  assert.deepEqual(net.oauthUrls(), [`${ISSUER}/oauth/token`]);
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

// NOT "so a remote host tunnels" (memql#4623). That is what this test claimed,
// and it was the wrong invariant: asExternalUri is applied to the AUTHORIZE
// URL, which is an `https://identity...` URL and comes back unchanged, so no
// tunnel is ever created for the loopback CALLBACK port. What the call
// genuinely buys is that whatever the host hands back is what gets opened,
// which is what a local host with a rewriting URI resolver needs. A remote
// host is refused before it reaches here, in its own test below.
test("the URL is opened THROUGH asExternalUri, so a rewriting host is honoured", async () => {
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
  assert.deepEqual(net.oauthUrls(), [`${ISSUER}/oauth/token`]);
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
    net.oauthUrls(),
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
  assert.deepEqual(net.oauthUrls(), [], "a refused flow makes no OAuth request at all");
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
  assert.deepEqual(net.oauthUrls(), [], "a refused flow makes no OAuth request at all");
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
  assert.deepEqual(net.oauthUrls(), [], "a refused flow makes no OAuth request at all");
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
  assert.deepEqual(
    net.oauthUrls(),
    [],
    "nothing was redeemed for a sign-in that never opened",
  );
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
test("a transport failure names the cause, not just \"fetch failed\"", async () => {
  const ui = browser();
  // Discovery succeeds and the EXCHANGE throws, so this stays a test about the
  // exchange (memql#4624 put a pre-flight in front of it; a fake that throws on
  // everything would now be exercising that instead, and the pre-flight has its
  // own tests below).
  const net: FakeIdentity = {
    calls: [],
    urls: () => [],
    oauthUrls: () => [],
    discoveryUrls: () => [],
    fetch: async (url: string) => {
      if (url.endsWith("/.well-known/oauth-authorization-server")) {
        const base = url.slice(0, -"/.well-known/oauth-authorization-server".length);
        return {
          ok: true,
          status: 200,
          text: async () =>
            JSON.stringify({
              issuer: base,
              authorization_endpoint: `${base}/authorize`,
              token_endpoint: `${base}/oauth/token`,
            }),
        };
      }
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
      assert.equal(err.kind, "exchangeRejected");
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

  assert.deepEqual(net.oauthUrls(), ["https://auth.example.com/oauth/token"]);
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

// -----------------------------------------------------------------------------
// The pre-flight (memql#4624)
// -----------------------------------------------------------------------------
//
// THE DEFECT THESE PIN. runAuthorizationFlow opened a browser and parked for
// 600 seconds without ever asking whether the issuer existed. A wrong domain,
// an unreachable host, a bad certificate, an old cluster and a plain non-MemQL
// host all cost the full deadline and were then blamed on the browser:
// "the browser page was never completed, or it could not reach 127.0.0.1" --
// wrong in every one of them. The device path has always failed in one round
// trip with the real reason.

test("sign-in asks the cluster where its endpoints are before opening a browser", async () => {
  const net = identity();
  const ui = browser();

  await runAuthorizationFlow(cluster(), deps(net, ui));

  assert.deepEqual(
    net.discoveryUrls(),
    [`${ISSUER}/.well-known/oauth-authorization-server`],
    "no pre-flight ran, so every failure below still costs the full sign-in deadline",
  );
  assert.equal(
    net.calls[0]?.url,
    `${ISSUER}/.well-known/oauth-authorization-server`,
    "the pre-flight must be FIRST -- after the browser is open it has saved nothing",
  );
});

test("an unreachable issuer fails immediately with the real reason", async () => {
  const started = Date.now();
  const net: FakeIdentity = {
    calls: [],
    urls: () => [],
    oauthUrls: () => [],
    discoveryUrls: () => [],
    fetch: async () => {
      const wrapper = new TypeError("fetch failed");
      (wrapper as { cause?: unknown }).cause = new Error("getaddrinfo ENOTFOUND identity.memql.localhost");
      throw wrapper;
    },
  };
  const ui = browser();

  await assert.rejects(
    () => runAuthorizationFlow(cluster(), deps(net, ui)),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.match(
        err.message,
        /ENOTFOUND/,
        "the failure does not name what actually went wrong, which is the whole complaint",
      );
      return true;
    },
  );
  assert.ok(Date.now() - started < 2_000, "the flow waited out a deadline instead of failing fast");
  assert.deepEqual(ui.opened, [], "a browser was opened at a host that cannot answer");
});

// THE DEGRADE, and it matters as much as the fail-fast. A host that answers
// WITHOUT an RFC 8414 document is either the wrong host or a cluster old
// enough to predate the document, and the extension cannot tell them apart.
// Refusing would make it unable to sign in to a cluster it can sign in to
// today -- a regression traded for a diagnosis.
test("a cluster that publishes no metadata still signs in, on the convention", async () => {
  const net = identity({ metadata: null });
  const ui = browser();

  const tokens = await runAuthorizationFlow(cluster(), deps(net, ui));

  assert.equal(tokens.accessToken, "ACCESS");
  assert.deepEqual(
    net.oauthUrls(),
    [`${ISSUER}/oauth/token`],
    "a cluster with no discovery document became unreachable, which is a regression",
  );
});

// The payoff: an identity service that is not at `identity.<domain>` and a
// cluster whose paths are not the conventional ones both work, because the
// document is believed.
test("the authorize and token URLs come from the published document", async () => {
  const net = identity({
    metadata: {
      issuer: ISSUER,
      authorization_endpoint: `${ISSUER}/oauth2/v1/auth`,
      token_endpoint: `${ISSUER}/oauth2/v1/token`,
    },
  });
  const ui = browser();

  await runAuthorizationFlow(cluster(), deps(net, ui));

  assert.equal(
    new URL(ui.opened[0] ?? "https://x.invalid/").pathname,
    "/oauth2/v1/auth",
    "the published authorization endpoint was overwritten by the convention",
  );
  assert.deepEqual(net.oauthUrls(), [`${ISSUER}/oauth2/v1/token`]);
});

// -----------------------------------------------------------------------------
// A remote extension host cannot receive this callback (memql#4623)
// -----------------------------------------------------------------------------
//
// THE DEFECT THESE PIN. loopback.ts binds 127.0.0.1 ON THE EXTENSION HOST -- the
// remote machine -- and that port goes into the redirect URI. The browser opens
// on the user's OWN machine and redirects to THEIR 127.0.0.1:PORT, where nothing
// is listening. Neither fallback trigger fired, because the bind succeeded and
// openExternal succeeded, so the result was a 600-second spinner and then advice
// to run a palette command. `README.md` and this module's own header both said
// it "falls back automatically"; neither `env.remoteName` nor `env.uiKind`
// appeared anywhere in `src/`.

test("a remote extension host is refused before anything binds", async () => {
  const net = identity();
  const ui = browser();
  let bound = false;

  await assert.rejects(
    () =>
      runAuthorizationFlow(cluster(), {
        ...deps(net, ui),
        remoteName: "ssh-remote",
        startListener: async () => {
          bound = true;
          throw new Error("must not be reached");
        },
      }),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(
        err.kind,
        "browserUnavailable",
        "the refusal must carry the kind that routes to the device-code flow, or a remote " +
          "user is refused instead of being signed in",
      );
      assert.match(err.message, /ssh-remote/, "the message does not say what is different here");
      return true;
    },
  );

  assert.equal(bound, false, "a port was bound on the remote host for a callback that cannot arrive");
  assert.deepEqual(ui.opened, [], "a browser was opened for a sign-in that cannot complete");
  assert.deepEqual(net.oauthUrls(), [], "an OAuth request was made for a sign-in that cannot complete");
});

// The other direction: a local host must be unaffected. `remoteName` is
// undefined locally, and every existing caller supplies nothing.
test("a local host is not refused", async () => {
  const net = identity();
  const ui = browser();
  const tokens = await runAuthorizationFlow(cluster(), { ...deps(net, ui), remoteName: undefined });
  assert.equal(tokens.accessToken, "ACCESS");
});

// An empty string is what a host that reports "no remote" as "" would give, and
// it must read as local rather than as a remote named nothing.
test("an empty remoteName is local", async () => {
  const net = identity();
  const ui = browser();
  const tokens = await runAuthorizationFlow(cluster(), { ...deps(net, ui), remoteName: "  " });
  assert.equal(tokens.accessToken, "ACCESS");
});
