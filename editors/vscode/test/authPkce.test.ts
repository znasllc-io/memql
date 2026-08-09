// PKCE generation.
//
// The S256 derivation is pinned against RFC 7636's own Appendix B vector rather
// than against this module's output: a test that only checks
// `challenge === sha256(verifier)` recomputed the same way would pass just as
// happily with the wrong encoding (base64 instead of base64url, padded instead
// of unpadded, hex instead of either), and identity compares the challenge byte
// for byte (component/identity/http/token.go, verifyPKCE). Only a value the RFC
// published can catch that class of mistake.

import test from "node:test";
import assert from "node:assert/strict";

import {
  CODE_CHALLENGE_METHOD,
  VERIFIER_BYTES,
  codeChallengeS256,
  generateCodeVerifier,
  generatePkcePair,
  generateState,
} from "../src/auth/pkce.js";

// RFC 7636 Appendix B, verbatim.
const RFC7636_VERIFIER = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk";
const RFC7636_CHALLENGE = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM";

test("S256 challenge matches the RFC 7636 Appendix B vector", () => {
  assert.equal(codeChallengeS256(RFC7636_VERIFIER), RFC7636_CHALLENGE);
});

test("the challenge is unpadded base64url, not base64", () => {
  const challenge = codeChallengeS256(RFC7636_VERIFIER);
  assert.ok(!challenge.includes("="), "padding would have to be percent-encoded in a query string");
  assert.ok(!challenge.includes("+") && !challenge.includes("/"), "base64 alphabet leaked through");
});

test("a generated verifier is 43 url-safe characters, the RFC minimum", () => {
  const verifier = generateCodeVerifier();
  // 32 CSPRNG bytes -> exactly 43 base64url characters.
  assert.equal(verifier.length, 43);
  assert.equal(VERIFIER_BYTES, 32);
  assert.match(verifier, /^[A-Za-z0-9_-]{43}$/);
});

test("every verifier and state is fresh", () => {
  const verifiers = new Set(Array.from({ length: 50 }, () => generateCodeVerifier()));
  const states = new Set(Array.from({ length: 50 }, () => generateState()));
  assert.equal(verifiers.size, 50);
  assert.equal(states.size, 50);
});

test("a generated pair's challenge is the S256 of its own verifier", () => {
  const pair = generatePkcePair();
  assert.equal(pair.method, CODE_CHALLENGE_METHOD);
  assert.equal(pair.method, "S256");
  assert.equal(pair.challenge, codeChallengeS256(pair.verifier));
  assert.notEqual(pair.challenge, pair.verifier);
});

test("state is url-safe so it survives a query string unescaped", () => {
  assert.match(generateState(), /^[A-Za-z0-9_-]+$/);
});
