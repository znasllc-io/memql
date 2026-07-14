package auth

// WebSocket handshake credential carry via Sec-WebSocket-Protocol
// (znasllc-io/memql#2511).
//
// Browsers cannot set an Authorization header on a WebSocket upgrade, and
// the legacy `?token=` / `?bearer_token=` / `?guest_token=` query params put
// live credentials in the request line, where ingress/proxy access logs and
// browser history capture them. The subprotocol list is the standard
// browser-compatible header channel (`new WebSocket(url, ["bearer", token])`
// arrives as `Sec-WebSocket-Protocol: bearer, <token>`): the FIRST entry is
// the scheme discriminator, the SECOND is the credential. JWTs (base64url
// segments joined by '.') and invite tokens are valid RFC 6455 subprotocol
// tokens, so no re-encoding is needed.
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

// WebSocket credential schemes carried as the first subprotocol entry.
const (
	WSCredentialSchemeBearer = "bearer"
	WSCredentialSchemeGuest  = "guest"
)

// WebSocketSubprotocolCredential extracts a credential from the request's
// Sec-WebSocket-Protocol offer list. Returns the scheme ("bearer" or
// "guest") and the raw credential when the first offered entry is a known
// scheme and a second entry follows; otherwise ("", ""). Only the leading
// pair is considered -- trailing entries are ignored.
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
	switch entries[0] {
	case WSCredentialSchemeBearer, WSCredentialSchemeGuest:
		return entries[0], entries[1]
	}
	return "", ""
}

// WSNegotiableSubprotocols is the list an upgrade handler passes to its
// Accept options so the scheme entry is echoed back on the 101 response
// (browsers require the negotiated subprotocol to be one they offered).
func WSNegotiableSubprotocols() []string {
	return []string{WSCredentialSchemeBearer, WSCredentialSchemeGuest}
}
