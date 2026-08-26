// RFC 8414 discovery (znasllc-io/memql#4624).
//
// WHAT THIS CLOSES. The extension derived the identity host purely by
// convention and hard-coded four endpoint paths, while the cluster had been
// publishing the metadata all along. Three consequences, all of which these
// cases pin:
//
//   - an identity service NOT at `identity.<domain>` was unreachable without
//     hand-editing clusters.yaml;
//   - pasting the API host composed `api.api.example.com` and dead-ended at
//     sign-in much later;
//   - `cluster.issuer` was written by nothing, which is what made the claim
//     probe in memql#4620 unreachable.

import assert from "node:assert/strict";
import { test } from "node:test";

import { discoverIssuer, type DiscoveryFetch } from "../src/connection/discovery.js";

const IDENTITY = "https://identity.example.com";
const API = "https://api.example.com";

function metadata(issuer: string): Record<string, unknown> {
  return {
    issuer,
    authorization_endpoint: `${issuer}/authorize`,
    token_endpoint: `${issuer}/oauth/token`,
    device_authorization_endpoint: `${issuer}/device/code`,
    jwks_uri: `${issuer}/.well-known/jwks.json`,
  };
}

function server(
  routes: Record<string, { status: number; body: string } | Error>,
): { fetch: DiscoveryFetch; urls: string[] } {
  const urls: string[] = [];
  const fetch: DiscoveryFetch = async (url) => {
    urls.push(url);
    const hit = routes[url];
    if (hit === undefined) return { ok: false, status: 404, text: async () => "not found" };
    if (hit instanceof Error) throw hit;
    return {
      ok: hit.status >= 200 && hit.status < 300,
      status: hit.status,
      text: async () => hit.body,
    };
  };
  return { fetch, urls };
}

const WK = "/.well-known/oauth-authorization-server";

// ---------------------------------------------------------------------------
// the ordinary case
// ---------------------------------------------------------------------------

test("endpoints come from the document, not from a convention", async () => {
  // Deliberately NON-conventional paths. A cluster that publishes them means
  // them, and composing `${issuer}/authorize` over the top is how a conformant
  // deployment becomes unreachable.
  const doc = {
    issuer: IDENTITY,
    authorization_endpoint: `${IDENTITY}/oauth2/v1/auth`,
    token_endpoint: `${IDENTITY}/oauth2/v1/token`,
  };
  const { fetch } = server({ [IDENTITY + WK]: { status: 200, body: JSON.stringify(doc) } });

  const result = await discoverIssuer(IDENTITY, fetch);
  assert.equal(result.ok, true);
  assert.equal(result.ok && result.authorizationEndpoint, `${IDENTITY}/oauth2/v1/auth`);
  assert.equal(result.ok && result.tokenEndpoint, `${IDENTITY}/oauth2/v1/token`);
  assert.equal(result.ok && result.issuer, IDENTITY);
});

test("a trailing slash on the base does not break the well-known URL", async () => {
  const { fetch, urls } = server({
    [IDENTITY + WK]: { status: 200, body: JSON.stringify(metadata(IDENTITY)) },
  });
  const result = await discoverIssuer(IDENTITY + "/", fetch);
  assert.equal(result.ok, true);
  assert.deepEqual(urls, [IDENTITY + WK]);
});

// ---------------------------------------------------------------------------
// the signpost: the copy served from the API host
// ---------------------------------------------------------------------------
//
// The cluster serves the same document from `api.<domain>` carrying
// `issuer: https://identity.<domain>` rather than its own URL. That is right --
// `api.<domain>` is not an issuer and nothing signs tokens there -- and it also
// means a strict RFC 8414 §3.3 match against the fetched URL fails there. So it
// is read as a signpost and followed exactly once.

test("a document naming a different issuer is followed once, to that issuer", async () => {
  const { fetch, urls } = server({
    [API + WK]: { status: 200, body: JSON.stringify(metadata(IDENTITY)) },
    [IDENTITY + WK]: { status: 200, body: JSON.stringify(metadata(IDENTITY)) },
  });

  const result = await discoverIssuer(API, fetch);
  assert.equal(result.ok, true, "the API host's pointer was not followed");
  assert.equal(result.ok && result.issuer, IDENTITY);
  assert.deepEqual(urls, [API + WK, IDENTITY + WK]);
});

