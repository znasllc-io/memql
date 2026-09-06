package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func reqWithProtocols(headers ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/memql/ws", nil)
	for _, h := range headers {
		r.Header.Add("Sec-WebSocket-Protocol", h)
	}
	return r
}

func TestWebSocketSubprotocolCredential(t *testing.T) {
	cases := []struct {
		name       string
		req        *http.Request
		wantScheme string
		wantCred   string
	}{
		{
			name:       "bearer pair in one header value",
			req:        reqWithProtocols("bearer, eyJhbGciOiJSUzI1NiJ9.e30.sig"),
			wantScheme: "bearer",
			wantCred:   "eyJhbGciOiJSUzI1NiJ9.e30.sig",
		},
		{
			// `guest` carried an invitation token and went with the
			// conversational product (epic memql#4988). It is now an unknown
			// scheme, and this case is here so a re-added one is a decision
			// rather than an accident.
			name: "retired guest pair reads as unknown",
			req:  reqWithProtocols("guest, invite-token-abc"),
		},
		{
			name:       "pair split across two header lines",
			req:        reqWithProtocols("bearer", "tok-123"),
			wantScheme: "bearer",
			wantCred:   "tok-123",
		},
		{
			name: "unknown scheme ignored",
			req:  reqWithProtocols("graphql-ws, tok-123"),
		},
		{
			name: "scheme with no credential ignored",
			req:  reqWithProtocols("bearer"),
		},
		{
			name: "no subprotocols",
			req:  httptest.NewRequest(http.MethodGet, "/memql/ws", nil),
		},
		{
			name: "nil request",
			req:  nil,
		},
		{
			name:       "trailing entries ignored",
			req:        reqWithProtocols("bearer, tok-123, something-else"),
			wantScheme: "bearer",
			wantCred:   "tok-123",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			scheme, cred := WebSocketSubprotocolCredential(tc.req)
			if scheme != tc.wantScheme || cred != tc.wantCred {
				t.Errorf("got (%q, %q), want (%q, %q)", scheme, cred, tc.wantScheme, tc.wantCred)
			}
		})
	}
}

func TestWSNegotiableSubprotocols(t *testing.T) {
	got := WSNegotiableSubprotocols()
	if len(got) != 1 || got[0] != WSCredentialSchemeBearer {
		t.Errorf("WSNegotiableSubprotocols() = %v, want [bearer]", got)
	}
}
