package verifier

// extractTokenHTTP precedence (#2511): Authorization header > bearer
// Sec-WebSocket-Protocol entry > memql_auth cookie > deprecated query
// params. The subprotocol beats the cookie deliberately -- an explicit dial
// credential wins over an ambient (possibly stale) browser session cookie.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtractTokenHTTP_Precedence(t *testing.T) {
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
		name       string
		req        *http.Request
		wantToken  string
		wantSource string
		wantErr    bool
	}{
		{
			name:       "header beats subprotocol, cookie, query",
			req:        build("h", "s", "c", "bearer_token=q"),
			wantToken:  "h",
			wantSource: "header",
		},
		{
			name:       "subprotocol beats cookie and query",
			req:        build("", "s", "c", "bearer_token=q"),
			wantToken:  "s",
			wantSource: "ws-subprotocol",
		},
		{
			name:       "cookie beats query",
			req:        build("", "", "c", "bearer_token=q"),
			wantToken:  "c",
			wantSource: "cookie",
		},
		{
			name:       "deprecated bearer_token query still accepted",
			req:        build("", "", "", "bearer_token=q"),
			wantToken:  "q",
			wantSource: "query",
		},
		{
			name:       "deprecated token query still accepted",
			req:        build("", "", "", "token=q2"),
			wantToken:  "q2",
			wantSource: "query",
		},
		{
			name:    "guest subprotocol is NOT a bearer credential",
			req:     func() *http.Request { r := httptest.NewRequest(http.MethodGet, "/memql/ws", nil); r.Header.Set("Sec-WebSocket-Protocol", "guest, invite"); return r }(),
			wantErr: true,
		},
		{
			name:    "nothing present errors",
			req:     httptest.NewRequest(http.MethodGet, "/memql/ws", nil),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tok, source, err := extractTokenHTTP(tc.req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got token %q source %q", tok, source)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tok != tc.wantToken || source != tc.wantSource {
				t.Errorf("got (%q, %q), want (%q, %q)", tok, source, tc.wantToken, tc.wantSource)
			}
		})
	}
}
