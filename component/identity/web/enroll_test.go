package web

// GET /enroll -- the redeem page (memql#3408).
//
// The acceptance criterion these tests exist for is a USER-FACING one: each of
// the four rejection states renders a DISTINCT, human message. That is easy to
// satisfy by accident on the day it is written and easy to lose later by
// factoring the four branches into one -- so the tests read the rendered page
// and compare the four bodies against each other, rather than checking that
// each contains a string somebody typed twice.
//
// They also pin the two protections the page shares with pair.go: HTTPS
// required (with the X-Forwarded-Proto arm), and a per-IP limit on redeem.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

// capturingAudit records every event the page emits, so a test can assert the
// audit trail rather than trusting it.
type capturingAudit struct{ events []identity.AuditEvent }

func (a *capturingAudit) Log(_ context.Context, ev identity.AuditEvent) {
	a.events = append(a.events, ev)
}

func (a *capturingAudit) last() identity.AuditEvent {
	if len(a.events) == 0 {
		return identity.AuditEvent{}
	}
	return a.events[len(a.events)-1]
}

// newEnrolServer builds a mounted server whose validator always answers with
// the supplied resolution.
func newEnrolServer(t *testing.T, res EnrolmentResolution) (*http.ServeMux, *capturingAudit, *Server) {
	t.Helper()
	srv, err := NewServer(identity.Config{BaseURL: "https://identity.example.test", BrandName: "memQL"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	audit := &capturingAudit{}
	srv.SetResolveEnrolment(func(context.Context, string) (EnrolmentResolution, error) {
		return res, nil
	}, audit)
	mux := http.NewServeMux()
	srv.Mount(mux)
	return mux, audit, srv
}

// getEnroll issues a TLS-fronted GET, which is how every real request arrives
// (ingress terminates TLS and forwards plaintext with X-Forwarded-Proto).
func getEnroll(mux *http.ServeMux, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/enroll"+query, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

const liveCode = "?code=mql_enr_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// ---------------------------------------------------------------------------
// The four rejections are four different pages
// ---------------------------------------------------------------------------

func TestEachRejectionStateRendersItsOwnMessage(t *testing.T) {
	states := []struct {
		state  EnrolmentState
		status int
		// mustSay is a phrase only this state's copy should contain. Chosen
		// from the ACTION each one asks for, because that is what makes the
		// four genuinely different rather than four spellings of "no".
		mustSay string
	}{
		{EnrolmentInvalid, http.StatusNotFound, "not valid"},
		{EnrolmentExpired, http.StatusGone, "expired"},
		{EnrolmentAlreadyUsed, http.StatusConflict, "already been used"},
		{EnrolmentRevoked, http.StatusForbidden, "revoked"},
	}

	bodies := map[EnrolmentState]string{}
	for _, tc := range states {
		t.Run(string(tc.state), func(t *testing.T) {
			mux, audit, _ := newEnrolServer(t, EnrolmentResolution{
				State:       tc.state,
				UserId:      "v1:identity:user:target",
				EnrolmentId: "v1:identity:enrolmentToken:tok",
			})
			rec := getEnroll(mux, liveCode)

			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d", rec.Code, tc.status)
			}
			body := rec.Body.String()
			bodies[tc.state] = body

			// The machine-readable state, so a screenshot and a test agree on
			// which page this is without matching prose.
			if !strings.Contains(body, `data-enroll-state="`+string(tc.state)+`"`) {
				t.Errorf("page does not declare data-enroll-state=%q", tc.state)
			}
			if !strings.Contains(strings.ToLower(body), tc.mustSay) {
				t.Errorf("page does not say %q; body:\n%s", tc.mustSay, body)
			}
			// A rejection page must not offer the ceremony.
			if strings.Contains(body, `id="enroll-start"`) {
				t.Errorf("a %s page still renders the create-passkey button", tc.state)
			}
			// Audited, with the source address and its own reason.
			ev := audit.last()
			if ev.Action != "enrolment_redeem_denied" {
				t.Errorf("audit action = %q, want enrolment_redeem_denied", ev.Action)
			}
			if ev.Outcome != identity.AuditOutcomeBlocked {
				t.Errorf("audit outcome = %q, want blocked", ev.Outcome)
			}
			if ev.SourceIP == "" {
				t.Error("audit event carries no SourceIP")
			}
			wantReason := "enrolment_" + strings.ReplaceAll(string(tc.state), "-", "_")
			if ev.FailureReason != wantReason {
				t.Errorf("audit failure reason = %q, want %q", ev.FailureReason, wantReason)
			}
		})
	}

	// THE REAL ASSERTION: no two rejections render the same page. A refactor
	// that collapses them into a shared "this link cannot be used" fails here
	// even if every per-state check above still passes.
	seen := map[string]EnrolmentState{}
	for state, body := range bodies {
		if prior, dup := seen[body]; dup {
			t.Errorf("%s and %s render byte-identical pages -- the four states must be told apart", prior, state)
		}
		seen[body] = state
	}
}

// ---------------------------------------------------------------------------
// The live page
// ---------------------------------------------------------------------------

func TestAValidLinkRendersTheRegistrationPage(t *testing.T) {
	mux, audit, _ := newEnrolServer(t, EnrolmentResolution{
		State:        EnrolmentValid,
		UserId:       "v1:identity:user:target",
		AccountLabel: "target@example.test",
		ExpiresAt:    time.Now().UTC().Add(14 * time.Minute),
		EnrolmentId:  "v1:identity:enrolmentToken:tok",
	})
	rec := getEnroll(mux, liveCode)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-enroll-state="valid"`,
		`id="enroll-start"`,
		"/static/enroll.js",
		"target@example.test",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("live page is missing %q", want)
		}
	}
	if ev := audit.last(); ev.Action != "enrolment_page_served" || ev.Outcome != identity.AuditOutcomeSuccess {
		t.Errorf("audit = %q/%q, want enrolment_page_served/success", ev.Action, ev.Outcome)
	}
}

// THE COPY CONSTRAINT (memql#3405). The spike that would tell us whether
// cross-device (hybrid) enrolment works against a .localhost-family RP ID has
// not run its authenticator leg, so the page must not PROMISE it. This test is
// what makes that a decision somebody has to revisit deliberately rather than
// a sentence that quietly reappears.
//
// It constrains what we promise, not what works: the browser still offers
// whatever it supports.
func TestThePageDoesNotPromisePhoneEnrolment(t *testing.T) {
	mux, _, _ := newEnrolServer(t, EnrolmentResolution{
		State: EnrolmentValid, UserId: "v1:identity:user:target",
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	body := strings.ToLower(getEnroll(mux, liveCode).Body.String())

	for _, forbidden := range []string{"qr code", "scan", "face id", "fingerprint", "your phone", "another device"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the enrolment page says %q -- memql#3405 has not reported, so cross-device "+
				"enrolment must not be promised in copy", forbidden)
		}
	}
	// And it does name the platform authenticator, so the omission above is a
	// constraint rather than an empty page.
	if !strings.Contains(body, "touch id") || !strings.Contains(body, "windows hello") {
		t.Error("the page should name the platform authenticator (Touch ID / Windows Hello)")
	}
}

// ---------------------------------------------------------------------------
// Transport + abuse
// ---------------------------------------------------------------------------

func TestPlaintextRedeemIsRefused(t *testing.T) {
	mux, audit, _ := newEnrolServer(t, EnrolmentResolution{State: EnrolmentValid, UserId: "u"})

	req := httptest.NewRequest(http.MethodGet, "/enroll"+liveCode, nil)
	// No TLS and no X-Forwarded-Proto: a genuinely plaintext hop.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 -- the link carries a credential in its query string", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `id="enroll-start"`) {
		t.Error("a refused plaintext request still rendered the ceremony")
	}
	if ev := audit.last(); ev.FailureReason != "insecure_transport" {
		t.Errorf("audit failure reason = %q, want insecure_transport", ev.FailureReason)
	}
}

func TestForwardedHttpsIsAccepted(t *testing.T) {
	// The arm that matters in production: TLS terminates at the ingress, so
	// r.TLS is nil on every real request and only the header says https.
	mux, _, _ := newEnrolServer(t, EnrolmentResolution{
		State: EnrolmentValid, UserId: "u", ExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	if rec := getEnroll(mux, liveCode); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an X-Forwarded-Proto: https request", rec.Code)
	}
}

func TestAMissingCodeIsRefusedWithoutConsultingTheValidator(t *testing.T) {
	consulted := 0
	srv, err := NewServer(identity.Config{BaseURL: "https://identity.example.test"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	audit := &capturingAudit{}
	srv.SetResolveEnrolment(func(context.Context, string) (EnrolmentResolution, error) {
		consulted++
		return EnrolmentResolution{State: EnrolmentValid}, nil
	}, audit)
	mux := http.NewServeMux()
	srv.Mount(mux)

	rec := getEnroll(mux, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if consulted != 0 {
		t.Errorf("the validator was consulted %d time(s) for a request with no code", consulted)
	}
	if ev := audit.last(); ev.FailureReason != "missing_code" {
		t.Errorf("audit failure reason = %q, want missing_code", ev.FailureReason)
	}
}

func TestRedeemIsRateLimitedPerIP(t *testing.T) {
	t.Setenv(envEnrollPerHour, "2")
	mux, audit, _ := newEnrolServer(t, EnrolmentResolution{
		State: EnrolmentValid, UserId: "u", ExpiresAt: time.Now().UTC().Add(time.Minute),
	})

	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		last = getEnroll(mux, liveCode)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("status after 6 requests = %d, want 429 with a limit of 2/h", last.Code)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Error("a 429 must carry Retry-After")
	}
	if ev := audit.last(); ev.FailureReason != "rate_limited" {
		t.Errorf("audit failure reason = %q, want rate_limited", ev.FailureReason)
	}
}

// A route that cannot be audited is not mounted. Same judgment adminops.New
// makes about its own Audit: an unaudited credential-redeem surface is worse
// than an absent one, because it looks like the trail exists.
func TestEnrollIsNotMountedWithoutAValidatorAndAudit(t *testing.T) {
	srv, err := NewServer(identity.Config{BaseURL: "https://identity.example.test"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	srv.Mount(mux)

	rec := getEnroll(mux, liveCode)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when no validator is wired", rec.Code)
	}
}

func TestHumanizeUntil(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		at   time.Time
		want string
	}{
		{time.Time{}, ""},
		{now.Add(-time.Minute), ""},
		{now.Add(20 * time.Second), "less than a minute"},
		{now.Add(time.Minute), "1 minute"},
		{now.Add(14 * time.Minute), "14 minutes"},
		{now.Add(90 * time.Minute), "2 hours"},
	}
	for _, tc := range cases {
		if got := humanizeUntil(tc.at, now); got != tc.want {
			t.Errorf("humanizeUntil(%v) = %q, want %q", tc.at, got, tc.want)
		}
	}
}
