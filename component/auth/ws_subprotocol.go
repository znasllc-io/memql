package auth

// WebSocket handshake credential carry via Sec-WebSocket-Protocol
// (znasllc-io/memql#2511).
//
// Browsers cannot set an Authorization header on a WebSocket upgrade, and
// the legacy `?token=` / `?bearer_token=` query params put live credentials
// in the request line, where ingress/proxy access logs and browser history
// capture them. The subprotocol list is the standard browser-compatible
// header channel (`new WebSocket(url, ["bearer", token])` arrives as
// `Sec-WebSocket-Protocol: bearer, <token>`): the FIRST entry is the scheme
// discriminator, the SECOND is the credential. JWTs (base64url segments
// joined by '.') are valid RFC 6455 subprotocol tokens, so no re-encoding
// is needed.
//
// There is one scheme. A `guest` scheme carried a space-invitation token to
// the guest-aware interceptor, and both went with the conversational product
// (epic memql#4988): nothing mints a guest invitation, so nothing could
// validate one.
//
// The server side must negotiate the scheme entry back on the 101 response
// (nhooyr AcceptOptions.Subprotocols) or browsers abort the handshake; the
// upgrade handlers own that part. This helper owns the parsing, shared by
// the identity-verifier HTTP middleware, the session-revocation middleware,
// and the WS-to-gRPC bridges.

import (
	"net/http"
	"strings"
)

// WSCredentialSchemeBearer is the credential scheme carried as the first
// subprotocol entry.
const WSCredentialSchemeBearer = "bearer"

// WebSocketSubprotocolCredential extracts a credential from the request's
// Sec-WebSocket-Protocol offer list. Returns the scheme ("bearer") and the
// raw credential when the first offered entry is the known scheme and a
// second entry follows; otherwise ("", ""). Only the leading pair is
// considered -- trailing entries are ignored.
func WebSocketSubprotocolCredential(r *http.Request) (scheme, credential string) {
	if r == nil {
		return "", ""
	}
	var entries []string
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(header, ",") {
			if p := strings.TrimSpace(part); p != "" {
				entries = append(entries, p)
			}
		}
	}
	if len(entries) < 2 {
		return "", ""
	}
	if entries[0] == WSCredentialSchemeBearer {
		return entries[0], entries[1]
	}
	return "", ""
}

// WSNegotiableSubprotocols is the list an upgrade handler passes to its
// Accept options so the scheme entry is echoed back on the 101 response
// (browsers require the negotiated subprotocol to be one they offered).
func WSNegotiableSubprotocols() []string {
	return []string{WSCredentialSchemeBearer}
}
