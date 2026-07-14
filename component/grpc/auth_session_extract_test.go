package memql

// extractBearerTokenHTTP precedence (#2511): must match the verifier
// middleware's extraction order exactly, so the session-revocation check
// hashes the same token the verifier validated -- Authorization header >
// bearer Sec-WebSocket-Protocol entry > memql_auth cookie > deprecated
// query params.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtractBearerTokenHTTP_Precedence(t *testing.T) {
	build := func(header, subprotocol, cookie, query string) *http.Request {
		target := "/memql/ws"
		if query != "" {
			target += "?" + query
		}
		r := httptest.NewRequest(http.MethodGet, target, nil)
		if header != "" {
			r.Header.Set("Authorization", "Bearer "+header)
		}
		if subprotocol != "" {
			r.Header.Set("Sec-WebSocket-Protocol", "bearer, "+subprotocol)
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: authCookieName, Value: cookie})
		}
		return r
	}

	cases := []struct {
		name string
		req  *http.Request
		want string
	}{
		{name: "header first", req: build("h", "s", "c", "bearer_token=q"), want: "h"},
		{name: "subprotocol beats cookie and query", req: build("", "s", "c", "bearer_token=q"), want: "s"},
		{name: "cookie beats query", req: build("", "", "c", "bearer_token=q"), want: "c"},
		{name: "deprecated bearer_token query", req: build("", "", "", "bearer_token=q"), want: "q"},
		{name: "deprecated token query", req: build("", "", "", "token=q2"), want: "q2"},
		{
			name: "guest subprotocol is not a bearer",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/memql/ws", nil)
				r.Header.Set("Sec-WebSocket-Protocol", "guest, invite")
				return r
			}(),
			want: "",
		},
		{name: "nothing present", req: httptest.NewRequest(http.MethodGet, "/memql/ws", nil), want: ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := extractBearerTokenHTTP(tc.req); got != tc.want {
				t.Errorf("extractBearerTokenHTTP = %q, want %q", got, tc.want)
			}
		})
	}
}
