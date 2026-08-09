// The bearer a dial actually carries: its CLASS, its LIFETIME, and its renewal.
//
// Two bugs meet in this module and both are silent in their own way.
//
//   memql#3383 -- the extension sent whatever was in the credential field and
//   the bff rejected a PAT BEFORE any lookup ("verifier: PAT path not wired on
//   this node"), so a perfectly valid PAT failed exactly like a forged one.
//   The class check here is what turns that into a sentence naming the
//   problem, without a round trip.
//
//   memql#3385 -- identity issues 900-second access tokens and the extension
//   held one forever. The refresh exchange here is what makes a session outlive
//   its first token, and the expiry classification is what makes "your
//   credential expired" reportable as something other than "the cluster is
//   down".
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go):
// the SecretStore seam is a structural interface `vscode.SecretStorage`
// satisfies, so the real secret store can be injected in the adapter layer
// while these tests drive a Map.

import test from "node:test";
import assert from "node:assert/strict";

import type { ClusterConfig } from "../src/clusters/model.js";
import {
  CredentialResolver,
  EXPIRY_SKEW_SECONDS,
  classifyToken,
  jwtExpirySeconds,
  refreshTokenSecretKey,
  type HttpRequestInit,
  type HttpResponseLike,
  type SecretStore,
} from "../src/connection/credentials.js";

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

const NOW_MS = 1_800_000_000_000; // a fixed "now" so every expiry is exact

function b64url(value: unknown): string {
  return Buffer.from(JSON.stringify(value)).toString("base64url");
}

// A structurally real JWT: three dot-separated segments, a decodable payload,
// and a signature we never look at. The resolver reads `exp` and MUST NOT
// verify -- verification needs JWKS, which is the server's job.
function jwt(claims: Record<string, unknown>): string {
  return `${b64url({ alg: "RS256", typ: "JWT", kid: "k1" })}.${b64url(claims)}.c2ln`;
}

function jwtExpiringIn(seconds: number): string {
  return jwt({ sub: "user-1", exp: Math.floor(NOW_MS / 1000) + seconds });
}

function cluster(overrides: Partial<ClusterConfig> = {}): ClusterConfig {
  return {
    name: "local",
    endpoint: "cockpit.local.znas.io:443",
    domain: "local.znas.io",
    ...overrides,
  };
}

class FakeSecrets implements SecretStore {
  readonly values = new Map<string, string>();
  async get(key: string): Promise<string | undefined> {
    return this.values.get(key);
  }
  async store(key: string, value: string): Promise<void> {
    this.values.set(key, value);
  }
  async delete(key: string): Promise<void> {
    this.values.delete(key);
  }
}

interface FakeHttp {
  fetch: (url: string, init: HttpRequestInit) => Promise<HttpResponseLike>;
  calls: Array<{ url: string; body: Record<string, unknown> }>;
}

function okHttp(
  payload: Record<string, unknown> = {
    access_token: "REFRESHED",
    refresh_token: "ROTATED",
    expires_in: 900,
  },
): FakeHttp {
  const calls: FakeHttp["calls"] = [];
  return {
    calls,
    fetch: async (url, init) => {
      calls.push({ url, body: JSON.parse(init.body) as Record<string, unknown> });
      return { ok: true, status: 200, text: async () => JSON.stringify(payload) };
    },
  };
}

function rejectingHttp(status = 400, body = '{"error":"invalid_grant","error_description":"refresh token is no longer valid"}'): FakeHttp {
  const calls: FakeHttp["calls"] = [];
  return {
    calls,
    fetch: async (url, init) => {
      calls.push({ url, body: JSON.parse(init.body) as Record<string, unknown> });
      return { ok: false, status, text: async () => body };
    },
  };
}

interface Persisted {
  clusterName: string;
  token: string;
  clearStoredRefreshToken: boolean;
}

function resolver(
  opts: {
    http?: FakeHttp;
    secrets?: SecretStore;
    persisted?: Persisted[];
  } = {},
): CredentialResolver {
  return new CredentialResolver({
    now: () => NOW_MS,
    fetch: opts.http?.fetch,
    secrets: opts.secrets,
    persist: async (clusterName, update) => {
      opts.persisted?.push({ clusterName, ...update });
    },
  });
}

