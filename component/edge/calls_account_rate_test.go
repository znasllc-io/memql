package edge

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func accountRateRequest(body, address string) *http.Request {
	r := accessRequest(http.MethodPost, "/_memql/calls/account", body)
	r.Header.Set("X-Forwarded-For", address)
	return r
}

func TestCallsAccountReadsCannotSpendAuthenticationBudget(t *testing.T) {
	up := newAccessUpstream(t)
	h := callsHandler(t, callsSite(nil), up.server.URL)
	const address = "203.0.113.9"
	for i := 0; i < 600; i++ {
		if rec := serveAccess(h, callsFrom(http.MethodGet, "/_memql/calls/account", address)); rec.Code != http.StatusOK {
			t.Fatalf("account read %d = %d, want the ordinary read budget", i+1, rec.Code)
		}
	}
	requireCallsLimited(t, serveAccess(h, callsFrom(http.MethodGet, "/_memql/calls/session", address)), 100, "1")
	for i := 0; i < 10; i++ {
		r := accountRateRequest(`{"action":"login_begin"}`, address)
		if i%2 == 0 {
			r = callsFrom(http.MethodPost, "/_memql/calls/unlock", address)
		}
		if rec := serveAccess(h, r); rec.Code != http.StatusOK {
			t.Fatalf("authentication attempt %d after 600 account reads = %d, want 200", i+1, rec.Code)
		}
	}
	requireCallsLimited(t, serveAccess(h, accountRateRequest(`{"action":"login_begin"}`, address)), 6000, "6")
	requireCallsLimited(t, serveAccess(h, callsFrom(http.MethodPost, "/_memql/calls/unlock", address)), 6000, "6")
	if up.count() != 610 {
		t.Fatalf("BFF received %d requests, want exactly 600 reads and 10 authentication attempts", up.count())
	}
}

func TestCallsAccountAuthenticationActionsKeepExistingLimits(t *testing.T) {
	for _, action := range []string{"register_begin", "register_finish", "login_begin", "login_finish", "verify_begin", "verify_finish", "add_begin", "add_finish", "claim_begin", "claim_finish", "revoke"} {
		t.Run(action, func(t *testing.T) {
			up := newAccessUpstream(t)
			h := callsHandler(t, callsSite(nil), up.server.URL)
			body := fmt.Sprintf(`{"action":%q}`, action)
			for i := 0; i < 10; i++ {
				if rec := serveAccess(h, accountRateRequest(body, "203.0.113.9")); rec.Code != http.StatusOK {
					t.Fatalf("attempt %d = %d, want 200 inside the existing budget", i+1, rec.Code)
				}
			}
			requireCallsLimited(t, serveAccess(h, accountRateRequest(body, "203.0.113.9")), 6000, "6")
			if rec := serveAccess(h, callsFrom(http.MethodGet, "/_memql/calls/account", "203.0.113.9")); rec.Code != http.StatusOK {
				t.Fatalf("account read after exhausted authentication = %d, want 200", rec.Code)
			}
			if up.count() != 11 {
				t.Fatalf("BFF received %d requests, want 10 mutations and one independent read", up.count())
			}
		})
	}
	t.Run("shared per-site authentication ceiling", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := callsHandler(t, callsSite(nil), up.server.URL)
		for i := 0; i < 60; i++ {
			r := accountRateRequest(`{"action":"register_begin"}`, fmt.Sprintf("203.0.113.%d", i+1))
			if rec := serveAccess(h, r); rec.Code != http.StatusOK {
				t.Fatalf("site authentication attempt %d = %d, want 200", i+1, rec.Code)
			}
		}
		requireCallsLimited(t, serveAccess(h, accountRateRequest(`{"action":"login_begin"}`, "198.18.0.1")), 1000, "1")
		requireCallsLimited(t, serveAccess(h, callsFrom(http.MethodPost, "/_memql/calls/unlock", "198.18.0.2")), 1000, "1")
		if up.count() != 60 {
			t.Fatalf("BFF received %d authentication attempts, want 60", up.count())
		}
	})
}

