// Endpoint derivation tests.
//
// clusters.yaml stores a gRPC address (host:port) because the cockpit dials
// native gRPC. This extension speaks the /memql/ws bridge, so the address must
// be lifted to a wss:// URL. Getting this wrong is the difference between
// "connects" and "hangs until the handshake times out".

import test from "node:test";
import assert from "node:assert/strict";

import {
  composeEndpointFromDomain,
  identityBaseUrlFor,
  webSocketUrlFor,
} from "../src/connection/endpoint.js";

test("derives a wss URL from a host:port endpoint", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "cockpit.local.znas.io:443" }),
    "wss://cockpit.local.znas.io/memql/ws",
  );
});

test("preserves a non-standard port", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "cockpit.local.znas.io:8443" }),
    "wss://cockpit.local.znas.io:8443/memql/ws",
  );
});

test("handles an endpoint with no port", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "cockpit.local.znas.io" }),
    "wss://cockpit.local.znas.io/memql/ws",
  );
});

test("uses ws for a plain-http localhost endpoint", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "localhost:50051" }),
    "ws://localhost:50051/memql/ws",
  );
});

test("uses ws for a 127.0.0.1 endpoint", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "127.0.0.1:50051" }),
    "ws://127.0.0.1:50051/memql/ws",
  );
});

test("passes an explicit wss URL through unchanged", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "wss://cockpit.example.com/memql/ws" }),
    "wss://cockpit.example.com/memql/ws",
  );
});

test("appends the bridge path to an explicit scheme without one", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "wss://cockpit.example.com" }),
    "wss://cockpit.example.com/memql/ws",
  );
});

test("throws on an empty endpoint rather than dialing nowhere", () => {
  assert.throws(() => webSocketUrlFor({ name: "l", endpoint: "" }), /endpoint is empty/);
});

test("rejects a non-websocket scheme with a clear message rather than mangling it", () => {
  assert.throws(
    () => webSocketUrlFor({ name: "l", endpoint: "https://cockpit.example.com" }),
    /endpoint scheme must be ws:\/\/ or wss:\/\//,
  );
});

test("derives ws for a bracketed IPv6 loopback literal with a port", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "[::1]:50051" }),
    "ws://[::1]:50051/memql/ws",
  );
});

test("derives ws for a bracketed IPv6 loopback literal with no port", () => {
  assert.equal(webSocketUrlFor({ name: "l", endpoint: "[::1]" }), "ws://[::1]/memql/ws");
});

test("derives wss for a bracketed non-loopback IPv6 literal", () => {
  assert.equal(
    webSocketUrlFor({ name: "l", endpoint: "[2001:db8::1]:50051" }),
    "wss://[2001:db8::1]:50051/memql/ws",
  );
});

// -----------------------------------------------------------------------------
// The identity service's base URL (memql#3385)
//
// A refresh exchange is an HTTP POST to the identity service, which is a
// DIFFERENT host from the bff the stream dials. clusters.yaml already carries
// the two facts that name it -- the OIDC `issuer` the cockpit writes, and the
// `domain` the add/edit flow collects -- so the URL is derived here rather than
// added as a fifth field an operator has to keep in sync.
// -----------------------------------------------------------------------------

test("identityBaseUrlFor prefers an explicit issuer, trimming its trailing slash", () => {
  assert.equal(
    identityBaseUrlFor({
      name: "l",
      endpoint: "cockpit.local.znas.io:443",
      domain: "local.znas.io",
      issuer: "https://auth.example.com/",
    }),
    "https://auth.example.com",
  );
});

test("identityBaseUrlFor falls back to the identity.<domain> convention", () => {
  assert.equal(
    identityBaseUrlFor({ name: "l", endpoint: "cockpit.local.znas.io:443", domain: "local.znas.io" }),
    "https://identity.local.znas.io",
  );
});

test("identityBaseUrlFor derives from a cockpit.<domain> endpoint when no domain is stored", () => {
  assert.equal(
    identityBaseUrlFor({ name: "l", endpoint: "cockpit.staging.example.com:443" }),
    "https://identity.staging.example.com",
  );
});

test("identityBaseUrlFor returns undefined when nothing names the identity service", () => {
  // A bare host:port says nothing about where auth lives. Guessing would send
  // a refresh token to an arbitrary host; the honest answer is "tell me".
  assert.equal(identityBaseUrlFor({ name: "l", endpoint: "10.0.0.5:50051" }), undefined);
  assert.equal(identityBaseUrlFor({ name: "l", endpoint: "" }), undefined);
});

// -----------------------------------------------------------------------------
// the composer (memql#3475)
// -----------------------------------------------------------------------------

test("composeEndpointFromDomain applies the cockpit.<domain>:443 convention", () => {
  assert.equal(composeEndpointFromDomain("staging.example.com"), "cockpit.staging.example.com:443");
});

test("composeEndpointFromDomain normalizes what an operator actually types", () => {
  // Surrounding whitespace from a paste, and the trailing dot of a
  // fully-qualified name. Both resolve identically in DNS and neither matches
  // the endpoint the same operator composes without them, so they are
  // normalized in ONE place rather than at each of the three call sites.
  for (const typed of ["  local.znas.io  ", "local.znas.io.", ".local.znas.io"]) {
    assert.equal(composeEndpointFromDomain(typed), "cockpit.local.znas.io:443", typed);
  }
});

test("composeEndpointFromDomain answers empty for a domain that names nothing", () => {
  // Not `cockpit.:443`. Every caller's next question is "did that name
  // anything", and a hostname built around a hole only moves the check
  // downstream.
  assert.equal(composeEndpointFromDomain(""), "");
  assert.equal(composeEndpointFromDomain("   "), "");
  assert.equal(composeEndpointFromDomain("."), "");
});

test("the composed endpoint is one the dialer accepts, and identity's sibling agrees with it", () => {
  // The composer and the validator are the two halves the registration form
  // leans on, so what one produces the other must accept -- and the endpoint
  // must still name the identity service the sign-in flow POSTs to.
  const endpoint = composeEndpointFromDomain("staging.example.com");
  assert.equal(webSocketUrlFor({ name: "s", endpoint }), "wss://cockpit.staging.example.com/memql/ws");
  assert.equal(
    identityBaseUrlFor({ name: "s", endpoint }),
    "https://identity.staging.example.com",
  );
});
