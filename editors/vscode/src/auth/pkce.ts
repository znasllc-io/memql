// PKCE (RFC 7636) and CSRF-state generation for the browser sign-in flow.
//
// Every value here is a SECRET or an ANTI-FORGERY TOKEN, so all of it comes
// from node:crypto's CSPRNG. `Math.random()` is not a near-miss that happens to
// be weaker -- V8 seeds it from a value an attacker can recover from a handful
// of outputs, which would make a code_verifier predictable and hand a
// network-adjacent attacker the ability to redeem an intercepted authorization
// code. There is no size of `Math.random()` output that fixes that.
//
// identity REQUIRES S256 (component/identity/web/authorize.go rejects a missing
// code_challenge or any method other than "S256" as invalid_request), so the
// "plain" method RFC 7636 also defines is not implemented here at all.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { createHash, randomBytes } from "node:crypto";

/**
 * How many random bytes back a verifier.
 *
 * RFC 7636 §4.1 requires the base64url-encoded verifier to be 43-128
 * characters. 32 bytes encodes to exactly 43 -- the shortest string the spec
 * admits, and 256 bits of entropy, which is the point.
 */
export const VERIFIER_BYTES = 32;

/** How many random bytes back the `state` parameter. */
export const STATE_BYTES = 16;

/** The only code_challenge_method identity accepts. */
export const CODE_CHALLENGE_METHOD = "S256" as const;

export interface PkcePair {
  /** The secret held by this process and presented at the token endpoint. */
  verifier: string;
  /** base64url(sha256(verifier)), sent to the authorization endpoint. */
  challenge: string;
  /** Always "S256". */
  method: typeof CODE_CHALLENGE_METHOD;
}

/**
 * randomUrlSafeToken returns `byteLength` CSPRNG bytes as unpadded base64url.
 *
 * Unpadded because every consumer of this value is a URL query parameter, and
 * base64's "=" padding is a character that has to be percent-encoded there.
 */
export function randomUrlSafeToken(byteLength: number): string {
  return randomBytes(byteLength).toString("base64url");
}

/** generateCodeVerifier mints a fresh RFC 7636 code_verifier. */
export function generateCodeVerifier(): string {
  return randomUrlSafeToken(VERIFIER_BYTES);
}

/**
 * codeChallengeS256 derives the challenge from a verifier.
 *
 * base64url(SHA256(ASCII(verifier))), unpadded -- exactly what identity
 * recomputes and compares in constant time (component/identity/http/token.go,
 * verifyPKCE).
 */
export function codeChallengeS256(verifier: string): string {
  return createHash("sha256").update(verifier, "ascii").digest("base64url");
}

/** generatePkcePair mints a verifier and its matching S256 challenge. */
export function generatePkcePair(): PkcePair {
  const verifier = generateCodeVerifier();
  return { verifier, challenge: codeChallengeS256(verifier), method: CODE_CHALLENGE_METHOD };
}

/**
 * generateState mints the `state` parameter.
 *
 * It is compared against the value the callback carries before the code is
 * exchanged (see flow.ts), which is what stops a third party from walking a
 * code of their choosing into this extension's loopback listener.
 */
export function generateState(): string {
  return randomUrlSafeToken(STATE_BYTES);
}
