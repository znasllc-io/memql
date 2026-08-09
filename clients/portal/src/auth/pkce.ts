// PKCE (RFC 7636) and the CSRF `state` value for the portal's OAuth flow.
//
// WHY PKCE AT ALL, given the portal is not a native app. The portal is a
// PUBLIC OAuth client: it ships as static JavaScript, so it cannot hold a
// client secret -- anything embedded in the bundle is readable by anyone who
// can load the bundle. The identity service knows this and accepts only
// `token_endpoint_auth_method: none` (see the RFC 8414 metadata in
// component/identity/oauth_metadata.go); it requires `code_challenge` with
// `code_challenge_method=S256` at /authorize and refuses the request without
// one (component/identity/web/authorize.go). PKCE is what replaces the secret:
// the authorization code is bound to a verifier that never left this browser,
// so a code intercepted in transit -- out of a redirect URL, a referrer
// header, a proxy log -- cannot be redeemed by whoever intercepted it.
//
// The verifier is the one secret this module produces. It lives in
// sessionStorage across the redirect (see pending.ts, which explains why that
// is acceptable) and is destroyed the moment the code is exchanged.

// VERIFIER_BYTES is 32 random bytes -> 43 base64url characters, comfortably
// inside RFC 7636's 43..128 range. The RFC's minimum is 43 characters
// precisely because that is 32 bytes of entropy; going lower is the mistake
// the range exists to prevent.
const VERIFIER_BYTES = 32;

// STATE_BYTES backs the OAuth `state` parameter. Its job is narrow and it is
// NOT interchangeable with the verifier: state proves that the callback the
// browser just handled belongs to an authorization THIS tab started, which is
// what stops an attacker from feeding the portal a code of their own choosing
// (OAuth 2.0 §10.12, login CSRF).
const STATE_BYTES = 32;

export interface PkcePair {
  // Kept in this browser and sent only to the token endpoint.
  verifier: string;
  // Sent to /authorize, where it is public by design.
  challenge: string;
}

// CryptoLike is the slice of the Web Crypto API used here. Narrowed to an
// interface rather than taking a `Crypto` so the test can supply a
// deterministic stand-in without constructing a whole Crypto object.
export interface CryptoLike {
  getRandomValues<T extends Uint8Array>(array: T): T;
  subtle: { digest(algorithm: string, data: BufferSource): Promise<ArrayBuffer> };
}

function requireCrypto(impl?: CryptoLike): CryptoLike {
  const crypto = impl ?? (globalThis.crypto as CryptoLike | undefined);
  // Web Crypto is unavailable on an insecure origin that is not localhost.
  // Failing loudly HERE is the point: the silent alternative is a sign-in
  // button that throws a TypeError deep inside the redirect, which reads as a
  // portal bug rather than "this page must be served over HTTPS".
  if (!crypto?.subtle || typeof crypto.getRandomValues !== "function") {
    throw new Error(
      "memQL portal: Web Crypto is unavailable, so the sign-in flow cannot " +
        "generate a PKCE verifier. Serve the portal over HTTPS (or from " +
        "localhost) -- crypto.subtle is restricted to secure contexts.",
    );
  }
  return crypto;
}

// randomToken returns `bytes` of CSPRNG output as base64url text.
export function randomToken(bytes: number, impl?: CryptoLike): string {
  const buf = new Uint8Array(bytes);
  requireCrypto(impl).getRandomValues(buf);
  return base64UrlEncode(buf);
}

export function newOAuthState(impl?: CryptoLike): string {
  return randomToken(STATE_BYTES, impl);
}

// createPkcePair mints a verifier and its S256 challenge.
export async function createPkcePair(impl?: CryptoLike): Promise<PkcePair> {
  const crypto = requireCrypto(impl);
  const verifier = randomToken(VERIFIER_BYTES, crypto);
  return { verifier, challenge: await s256Challenge(verifier, crypto) };
}

// s256Challenge is BASE64URL(SHA256(ASCII(verifier))), per RFC 7636 §4.2.
// The digest is over the verifier's ASCII BYTES, not the raw random bytes it
// was encoded from -- a subtle difference that produces a challenge the server
// rejects with "PKCE verification failed" and no clue as to why.
async function s256Challenge(verifier: string, crypto: CryptoLike): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier));
  return base64UrlEncode(new Uint8Array(digest));
}

// base64UrlEncode is unpadded base64url (RFC 4648 §5). Padding is stripped
// because RFC 7636 §4.2 requires it stripped; a trailing "=" is also a
// URL-reserved character the query encoder would then have to escape.
function base64UrlEncode(bytes: Uint8Array): string {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}
