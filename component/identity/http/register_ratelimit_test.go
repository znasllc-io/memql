package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// register_ratelimit_test.go -- memql#3793.
//
// POST /register is UNAUTHENTICATED by design: RFC 7591 dynamic client
// registration exists so a client nobody pre-configured can complete the flow
// with no human present. Each success writes a v1:identity:oauthClient row, and
// until now nothing bounded how often.
//
// The cost was never only storage. Those rows are an input OTHER trust
// decisions read -- ClientAllowsRedirectURI consults them alongside the static
// list, and memql#3716 had to be designed specifically so a dynamically
// registered row could not grant itself credentialed CORS. Unbounded growth
// makes that table harder to audit at exactly the moment an operator needs to.
//
// WHAT THESE TESTS ARE FOR. The issue asks for a test that watches the limit
// actually refuse -- "not 'the limiter exists'; a run where the Nth call is
// rejected and the (N-1)th is not, so the limit is measured rather than
// asserted." That distinction is the point: a test that only checks a limiter
// field is non-nil passes just as happily when the check is never reached,
// which is the defect it would be written to prevent.

// postRegisterFrom issues a registration from a specific source address, so a
// test can exhaust one caller's bucket and then show a different caller is
// unaffected. httptest.NewRequest sets RemoteAddr to a fixed value, so without
// this every request in a test looks like the same client.
func postRegisterFrom(t *testing.T, s *Server, remoteAddr, jsonBody string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	s.handleRegister(rec, req)
	return rec
}

const registerValidBody = `{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`

// TestRegisterRateLimitRefusesAfterTheBudget is the measurement.
//
// It sets the budget to a small number, spends exactly that many registrations
// from one address, and asserts that every one of them succeeded and the NEXT
// one did not. Both halves matter: a limiter that refused from the first call
// would also make "the last one is a 429" true, and would be broken.
func TestRegisterRateLimitRefusesAfterTheBudget(t *testing.T) {
	const budget = 3
	t.Setenv(envRegisterPerHour, strconv.Itoa(budget))

	s, eng := newRegisterTestServer(true)

	for i := 1; i <= budget; i++ {
		rec := postRegisterFrom(t, s, "203.0.113.7:44100", registerValidBody)
		if rec.Code != http.StatusCreated {
			t.Fatalf("registration %d/%d returned %d, want 201 -- the budget is %d, so this "+
				"call is within it and the limiter must not refuse yet; body=%s",
				i, budget, rec.Code, budget, rec.Body.String())
		}
	}

	rec := postRegisterFrom(t, s, "203.0.113.7:44100", registerValidBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("registration %d returned %d, want 429 -- the per-IP budget of %d was already "+
			"spent, so this one must be refused (memql#3793); body=%s",
			budget+1, rec.Code, budget, rec.Body.String())
	}

	// The refusal must be the documented shape, not a bare status: the endpoint
	// already answers 403 registration_disabled as JSON, so a caller parses
	// errors here.
	var body struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("429 body is not JSON (%v): %s", err, rec.Body.String())
	}
	if body.Error != "rate_limited" {
		t.Errorf("429 error code = %q, want %q; body=%s", body.Error, "rate_limited", rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("429 carries no Retry-After header -- a caller told only 'too many' has nothing " +
			"to schedule against, and the badge-grant limiter sets one")
	}

	// AND NOTHING WAS PERSISTED. The status code is the symptom; the property
	// the issue is about is that a refused call creates no row. A limiter that
	// wrote the row and then returned 429 would pass every assertion above.
	if got := len(eng.created); got != budget {
		t.Errorf("%d oauthClient rows were created, want %d -- the refused registration must "+
			"persist nothing, which is the whole point of bounding this endpoint", got, budget)
	}
}

// TestRegisterRateLimitIsPerIP pins that one noisy caller cannot lock everybody
// else out. A limiter keyed globally rather than per-IP would pass the test
// above and turn a single abusive client into a denial of service against every
// legitimate one.
func TestRegisterRateLimitIsPerIP(t *testing.T) {
	const budget = 2
	t.Setenv(envRegisterPerHour, strconv.Itoa(budget))

	s, _ := newRegisterTestServer(true)

	for i := 0; i < budget+2; i++ {
		postRegisterFrom(t, s, "203.0.113.9:44100", registerValidBody)
	}
	// The noisy caller is now refused...
	if rec := postRegisterFrom(t, s, "203.0.113.9:44100", registerValidBody); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the exhausted caller got %d, want 429 -- the rest of this test measures "+
			"nothing unless that address is actually blocked", rec.Code)
	}
	// ...and a different one is not.
	if rec := postRegisterFrom(t, s, "198.51.100.4:52000", registerValidBody); rec.Code != http.StatusCreated {
		t.Errorf("a DIFFERENT source address got %d, want 201. The limit is per-IP; keyed any "+
			"more broadly, one abusive caller denies service to every legitimate one "+
			"(memql#3793); body=%s", rec.Code, rec.Body.String())
	}
}

// TestRegisterDisabledIsCheckedBeforeTheRateLimit pins the ORDER of the two
// gates, which is a real decision rather than an accident of where the lines
// landed.
//
// A cluster with DCR off must answer registration_disabled to everyone whatever
// their rate. If the limiter ran first, a caller past the budget would learn
// "429" from a server that does not offer registration at all -- a different and
// more interesting answer than the truth, and one that leaks how busy the
// endpoint is on a cluster where it is switched off.
func TestRegisterDisabledIsCheckedBeforeTheRateLimit(t *testing.T) {
	t.Setenv(envRegisterPerHour, "1")

	s, _ := newRegisterTestServer(false) // DCR disabled

	for i := 0; i < 5; i++ {
		rec := postRegisterFrom(t, s, "203.0.113.11:44100", registerValidBody)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("call %d returned %d, want 403 registration_disabled on every call. "+
				"With DCR off the answer must not depend on the caller's rate (memql#3793).",
				i+1, rec.Code)
		}
	}
}

// TestRegisterLimiterIsServerScoped is the anti-flake property the issue calls
// out by name: the badge limiter is Server-scoped rather than a package global
// precisely so one test cannot change what another observes.
//
// Two Servers built in the same process must not share a bucket. If they did,
// this whole file would be order-dependent, and so would every future test that
// happens to register a client.
func TestRegisterLimiterIsServerScoped(t *testing.T) {
	t.Setenv(envRegisterPerHour, "1")

	first, _ := newRegisterTestServer(true)
	if rec := postRegisterFrom(t, first, "203.0.113.13:44100", registerValidBody); rec.Code != http.StatusCreated {
		t.Fatalf("first server, first call: got %d, want 201", rec.Code)
	}
	if rec := postRegisterFrom(t, first, "203.0.113.13:44100", registerValidBody); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("first server, second call: got %d, want 429 (budget is 1)", rec.Code)
	}

	second, _ := newRegisterTestServer(true)
	if rec := postRegisterFrom(t, second, "203.0.113.13:44100", registerValidBody); rec.Code != http.StatusCreated {
		t.Errorf("a SECOND Server refused the same address at %d -- the two are sharing a "+
			"bucket, which makes every test in this package order-dependent (memql#3793)", rec.Code)
	}
}
