// RFC 8414 discovery and the pre-flight it powers (memql#4624).
//
// The defect: the flow opened a browser and parked for 600 seconds without
// ever asking whether the issuer exists, then blamed the browser -- "No sign-in
// callback arrived within 600 seconds... or it could not reach 127.0.0.1" --
// which is wrong for a wrong domain, an unreachable host, a bad certificate, an
// old cluster and a host that is not MemQL at all.
//
// So these tests are mostly about the SENTENCE. A pre-flight that fails fast
// with the wrong reason is no better than a slow one.

import test from "node:test";
import assert from "node:assert/strict";

import type { HttpRequestInit, HttpResponseLike } from "../src/connection/credentials.js";
import {
  OAUTH_METADATA_PATH,
  describeDiscoveryFailure,
  discoverAuthorizationServer,
  normalizeBase,
  supportsAuthorizationCodeWithS256,
  supportsDeviceCode,
} from "../src/auth/discovery.js";

const IDENTITY = "https://identity.example.com";
const API = "https://api.example.com";

function doc(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    issuer: IDENTITY,
    authorization_endpoint: `${IDENTITY}/authorize`,
    token_endpoint: `${IDENTITY}/oauth/token`,
    device_authorization_endpoint: `${IDENTITY}/device/code`,
    jwks_uri: `${IDENTITY}/.well-known/jwks.json`,
    grant_types_supported: [
      "authorization_code",
      "refresh_token",
      "urn:ietf:params:oauth:grant-type:device_code",
    ],
    code_challenge_methods_supported: ["S256"],
    ...overrides,
  };
}

function serving(
  bodies: Record<string, unknown | string>,
  status = 200,
): { fetch: (url: string, init: HttpRequestInit) => Promise<HttpResponseLike>; urls: string[] } {
  const urls: string[] = [];
  return {
    urls,
    fetch: async (url) => {
      urls.push(url);
      const origin = new URL(url).origin;
      const payload = bodies[origin];
      if (payload === undefined) {
        return { ok: false, status: 404, text: async () => "not found" };
      }
      return {
        ok: status >= 200 && status < 300,
        status,
        text: async () => (typeof payload === "string" ? payload : JSON.stringify(payload)),
      };
    },
  };
}

test("the endpoints come from the cluster's own document, not from a convention", async () => {
  // An identity service NOT at identity.<domain>, and mounting /authorize
  // somewhere else. This was unrecoverable without hand-editing clusters.yaml.
  const custom = "https://auth.example.com";
  const { fetch } = serving({
    [custom]: doc({ issuer: custom, authorization_endpoint: `${custom}/sso/authorize` }),
  });

  const outcome = await discoverAuthorizationServer({ baseUrl: custom, fetch });
  assert.equal(outcome.kind, "ok");
  assert.equal(
    outcome.kind === "ok" ? outcome.metadata.authorizationEndpoint : "",
    `${custom}/sso/authorize`,
  );
});

// THE api.<domain> COPY IS A POINTER, NOT A CLAIM. It reports
// `issuer: https://identity.<domain>` rather than its own URL, so a strict RFC
// 8414 issuer match against the fetch URL fails there -- correctly. Following
// it is what makes the front door a usable starting point.
test("a document naming another issuer is followed exactly once", async () => {
  const { fetch, urls } = serving({
    [API]: doc(),        // served from api., naming identity. as the issuer
    [IDENTITY]: doc(),   // and there it matches itself
  });

  const outcome = await discoverAuthorizationServer({ baseUrl: API, fetch });
  assert.equal(outcome.kind, "ok");
  assert.equal(outcome.kind === "ok" ? outcome.metadata.issuer : "", IDENTITY);
  assert.deepEqual(urls, [
    `${API}${OAUTH_METADATA_PATH}`,
    `${IDENTITY}${OAUTH_METADATA_PATH}`,
  ]);
});

