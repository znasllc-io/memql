// WebSocket-handshake credential carry (memql#2511).
//
// Browsers cannot set an Authorization header on the WebSocket upgrade, and
// the old query-param stamping (`?bearer_token=` / `?token=`) put live
// credentials in the URL, where ingress access logs and browser history
// capture them. The SDK now offers the credential through the subprotocol
// list -- `new WebSocket(url, ["bearer", token])` -- which travels as the
// `Sec-WebSocket-Protocol` header: absent from request-line logs, supported
// by every browser. The engine negotiates the scheme entry ("bearer") back
// on the 101 response.
//
// There is one scheme. A `guest` scheme carried a space-invitation token,
// and it went with the conversational product (epic memql#4988): nothing
// mints a guest invitation, so the engine no longer negotiates it -- and an
// unnegotiated subprotocol aborts the handshake in every browser.

/** The subprotocol pair for a dial, or undefined when the connection is
 * unauthenticated (or worker-token based, which stays on the query string
 * until the worker surface migrates). Returns undefined when `legacyUrlToken`
 * is set: that opt-in falls back to the deprecated query-param carry
 * (`?bearer_token=`, stamped in connection.ts), so the credential must NOT
 * also ride the subprotocol channel. */
export function wsAuthSubprotocols(
  auth: { bearer?: string; legacyUrlToken?: boolean } | undefined,
): string[] | undefined {
  if (auth?.legacyUrlToken) return undefined;
  if (auth?.bearer) return ["bearer", auth.bearer];
  return undefined;
}