func TestCallsAccountLogoutRemainsReachableAndKeepsItsOwnLimit(t *testing.T) {
	up := newAccessUpstream(t)
	h := callsHandler(t, callsSite(nil), up.server.URL)
	const address = "203.0.113.9"
	for i := 0; i < 10; i++ {
		serveAccess(h, accountRateRequest(`{"action":"login_begin"}`, address))
	}
	for i := 0; i < 600; i++ {
		serveAccess(h, callsFrom(http.MethodGet, "/_memql/calls/account", address))
	}
	requireCallsLimited(t, serveAccess(h, accountRateRequest(`{"action":"login_begin"}`, address)), 6000, "6")
	requireCallsLimited(t, serveAccess(h, callsFrom(http.MethodGet, "/_memql/calls/account", address)), 100, "1")
	const body = " \n{\"action\":\"\\u006cogout\"}\n"
	r := accountRateRequest(body, address)
	r.ContentLength = -1
	r.TransferEncoding = []string{"chunked"}
	if rec := serveAccess(h, r); rec.Code != http.StatusOK {
		t.Fatalf("logout after exhausted reads and authentication = %d, want 200", rec.Code)
	}
	hits := up.all()
	hit := hits[len(hits)-1]
	if hit.path != "/calls/account" || hit.method != http.MethodPost || hit.body != body || hit.contentLength != int64(len(body)) || len(hit.transferEncoding) != 0 {
		t.Fatalf("classification changed the forwarded logout: %+v", hit)
	}
	for i := 1; i < 120; i++ {
		if rec := serveAccess(h, accountRateRequest(`{"action":"logout"}`, address)); rec.Code != http.StatusOK {
			t.Fatalf("logout %d = %d, want 200 inside control budget", i+1, rec.Code)
		}
	}
	requireCallsLimited(t, serveAccess(h, accountRateRequest(`{"action":"logout"}`, address)), 500, "1")
	if up.count() != 730 {
		t.Fatalf("BFF received %d requests, want 600 reads, 10 authentication attempts and 120 logouts", up.count())
	}
}

func TestCallsAccountAmbiguousLogoutCannotBypassAuthenticationLimit(t *testing.T) {
	up := newAccessUpstream(t)
	h := callsHandler(t, callsSite(nil), up.server.URL)
	const address = "203.0.113.9"
	for i := 0; i < 10; i++ {
		serveAccess(h, accountRateRequest(`{"action":"login_begin"}`, address))
	}
	for _, body := range []string{
		`{"action":"logout","action":"login_begin"}`,
		`{"action":"login_begin","action":"logout"}`,
		`{"action":"logout","action":"logout"}`,
		`{"action":"logout","Action":"login_begin"}`,
		`{"action":"logout","key":""}`,
		`{"action":"logout","unknown":true}`,
		`{"action":"logout"} {"action":"login_begin"}`,
		`[{"action":"logout"}]`,
		`{"action":null}`,
		`{"action":"logout"`,
		`{"Action":"logout"}`,
		strings.Repeat(" ", 1024) + `{"action":"logout"}`,
	} {
		requireCallsLimited(t, serveAccess(h, accountRateRequest(body, address)), 6000, "6")
	}
	r := accountRateRequest(`{"action":"logout"}`+strings.Repeat(" ", 64<<10), address)
	r.ContentLength = -1
	if rec := serveAccess(h, r); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized chunked account body = %d, want 413", rec.Code)
	}
	if up.count() != 10 {
		t.Fatalf("an ambiguous or oversized payload bypassed authentication limits: BFF received %d requests", up.count())
	}
	if rec := serveAccess(h, accountRateRequest(`{"action":"logout"}`, address)); rec.Code != http.StatusOK {
		t.Fatalf("explicit logout after rejected payloads = %d, want its independent control budget", rec.Code)
	}
}