test("a chain of pointers is refused, not followed", async () => {
  // One hop is a pointer; two is a redirect chain a hostile document could
  // steer.
  const third = "https://elsewhere.example.com";
  const { fetch } = serving({
    [API]: doc({ issuer: IDENTITY }),
    [IDENTITY]: doc({ issuer: third }),
  });
  const outcome = await discoverAuthorizationServer({ baseUrl: API, fetch });
  assert.equal(outcome.kind, "notAnAuthorizationServer");
});

test("an HTML page is named as one, not as a parse error", async () => {
  // A captive portal, a proxy, or a host that is simply not a MemQL cluster.
  const { fetch } = serving({ [IDENTITY]: "<!doctype html><html><body>Sign in</body></html>" });
  const outcome = await discoverAuthorizationServer({ baseUrl: IDENTITY, fetch });
  assert.equal(outcome.kind, "notAnAuthorizationServer");
  assert.match(describeDiscoveryFailure(IDENTITY, outcome), /HTML page/);
});

test("a host that answers nothing at that path is distinguishable from an unreachable one", async () => {
  const { fetch } = serving({});
  const refused = await discoverAuthorizationServer({ baseUrl: IDENTITY, fetch });
  assert.equal(refused.kind, "refused");
  assert.match(describeDiscoveryFailure(IDENTITY, refused), /answered 404/);

  const unreachable = await discoverAuthorizationServer({
    baseUrl: IDENTITY,
    fetch: async () => {
      throw new Error("getaddrinfo ENOTFOUND identity.example.com");
    },
  });
  assert.equal(unreachable.kind, "unreachable");
  const sentence = describeDiscoveryFailure(IDENTITY, unreachable);
  assert.match(sentence, /could not be reached/);
  assert.match(sentence, /ENOTFOUND/);
  // The old message blamed the browser and 127.0.0.1 for every one of these.
  assert.ok(!/127\.0\.0\.1/.test(sentence), "still blames the loopback listener");
  assert.ok(!/600 seconds/.test(sentence), "still reports the callback deadline");
});

test("JSON without the RFC 8414 fields is not an authorization server", async () => {
  const { fetch } = serving({ [IDENTITY]: { hello: "world" } });
  const outcome = await discoverAuthorizationServer({ baseUrl: IDENTITY, fetch });
  assert.equal(outcome.kind, "notAnAuthorizationServer");
  assert.match(describeDiscoveryFailure(IDENTITY, outcome), /RFC 8414 fields/);
});

test("a missing optional list is not read as a refusal", () => {
  // RFC 8414 leaves these optional. Refusing on absence would reject a
  // conformant server -- only a list that is PRESENT and lacks the value is a
  // no.
  const bare = {
    issuer: IDENTITY,
    authorizationEndpoint: `${IDENTITY}/authorize`,
    tokenEndpoint: `${IDENTITY}/oauth/token`,
    codeChallengeMethodsSupported: [],
    grantTypesSupported: [],
  };
  assert.ok(supportsAuthorizationCodeWithS256(bare));

  assert.ok(
    !supportsAuthorizationCodeWithS256({
      ...bare,
      codeChallengeMethodsSupported: ["plain"],
    }),
    "a server advertising only plain PKCE cannot run this extension's browser flow",
  );
  // Device support needs the endpoint, not just the grant.
  assert.ok(!supportsDeviceCode(bare));
  assert.ok(supportsDeviceCode({ ...bare, deviceAuthorizationEndpoint: `${IDENTITY}/device/code` }));
});

test("normalizeBase composes a bare host and a stored issuer identically", () => {
  assert.equal(normalizeBase("identity.example.com"), IDENTITY);
  assert.equal(normalizeBase(`${IDENTITY}/`), IDENTITY);
  assert.equal(normalizeBase(`${IDENTITY}///`), IDENTITY);
  assert.equal(normalizeBase("  "), "");
  assert.equal(normalizeBase(undefined), "");
  assert.equal(normalizeBase("http://identity.example.com"), "http://identity.example.com");
});
