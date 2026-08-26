// The well-known first-party client, and the property the whole epic exists
// for: NEITHER FLOW EVER CALLS /register.
//
// The defect (memql#4514): this extension obtained its client_id by RFC 7591
// dynamic client registration, and DCR is OFF by default (memql#3719). So on a
// cluster in its default posture the browser flow ended at
// `.../register returned 403: registration_disabled` and the device fallback
// ended at /device/code refusing an unregistered client. Nothing caught it,
// because nothing exercised a FIRST sign-in against a cluster in that posture.
//
// The zero-/register assertions below count requests at the stub rather than
// inferring absence from success -- a flow that succeeded for some other reason
// would otherwise look like a pass.
//
// Two properties of the redirect URI are load-bearing and neither is visible at
// a glance: it is PORTLESS (identity's RFC 8252 §7.3 matcher grants the
// any-port exception only to a registered loopback URI carrying no port, so
// pinning one would make every callback fail validation), and it must equal
// identity's own registered value byte for byte.

import test from "node:test";
import assert from "node:assert/strict";

import type { HttpRequestInit, HttpResponseLike } from "../src/connection/credentials.js";
import { runAuthorizationFlow } from "../src/auth/flow.js";
import { runDeviceCodeFlow } from "../src/auth/deviceCode.js";
import {
  WELL_KNOWN_CLIENT_ID,
  WELL_KNOWN_REDIRECT_URI,
  normalizeIssuer,
  resolveClientId,
} from "../src/auth/wellKnownClient.js";

const ISSUER = "https://identity.memql.localhost";
const CLUSTER = { name: "prod", endpoint: "https://api.memql.localhost", issuer: ISSUER } as const;

// -----------------------------------------------------------------------------
// The constants
// -----------------------------------------------------------------------------

test("the well-known client id matches identity's built-in registry", () => {
  // Must equal identity.BuiltinClientVSCode (component/identity/builtin_clients.go).
  // It is a wire contract between a released extension and a released cluster.
  assert.equal(WELL_KNOWN_CLIENT_ID, "memql-vscode");
});

test("the well-known redirect URI carries no port", () => {
  assert.equal(WELL_KNOWN_REDIRECT_URI, "http://127.0.0.1/callback");
  const parsed = new URL(WELL_KNOWN_REDIRECT_URI);
  assert.equal(parsed.port, "", "a port would opt the URI out of RFC 8252's any-port exception");
  assert.equal(parsed.hostname, "127.0.0.1");
  assert.equal(parsed.pathname, "/callback");
});

test("resolveClientId falls back to the well-known id, and honours an override", () => {
  assert.equal(resolveClientId(undefined), WELL_KNOWN_CLIENT_ID);
  assert.equal(resolveClientId(""), WELL_KNOWN_CLIENT_ID);
  assert.equal(resolveClientId("   "), WELL_KNOWN_CLIENT_ID, "whitespace is not a client_id");
  // An operator's custom static client, or an id minted by the old
  // registration path -- both are just override values now, so a cluster entry
  // carrying either keeps working with nothing migrated.
  assert.equal(resolveClientId("mcp_already_registered"), "mcp_already_registered");
  assert.equal(resolveClientId("  padded  "), "padded");
});

test("normalizeIssuer trims whitespace and trailing slashes", () => {
  assert.equal(normalizeIssuer(" https://identity.example.test/// "), "https://identity.example.test");
  assert.equal(normalizeIssuer(undefined), "");
});

// -----------------------------------------------------------------------------
// Zero /register, both flows
// -----------------------------------------------------------------------------

interface RequestLog {
  fetch: (url: string, init: HttpRequestInit) => Promise<HttpResponseLike>;
  urls: string[];
  registerCalls: () => number;
}

/** recorder answers the token / device endpoints and counts every URL it sees. */
function recorder(): RequestLog {
  const urls: string[] = [];
  return {
    urls,
    registerCalls: () => urls.filter((u) => new URL(u).pathname === "/register").length,
    fetch: async (url) => {
      urls.push(url);
      const path = new URL(url).pathname;
      // The RFC 8414 pre-flight (memql#4624).
      if (path === "/.well-known/oauth-authorization-server") {
        const metaBase = url.slice(0, -path.length);
        return json(200, {
          issuer: metaBase,
          authorization_endpoint: `${metaBase}/authorize`,
          token_endpoint: `${metaBase}/oauth/token`,
          device_authorization_endpoint: `${metaBase}/device/code`,
        });
      }
      if (path === "/device/code") {
        return json(200, {
          device_code: "dc-1",
          user_code: "ABCD-2345",
          verification_uri: `${ISSUER}/device`,
          verification_uri_complete: `${ISSUER}/device?user_code=ABCD-2345`,
          expires_in: 600,
          interval: 0,
        });
      }
      if (path === "/oauth/token") {
        return json(200, {
          access_token: "at-1",
          refresh_token: "rt-1",
          expires_in: 3600,
          token_type: "Bearer",
        });
      }
      // Anything else -- /register above all -- is a request this extension
      // must no longer make.
      return json(404, { error: "not_found" });
    },
  };
}

