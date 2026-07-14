// WebSocket-handshake credential carry (memql#2511).
//
// Browsers cannot set an Authorization header on the WebSocket upgrade, and
// the old query-param stamping (`?bearer_token=` / `?guest_token=` /
// `?token=`) put live credentials in the URL, where ingress access logs and
// browser history capture them. The SDK now offers the credential through
// the subprotocol list -- `new WebSocket(url, ["bearer", token])` -- which
// travels as the `Sec-WebSocket-Protocol` header: absent from request-line
// logs, supported by every browser. The engine negotiates the scheme entry
// ("bearer" / "guest") back on the 101 response.

/** The subprotocol pair for a dial, or undefined when the connection is
 * unauthenticated (or worker-token based, which stays on the query string
 * until the worker surface migrates). */
export function wsAuthSubprotocols(
  auth: { bearer?: string; guestToken?: string } | undefined,
): string[] | undefined {
  if (auth?.bearer) return ["bearer", auth.bearer];
  if (auth?.guestToken) return ["guest", auth.guestToken];
  return undefined;
}