// -----------------------------------------------------------------------------
// Token classification -- memql#3383
// -----------------------------------------------------------------------------

test("classifyToken names a PAT, a worker token, a JWT, and an unrecognised string", () => {
  assert.equal(classifyToken("mql_pat_abcdef"), "pat");
  assert.equal(classifyToken("mql_wkr_abcdef"), "workerToken");
  assert.equal(classifyToken(jwtExpiringIn(600)), "jwt");
  assert.equal(classifyToken("hunter2"), "opaque");
  assert.equal(classifyToken(""), "empty");
  assert.equal(classifyToken(undefined), "empty");
  assert.equal(classifyToken("   "), "empty");
});

test("a PAT is refused BEFORE dialling, with a message naming the token class", async () => {
  // The whole point of memql#3383: the bff rejects a PAT before any lookup, so
  // a VALID PAT fails identically to a forged one and the operator learns
  // nothing. The refusal has to name the class and say what to use instead.
  const result = await resolver().resolve(cluster({ token: "mql_pat_abcdef" }));

  assert.equal(result.ok, false);
  assert.equal(result.ok === false ? result.reason : "", "wrongTokenClass");
  const message = result.ok === false ? result.message : "";
  assert.match(message, /Personal Access Token/);
  assert.match(message, /JWT access token/);
  assert.match(message, /bff/);
});

test("a worker token is refused for the same reason a PAT is", async () => {
  const result = await resolver().resolve(cluster({ token: "mql_wkr_abcdef" }));
  assert.equal(result.ok === false ? result.reason : "", "wrongTokenClass");
});

test("a live JWT is passed through verbatim", async () => {
  const token = jwtExpiringIn(600);
  const result = await resolver().resolve(cluster({ token }));
  assert.deepEqual(result, { ok: true, bearer: token });
});

test("an opaque non-PAT string is passed through -- only the classes the mesh cannot verify are refused", async () => {
  // A token whose shape we do not recognise may still be something the server
  // accepts. Refusing it here would be this module inventing a policy the
  // engine does not have; refusing a PAT is reporting one it does.
  const result = await resolver().resolve(cluster({ token: "some-opaque-bearer" }));
  assert.deepEqual(result, { ok: true, bearer: "some-opaque-bearer" });
});

// -----------------------------------------------------------------------------
// Expiry -- memql#3385
// -----------------------------------------------------------------------------

test("jwtExpirySeconds reads exp without verifying, and returns undefined for a non-JWT", () => {
  assert.equal(jwtExpirySeconds(jwt({ exp: 1234 })), 1234);
  assert.equal(jwtExpirySeconds("mql_pat_x"), undefined);
  assert.equal(jwtExpirySeconds("a.b"), undefined);
  assert.equal(jwtExpirySeconds("a.!!!not-base64!!!.c"), undefined);
  assert.equal(jwtExpirySeconds(jwt({ sub: "u" })), undefined, "no exp claim is not an expiry of 0");
});

test("an expired JWT with no refresh token is reported AS EXPIRED, not as a dial failure", async () => {
  const result = await resolver().resolve(cluster({ token: jwtExpiringIn(-60) }));

  assert.equal(result.ok, false);
  assert.equal(result.ok === false ? result.reason : "", "credentialExpired");
  assert.match(result.ok === false ? result.message : "", /expired/i);
});

test("an empty credential is reported as missing, distinctly from expired", async () => {
  const result = await resolver().resolve(cluster({ token: "" }));
  assert.equal(result.ok === false ? result.reason : "", "missingCredential");
});

test("an empty endpoint is reported as not configured, before any credential question", async () => {
  const result = await resolver().resolve(cluster({ endpoint: "", token: "mql_pat_x" }));
  assert.equal(result.ok === false ? result.reason : "", "notConfigured");
});

// -----------------------------------------------------------------------------
// Refresh -- memql#3385
// -----------------------------------------------------------------------------