function json(status: number, payload: unknown): HttpResponseLike {
  return {
    ok: status >= 200 && status < 300,
    status,
    text: async () => JSON.stringify(payload),
  };
}

test("the browser flow authorizes as the well-known client and never calls /register", async () => {
  const net = recorder();
  let authorizeUrl = "";

  const tokens = await runAuthorizationFlow(CLUSTER, {
    fetch: net.fetch,
    resolveExternalUri: (url) => url,
    openExternal: (url) => {
      authorizeUrl = url;
    },
    startListener: async () => ({
      host: "127.0.0.1",
      port: 54321,
      redirectUri: "http://127.0.0.1:54321/callback",
      waitForCallback: async () => ({
        code: "code-1",
        state: new URL(authorizeUrl).searchParams.get("state") ?? "",
      }),
      close: () => {},
    }),
  });

  assert.equal(net.registerCalls(), 0, "the browser flow must make zero /register requests");
  assert.equal(tokens.clientId, WELL_KNOWN_CLIENT_ID);
  assert.equal(tokens.accessToken, "at-1");
  assert.equal(tokens.refreshToken, "rt-1");

  // The id reaches /authorize and the exchange, not just the return value.
  assert.equal(new URL(authorizeUrl).searchParams.get("client_id"), WELL_KNOWN_CLIENT_ID);
  assert.deepEqual(
    net.urls
      .map((u) => new URL(u).pathname)
      // The RFC 8414 pre-flight is not an OAuth request and carries no
      // credential (memql#4624); the claim here is about which OAuth
      // endpoints a browser sign-in drives, and /register is the one that
      // must not appear.
      .filter((p) => p !== "/.well-known/oauth-authorization-server"),
    ["/oauth/token"],
    "the only OAuth request a browser sign-in makes is the code exchange",
  );
});

test("the device flow authorizes as the well-known client and never calls /register", async () => {
  const net = recorder();

  const tokens = await runDeviceCodeFlow(CLUSTER, {
    fetch: net.fetch,
    sleep: async () => {},
  });

  assert.equal(net.registerCalls(), 0, "the device flow must make zero /register requests");
  assert.equal(tokens.clientId, WELL_KNOWN_CLIENT_ID);
  assert.equal(tokens.accessToken, "at-1");
  assert.deepEqual(
    net.urls.map((u) => new URL(u).pathname),
    ["/device/code", "/oauth/token"],
    "a device sign-in requests an authorization and then polls -- nothing else",
  );
});

test("an operator's clientId override reaches both flows", async () => {
  const overridden = { ...CLUSTER, clientId: "our-static-client" };

  const browserNet = recorder();
  let authorizeUrl = "";
  const browser = await runAuthorizationFlow(overridden, {
    fetch: browserNet.fetch,
    resolveExternalUri: (url) => url,
    openExternal: (url) => {
      authorizeUrl = url;
    },
    startListener: async () => ({
      host: "127.0.0.1",
      port: 54321,
      redirectUri: "http://127.0.0.1:54321/callback",
      waitForCallback: async () => ({
        code: "code-1",
        state: new URL(authorizeUrl).searchParams.get("state") ?? "",
      }),
      close: () => {},
    }),
  });
  assert.equal(browser.clientId, "our-static-client");
  assert.equal(new URL(authorizeUrl).searchParams.get("client_id"), "our-static-client");
  assert.equal(browserNet.registerCalls(), 0);

  const deviceNet = recorder();
  const device = await runDeviceCodeFlow(overridden, {
    fetch: deviceNet.fetch,
    sleep: async () => {},
  });
  assert.equal(device.clientId, "our-static-client");
  assert.equal(deviceNet.registerCalls(), 0);
});

test("a role-floor refusal surfaces the server's sentence verbatim", async () => {
  // identity refuses a below-floor sign-in through the standard OAuth error
  // redirect (memql#4516) precisely so the editor can print what it says. A
  // flow that summarised it -- or rendered its own copy -- would tell the
  // person less than the server already told it.
  const net = recorder();
  const description =
    "MemQL for VS Code manages this cluster. Your role on it is reader, and signing in " +
    "from an editor needs developer or above. Ask a cluster owner or admin to raise your role.";

  await assert.rejects(
    () =>
      runAuthorizationFlow(CLUSTER, {
        fetch: net.fetch,
        resolveExternalUri: (url) => url,
        openExternal: () => {},
        startListener: async () => ({
          host: "127.0.0.1",
          port: 54321,
          redirectUri: "http://127.0.0.1:54321/callback",
          waitForCallback: async () => ({
            error: "access_denied",
            errorDescription: description,
          }),
          close: () => {},
        }),
      }),
    (err: unknown) => {
      const e = err as { kind?: string; message?: string };
      assert.equal(e.kind, "authorizationDenied");
      assert.ok(
        (e.message ?? "").includes(description),
        `the server's sentence must survive intact, got: ${e.message}`,
      );
      return true;
    },
  );
  assert.equal(net.registerCalls(), 0);
});
