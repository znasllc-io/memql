package abuse

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// form_body_test.go -- memql#4303's acceptance criterion: "a request the HTTP
// path rejects is rejected by the web path under identical conditions".
//
// # The trap this file exists for
//
// Wrapping POST /login in the stack is one line, and it LOOKS like it works
// the moment you do it: the per-IP limiter runs before the body is read, so a
// rate-limit test passes immediately. Turnstile, the disposable-domain
// blocklist and the MX check all key on the EMAIL, which the peek used to
// extract from JSON only -- and a form body is not JSON. Those three would
// have seen an empty address and passed everything, on the surface a human
// abuser actually uses, while the route showed up as "gated" in every review.

func TestPeekBodyReadsBothEncodings(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		wantEmail   string
		wantToken   string
	}{
		{
			name:        "json (the API issue path)",
			contentType: "application/json",
			body:        `{"email":"a@b.test","turnstile":"tok-json"}`,
			wantEmail:   "a@b.test",
			wantToken:   "tok-json",
		},
		{
			name:        "form (the browser issue path)",
			contentType: "application/x-www-form-urlencoded",
			body:        "form=email&email=a%40b.test&cf-turnstile-response=tok-form",
			wantEmail:   "a@b.test",
			wantToken:   "tok-form",
		},
		{
			name:        "form with a charset parameter",
			contentType: "application/x-www-form-urlencoded; charset=UTF-8",
			body:        "email=a%40b.test",
			wantEmail:   "a@b.test",
		},
		{
			name:        "form using the API's field name for the token",
			contentType: "application/x-www-form-urlencoded",
			body:        "email=a%40b.test&turnstile=tok-alt",
			wantEmail:   "a@b.test",
			wantToken:   "tok-alt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			peek, err := peekBody(tc.contentType, []byte(tc.body))
			if err != nil {
				t.Fatalf("peekBody: %v", err)
			}
			if peek.Email != tc.wantEmail {
				t.Errorf("email = %q, want %q.\n\n"+
					"An email the peek cannot see is an email the disposable-domain blocklist, "+
					"the MX check and the risk score never evaluate -- while the route still "+
					"reads as gated.", peek.Email, tc.wantEmail)
			}
			if peek.Turnstile != tc.wantToken {
				t.Errorf("turnstile token = %q, want %q", peek.Turnstile, tc.wantToken)
			}
		})
	}
}

// TestBothIssuePathsAreGated pins the route set. Adding a third issue path
// without adding it here leaves that path outside every control in this
// package.
func TestBothIssuePathsAreGated(t *testing.T) {
	m := &Middleware{}
	for _, path := range []string{"/auth/magic-link", "/login"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
		if !m.shouldGate(req) {
			t.Errorf("POST %s is not gated by the abuse stack", path)
		}
	}
	// And a path that is not an issue path still passes straight through --
	// so the test above is measuring the route list rather than a middleware
	// that gates everything.
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(""))
	if m.shouldGate(req) {
		t.Error("POST /oauth/token is gated; the stack must only wrap the issue paths")
	}
	// GET /login is the form itself. Gating it would rate-limit reading the
	// page, not submitting it.
	get := httptest.NewRequest(http.MethodGet, "/login", nil)
	if m.shouldGate(get) {
		t.Error("GET /login is gated; only the POST issues anything")
	}
}

// TestRejectRendersHTMLForTheBrowserPath pins that a person tripping a
// control reads a sentence rather than a JSON payload -- and that the reason
// is NOT in it.
//
// Naming the control that fired tells an abuser which one to work around and
// tells a legitimate user nothing they can act on. The audit row carries the
// reason for the operator who can.
func TestRejectRendersHTMLForTheBrowserPath(t *testing.T) {
	var gotStatus int
	var gotReason string
	m := &Middleware{
		RenderReject: func(w http.ResponseWriter, _ *http.Request, status int, reason string) {
			gotStatus, gotReason = status, reason
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("<p>Too many sign-in attempts</p>"))
		},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(""))
	m.reject(rec, req, http.StatusTooManyRequests, FailureRateLimited, "203.0.113.1", "a@b.test", nil)

	if gotStatus != http.StatusTooManyRequests {
		t.Errorf("renderer saw status %d, want 429", gotStatus)
	}
	if gotReason != FailureRateLimited {
		t.Errorf("renderer saw reason %q, want %q -- the renderer decides how much to say, so it "+
			"has to be told", gotReason, FailureRateLimited)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("browser rejection Content-Type = %q, want text/html", ct)
	}
	if strings.Contains(rec.Body.String(), FailureRateLimited) {
		t.Error("the rendered page names the control that fired")
	}

	// Without the hook the JSON envelope is unchanged, so the API's clients
	// keep parsing what they always parsed.
	plain := &Middleware{}
	rec2 := httptest.NewRecorder()
	plain.reject(rec2, httptest.NewRequest(http.MethodPost, "/auth/magic-link", strings.NewReader("")),
		http.StatusTooManyRequests, FailureRateLimited, "203.0.113.1", "a@b.test", nil)
	if ct := rec2.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("API rejection Content-Type = %q, want application/json", ct)
	}
}