test("an expired access token is exchanged against the identity token endpoint", async () => {
  const http = okHttp();
  const persisted: Persisted[] = [];
  const result = await resolver({ http, persisted }).resolve(
    cluster({ token: jwtExpiringIn(-60), refreshToken: "RT-1", clientId: "cockpit" }),
  );

  assert.deepEqual(result, { ok: true, bearer: "REFRESHED" });
  assert.equal(http.calls.length, 1);
  assert.equal(http.calls[0]?.url, "https://identity.local.znas.io/oauth/token");
  assert.deepEqual(http.calls[0]?.body, {
    grant_type: "refresh_token",
    refresh_token: "RT-1",
    client_id: "cockpit",
  });
});

test("refresh is PROACTIVE: a token inside the skew window is renewed before it can fail", async () => {
  // Refreshing only after a failure means the operator sees one broken dial
  // per token lifetime. Renewing just before expiry means they see none.
  const http = okHttp();
  const result = await resolver({ http }).resolve(
    cluster({ token: jwtExpiringIn(EXPIRY_SKEW_SECONDS - 5), refreshToken: "RT-1" }),
  );

  assert.deepEqual(result, { ok: true, bearer: "REFRESHED" });
  assert.equal(http.calls.length, 1);
});

test("a token comfortably inside its lifetime is NOT refreshed", async () => {
  const http = okHttp();
  const token = jwtExpiringIn(EXPIRY_SKEW_SECONDS + 300);
  const result = await resolver({ http }).resolve(cluster({ token, refreshToken: "RT-1" }));

  assert.deepEqual(result, { ok: true, bearer: token });
  assert.equal(http.calls.length, 0, "a live token must not spend a refresh-token rotation");
});

test("the refreshed access token is persisted so the NEXT connect starts fresh", async () => {
  const persisted: Persisted[] = [];
  await resolver({ http: okHttp(), persisted, secrets: new FakeSecrets() }).resolve(
    cluster({ token: jwtExpiringIn(-60), refreshToken: "RT-1" }),
  );

  assert.equal(persisted.length, 1);
  assert.equal(persisted[0]?.clusterName, "local");
  assert.equal(persisted[0]?.token, "REFRESHED");
});

test("a rejected refresh token reports an EXPIRED credential naming the rejection", async () => {
  const http = rejectingHttp();
  const result = await resolver({ http }).resolve(
    cluster({ token: jwtExpiringIn(-60), refreshToken: "RT-STALE" }),
  );

  assert.equal(result.ok === false ? result.reason : "", "credentialExpired");
  const message = result.ok === false ? result.message : "";
  assert.match(message, /refresh token is no longer valid/);
});

test("the identity service's OWN error body shape is read, not just the RFC one", async () => {
  // Captured verbatim from a live cluster:
  //
  //   POST https://identity.local.znas.io/oauth/token -> 400
  //   {"error":"invalid_grant","errorId":"ERR-1845ef",
  //    "message":"refresh token is no longer valid"}
  //
  // identity writes `message`, not RFC 6749's `error_description`
  // (component/identity/http/magic_link.go, writeJSONError). A parser that only
  // knew the RFC spelling would degrade the whole failure to "invalid_grant"
  // and lose the sentence -- against the real service, every time.
  const http = rejectingHttp(
    400,
    '{"error":"invalid_grant","errorId":"ERR-1845ef","message":"refresh token is no longer valid"}',
  );
  const result = await resolver({ http }).resolve(
    cluster({ token: jwtExpiringIn(-60), refreshToken: "RT-STALE" }),
  );

  const message = result.ok === false ? result.message : "";
  assert.match(message, /invalid_grant -- refresh token is no longer valid/);
  assert.match(message, /ERR-1845ef/, "the correlation id must survive into the operator's message");
});

test("a refresh failure on a token that is merely NEARING expiry falls back to the live token", async () => {
  // The proactive window is an optimisation, not a gate. A cluster whose
  // identity service is briefly unreachable must not lose a connection it
  // still holds a valid bearer for.
  const token = jwtExpiringIn(EXPIRY_SKEW_SECONDS - 5);
  const result = await resolver({ http: rejectingHttp(500, "boom") }).resolve(
    cluster({ token, refreshToken: "RT-1" }),
  );

  assert.deepEqual(result, { ok: true, bearer: token });
});

