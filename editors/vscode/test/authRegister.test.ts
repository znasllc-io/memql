// RFC 7591 dynamic client registration.
//
// Two properties here are load-bearing and neither is visible at a glance:
//
//   The registered redirect URI is PORTLESS. identity's RFC 8252 §7.3 matcher
//   only grants the any-port exception to a registered loopback URI that
//   carries NO port (component/identity/config.go, matchesLoopbackAnyPort --
//   `r.Port() != ""` opts back into exact-match), so pinning a port here would
//   make every callback fail validation.
//
//   Registration runs at most once per cluster. Each POST mints a client_id and
//   persists a row, so registering on every sign-in would leave orphaned
//   clients nothing revokes.

import test from "node:test";
import assert from "node:assert/strict";

import type { HttpRequestInit, HttpResponseLike } from "../src/connection/credentials.js";
import { isAuthFlowError } from "../src/auth/errors.js";
import {
  DEFAULT_CLIENT_NAME,
  REGISTERED_REDIRECT_URI,
  ensureClientId,
  registerPublicClient,
} from "../src/auth/register.js";

const ISSUER = "https://identity.local.znas.io";

interface FakeHttp {
  fetch: (url: string, init: HttpRequestInit) => Promise<HttpResponseLike>;
  calls: Array<{ url: string; body: Record<string, unknown> }>;
}

function http(status: number, payload: unknown): FakeHttp {
  const calls: FakeHttp["calls"] = [];
  return {
    calls,
    fetch: async (url, init) => {
      calls.push({ url, body: JSON.parse(init.body) as Record<string, unknown> });
      return {
        ok: status >= 200 && status < 300,
        status,
        text: async () => (typeof payload === "string" ? payload : JSON.stringify(payload)),
      };
    },
  };
}

test("registers as a PUBLIC client at the portless loopback redirect URI", async () => {
  const net = http(201, { client_id: "mcp_abc123" });

  const clientId = await registerPublicClient({ issuer: ISSUER, fetch: net.fetch });

  assert.equal(clientId, "mcp_abc123");
  assert.equal(net.calls.length, 1);
  assert.equal(net.calls[0]?.url, `${ISSUER}/register`);

  const body = net.calls[0]?.body ?? {};
  assert.deepEqual(body.redirect_uris, ["http://127.0.0.1/callback"]);
  assert.equal(body.token_endpoint_auth_method, "none");
  assert.deepEqual(body.grant_types, ["authorization_code", "refresh_token"]);
  assert.deepEqual(body.response_types, ["code"]);
  assert.equal(body.client_name, DEFAULT_CLIENT_NAME);
});

test("the registered redirect URI carries no port", () => {
  assert.equal(REGISTERED_REDIRECT_URI, "http://127.0.0.1/callback");
  const parsed = new URL(REGISTERED_REDIRECT_URI);
  assert.equal(parsed.port, "", "a port would opt the URI out of RFC 8252's any-port exception");
  assert.equal(parsed.hostname, "127.0.0.1");
  assert.equal(parsed.pathname, "/callback");
});

test("registration is SKIPPED when the cluster already has a client_id", async () => {
  const net = http(201, { client_id: "mcp_should_not_be_minted" });

  const result = await ensureClientId({
    issuer: ISSUER,
    clientId: "mcp_already_registered",
    fetch: net.fetch,
  });

  assert.deepEqual(result, { clientId: "mcp_already_registered", registered: false });
  assert.equal(net.calls.length, 0, "an existing client_id must cost zero network calls");
});

test("a whitespace-only client_id is not a client_id", async () => {
  const net = http(201, { client_id: "mcp_fresh" });
  const result = await ensureClientId({ issuer: ISSUER, clientId: "   ", fetch: net.fetch });
  assert.deepEqual(result, { clientId: "mcp_fresh", registered: true });
  assert.equal(net.calls.length, 1);
});

test("a custom client name reaches the consent screen", async () => {
  const net = http(201, { client_id: "mcp_x" });
  await registerPublicClient({ issuer: ISSUER, clientName: "memQL (dev build)", fetch: net.fetch });
  assert.equal(net.calls[0]?.body.client_name, "memQL (dev build)");
});

test("a refused registration reports registrationFailed with the server's sentence", async () => {
  const net = http(403, {
    error: "registration_disabled",
    message: "dynamic client registration is disabled on this server",
  });

  await assert.rejects(
    () => registerPublicClient({ issuer: ISSUER, fetch: net.fetch }),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "registrationFailed");
      assert.match(err.message, /403/);
      assert.match(err.message, /registration_disabled/);
      assert.match(err.message, /disabled on this server/);
      return true;
    },
  );
});

test("an unreachable identity service reports registrationFailed, not a raw network error", async () => {
  await assert.rejects(
    () =>
      registerPublicClient({
        issuer: ISSUER,
        fetch: async () => {
          throw new Error("ECONNREFUSED");
        },
      }),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "registrationFailed");
      assert.match(err.message, /ECONNREFUSED/);
      return true;
    },
  );
});

test("a 201 carrying no client_id is a failure, not an empty success", async () => {
  const net = http(201, { client_id_issued_at: 123 });
  await assert.rejects(
    () => registerPublicClient({ issuer: ISSUER, fetch: net.fetch }),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "registrationFailed");
      assert.match(err.message, /no client_id/);
      return true;
    },
  );
});

test("a non-JSON body is a failure, not a parse crash", async () => {
  const net = http(201, "<html>proxy interstitial</html>");
  await assert.rejects(
    () => registerPublicClient({ issuer: ISSUER, fetch: net.fetch }),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "registrationFailed");
      assert.match(err.message, /not JSON/);
      return true;
    },
  );
});

test("an issuer nobody supplied is misconfigured, not a request to nowhere", async () => {
  const net = http(201, { client_id: "mcp_x" });
  await assert.rejects(
    () => registerPublicClient({ issuer: "  ", fetch: net.fetch }),
    (err: unknown) => {
      assert.ok(isAuthFlowError(err));
      assert.equal(err.kind, "misconfigured");
      return true;
    },
  );
  assert.equal(net.calls.length, 0);
});

test("a trailing slash on the issuer does not produce a double slash", async () => {
  const net = http(201, { client_id: "mcp_x" });
  await registerPublicClient({ issuer: `${ISSUER}/`, fetch: net.fetch });
  assert.equal(net.calls[0]?.url, `${ISSUER}/register`);
});
