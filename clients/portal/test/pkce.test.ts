// PKCE generation, checked against RFC 7636's own worked example.
//
// A PKCE bug is invisible at the point of the mistake: the sign-in proceeds
// normally right up to the token exchange, which fails with "PKCE
// verification failed" and no indication of which side computed what. Pinning
// the transform against the RFC's published vector is the cheapest way to
// never spend an afternoon on that.

import { webcrypto } from "node:crypto";
import { describe, expect, it } from "vitest";

import { createPkcePair, newOAuthState, randomToken, type CryptoLike } from "../src/auth/pkce";

// jsdom's `crypto` provides getRandomValues but not `subtle`, so tests inject
// Node's Web Crypto rather than relying on the environment.
const nodeCrypto = webcrypto as unknown as CryptoLike;

// RFC 7636 Appendix B.
const RFC_VERIFIER = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk";
const RFC_CHALLENGE = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM";

// fixedCrypto returns a CryptoLike whose getRandomValues yields a chosen byte
// sequence, so createPkcePair produces a known verifier and the challenge can
// be compared to the RFC's.
function fixedCrypto(bytes: Uint8Array): CryptoLike {
  return {
    getRandomValues<T extends Uint8Array>(array: T): T {
      array.set(bytes.subarray(0, array.length));
      return array;
    },
    subtle: nodeCrypto.subtle,
  };
}

function base64UrlDecode(value: string): Uint8Array {
  const padded = value.replace(/-/g, "+").replace(/_/g, "/");
  const binary = atob(padded + "=".repeat((4 - (padded.length % 4)) % 4));
  return Uint8Array.from(binary, (c) => c.charCodeAt(0));
}

describe("createPkcePair", () => {
  it("computes the S256 challenge exactly as RFC 7636 Appendix B does", async () => {
    const pair = await createPkcePair(fixedCrypto(base64UrlDecode(RFC_VERIFIER)));
    expect(pair.verifier).toBe(RFC_VERIFIER);
    expect(pair.challenge).toBe(RFC_CHALLENGE);
  });

  it("produces a verifier inside the RFC's 43..128 character range", async () => {
    const pair = await createPkcePair(nodeCrypto);
    expect(pair.verifier.length).toBeGreaterThanOrEqual(43);
    expect(pair.verifier.length).toBeLessThanOrEqual(128);
    // Unreserved characters only (RFC 7636 §4.1); no padding.
    expect(pair.verifier).toMatch(/^[A-Za-z0-9\-._~]+$/);
    expect(pair.challenge).toMatch(/^[A-Za-z0-9\-._~]+$/);
  });

  it("never repeats a verifier or a state", async () => {
    const verifiers = new Set<string>();
    const states = new Set<string>();
    for (let i = 0; i < 32; i++) {
      verifiers.add((await createPkcePair(nodeCrypto)).verifier);
      states.add(newOAuthState(nodeCrypto));
    }
    expect(verifiers.size).toBe(32);
    expect(states.size).toBe(32);
  });

  it("fails loudly when Web Crypto is unavailable", () => {
    // The realistic cause is an insecure origin (crypto.subtle is restricted
    // to secure contexts). Failing here, with that in the message, beats a
    // TypeError thrown from inside the redirect.
    const noSubtle = { getRandomValues: <T,>(a: T) => a } as unknown as CryptoLike;
    expect(() => randomToken(32, noSubtle)).toThrow(/Web Crypto is unavailable/);
  });
});