// One hop is all the real topology needs. A chain would let a hostile or
// misconfigured host walk a client to an issuer of its choosing.
test("a second redirection is refused rather than followed", async () => {
  const ELSEWHERE = "https://elsewhere.example.net";
  const { fetch, urls } = server({
    [API + WK]: { status: 200, body: JSON.stringify(metadata(IDENTITY)) },
    [IDENTITY + WK]: { status: 200, body: JSON.stringify(metadata(ELSEWHERE)) },
  });

  const result = await discoverIssuer(API, fetch);
  assert.equal(result.ok, false);
  assert.equal(!result.ok && result.kind, "issuerMismatch");
  assert.equal(urls.length, 2, "a chain of redirections was walked");
});

// ---------------------------------------------------------------------------
// the failures, told apart
// ---------------------------------------------------------------------------
//
// The split is load-bearing: flow.ts fails fast on `unreachable` (nothing can
// complete a sign-in against a host that does not answer) and DEGRADES on
// `notAnIssuer` (which is also what a cluster too old to publish the document
// looks like, and refusing there would be a regression traded for a
// diagnosis).

test("nothing answering is unreachable, and names the host", async () => {
  const { fetch } = server({ [IDENTITY + WK]: new TypeError("fetch failed") });
  const result = await discoverIssuer(IDENTITY, fetch);
  assert.equal(!result.ok && result.kind, "unreachable");
  assert.equal(
    !result.ok ? result.message : "",
    `${IDENTITY}${WK} could not be reached (fetch failed)`,
    "the message must name the URL that did not answer -- an operator cannot act on a " +
      "failure that does not say what it was talking to",
  );
});

test("undici's real reason is carried out of .cause", async () => {
  const wrapper = new TypeError("fetch failed");
  (wrapper as { cause?: unknown }).cause = new Error("unable to verify the first certificate");
  const { fetch } = server({ [IDENTITY + WK]: wrapper });
  const result = await discoverIssuer(IDENTITY, fetch);
  assert.match(
    !result.ok ? result.message : "",
    /unable to verify the first certificate/,
    'the operator reads "fetch failed" and learns nothing (memql#4619)',
  );
});

test("a 404 is notAnIssuer, so an old cluster degrades instead of being refused", async () => {
  const { fetch } = server({});
  const result = await discoverIssuer(IDENTITY, fetch);
  assert.equal(!result.ok && result.kind, "notAnIssuer");
});

test("an HTML page is notAnIssuer, not a parse crash", async () => {
  const { fetch } = server({
    [IDENTITY + WK]: { status: 200, body: "<!doctype html><title>Login</title>" },
  });
  const result = await discoverIssuer(IDENTITY, fetch);
  assert.equal(!result.ok && result.kind, "notAnIssuer");
});

test("JSON without the required fields is notAnIssuer", async () => {
  const { fetch } = server({
    [IDENTITY + WK]: { status: 200, body: JSON.stringify({ issuer: IDENTITY }) },
  });
  const result = await discoverIssuer(IDENTITY, fetch);
  assert.equal(!result.ok && result.kind, "notAnIssuer");
});

test("a 500 is unreachable rather than notAnIssuer", async () => {
  const { fetch } = server({ [IDENTITY + WK]: { status: 500, body: "boom" } });
  const result = await discoverIssuer(IDENTITY, fetch);
  assert.equal(
    !result.ok && result.kind,
    "unreachable",
    "a server error is a host that cannot answer, not a host that is not an issuer -- " +
      "and the two are treated differently by the sign-in pre-flight",
  );
});

test("no base URL is a refusal, not a request", async () => {
  const { fetch, urls } = server({});
  const result = await discoverIssuer("", fetch);
  assert.equal(result.ok, false);
  assert.deepEqual(urls, []);
});