test("a cluster with a refresh token but no identity URL says WHICH field is missing", async () => {
  const http = okHttp();
  const result = await resolver({ http }).resolve({
    name: "bare",
    endpoint: "10.0.0.5:50051",
    token: jwtExpiringIn(-60),
    refreshToken: "RT-1",
  });

  assert.equal(result.ok === false ? result.reason : "", "credentialExpired");
  assert.match(result.ok === false ? result.message : "", /issuer/);
  assert.equal(http.calls.length, 0);
});

test("an explicit issuer wins over the domain convention", async () => {
  const http = okHttp();
  await resolver({ http }).resolve(
    cluster({
      token: jwtExpiringIn(-60),
      refreshToken: "RT-1",
      issuer: "https://auth.example.com/",
    }),
  );
  assert.equal(http.calls[0]?.url, "https://auth.example.com/oauth/token");
});

test("an empty credential WITH a refresh token still refreshes rather than reporting missing", async () => {
  const http = okHttp();
  const result = await resolver({ http }).resolve(cluster({ token: "", refreshToken: "RT-1" }));
  assert.deepEqual(result, { ok: true, bearer: "REFRESHED" });
});

// -----------------------------------------------------------------------------
// SecretStorage custody of the long-lived refresh token
// -----------------------------------------------------------------------------

test("a plaintext refresh token is MIGRATED into SecretStorage and cleared from the file", async () => {
  // The security note on memql#3385: the access token is a 15-minute credential
  // and living in a cockpit-shared plaintext file is a fair trade for a shared
  // registry. A 30-day refresh token is not. So the file is an INGEST path
  // only: the first read takes custody and clears it.
  const secrets = new FakeSecrets();
  const persisted: Persisted[] = [];
  await resolver({ http: okHttp(), secrets, persisted }).resolve(
    cluster({ token: jwtExpiringIn(-60), refreshToken: "RT-1" }),
  );

  assert.equal(secrets.values.get(refreshTokenSecretKey("local")), "ROTATED");
  assert.equal(
    persisted[0]?.clearStoredRefreshToken,
    true,
    "the plaintext refresh token must be deleted once SecretStorage holds it",
  );
});

test("SecretStorage is preferred over the file when both hold a refresh token", async () => {
  const secrets = new FakeSecrets();
  await secrets.store(refreshTokenSecretKey("local"), "RT-FROM-KEYCHAIN");
  const http = okHttp();

  await resolver({ http, secrets }).resolve(
    cluster({ token: jwtExpiringIn(-60), refreshToken: "RT-FROM-FILE" }),
  );

  assert.equal(http.calls[0]?.body.refresh_token, "RT-FROM-KEYCHAIN");
});

test("the ROTATED refresh token replaces the old one -- a stale one is unusable next time", async () => {
  // identity rotates on every exchange (component/identity/refresh). Keeping
  // the presented token would make the second refresh fail with invalid_grant
  // and drop the operator back to hand-editing the file.
  const secrets = new FakeSecrets();
  await secrets.store(refreshTokenSecretKey("local"), "RT-1");
  await resolver({ http: okHttp(), secrets }).resolve(cluster({ token: jwtExpiringIn(-60) }));

  assert.equal(secrets.values.get(refreshTokenSecretKey("local")), "ROTATED");
});

test("with NO SecretStorage the plaintext refresh token is used but never cleared", async () => {
  // Clearing a secret we have nowhere to put would destroy the only copy.
  const persisted: Persisted[] = [];
  const http = okHttp();
  const result = await resolver({ http, persisted }).resolve(
    cluster({ token: jwtExpiringIn(-60), refreshToken: "RT-1" }),
  );

  assert.deepEqual(result, { ok: true, bearer: "REFRESHED" });
  assert.equal(persisted[0]?.clearStoredRefreshToken, false);
});

test("secret keys are per cluster, so two clusters cannot share a refresh token", () => {
  assert.notEqual(refreshTokenSecretKey("local"), refreshTokenSecretKey("staging"));
  assert.match(refreshTokenSecretKey("local"), /local/);
});
