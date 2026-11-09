package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

// legal_notice_test.go -- the account overview (GET /me/, native) names the
// legal-document versions THIS sign-in accepted, so MemQL OS can show the
// person a notice after sign-in: the documents promise notice in the
// platform when a version changes, and a sign-in that recorded a version is
// exactly the moment the version in force differed from what the person had
// accepted (or they had accepted nothing yet).

type nativeOverviewPage struct {
	Page string `json:"page"`
	Data struct {
		Profile      map[string]string
		LegalNotices []struct {
			DocumentType, Document, Version, EffectiveDate string
			Changed                                        bool
		}
	} `json:"data"`
}

func overviewFor(t *testing.T, srv *Server, mux http.Handler, signIn func(*http.Request, string, string), user, session string) (*httptest.ResponseRecorder, nativeOverviewPage) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/me/", nil)
	req.Header.Set("Accept", NativeMediaType)
	req.Header.Set("Origin", srv.nativeOrigin(req))
	signIn(req, user, session)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var page nativeOverviewPage
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode: %v; body: %s", err, rec.Body.String())
		}
	}
	return rec, page
}

// legalNoticeServer is legalSettingsServer with a sign-in that names the
// session the token belongs to.
func legalNoticeServer(t *testing.T, eng *settingsLegalEngine) (*Server, http.Handler, func(*http.Request, string, string)) {
	t.Helper()
	srv, mux, _ := legalSettingsServer(t, eng)
	iss := srv.userIssuer()
	signIn := func(r *http.Request, userId, sessionId string) {
		tok, _, err := iss.IssueAccessToken(identity.IssueInput{UserId: userId, SessionId: sessionId, Email: "person@example.test", Role: "writer"}, time.Now().UTC())
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		r.AddCookie(&http.Cookie{Name: "memql_admin", Value: tok})
	}
	return srv, mux, signIn
}

func TestAccountOverviewNamesWhatThisSignInAccepted(t *testing.T) {
	const caller = "v1:identity:user:me"
	eng := &settingsLegalEngine{userId: caller, entries: []map[string]any{
		{"documentType": "tos", "version": "1.0.0", "acceptedAt": "2026-01-02T03:04:05Z", "sourceIP": "192.0.2.1", "sessionId": "v1:identity:authSession:old"},
		{"documentType": "tos", "version": "2.0.0", "acceptedAt": "2026-11-09T16:10:00Z", "sourceIP": "203.0.113.5", "sessionId": "v1:identity:authSession:now"},
		{"documentType": "privacy", "version": "2.0.0", "acceptedAt": "2026-11-09T16:10:00Z", "sourceIP": "203.0.113.5", "sessionId": "v1:identity:authSession:now"},
	}}
	srv, mux, signIn := legalNoticeServer(t, eng)
	// The embedded documents are what the node serves, so their versions are
	// in force for this test; the fake's 2.0.0 entries must match them.
	if srv.tos.Version != "2.0.0" || srv.privacy.Version != "2.0.0" {
		t.Skipf("embedded document versions moved (%s / %s); adjust the fixture", srv.tos.Version, srv.privacy.Version)
	}

	rec, page := overviewFor(t, srv, mux, signIn, caller, "v1:identity:authSession:now")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/ = %d; body: %s", rec.Code, rec.Body.String())
	}
	if page.Page != "me/profile" || page.Data.Profile["Email"] == "" {
		t.Errorf("the overview page changed shape: %+v", page)
	}
	notices := page.Data.LegalNotices
	if len(notices) != 2 {
		t.Fatalf("%d notices, want 2 (the two entries this session recorded); body: %s", len(notices), rec.Body.String())
	}
	tos, privacy := notices[0], notices[1]
	if tos.DocumentType != "tos" || tos.Document != "Terms of Service" || tos.Version != "2.0.0" || tos.EffectiveDate != srv.tos.EffectiveDate || !tos.Changed {
		t.Errorf("tos notice = %+v, want version 2.0.0 effective %s, Changed (a 1.0.0 entry preceded it)", tos, srv.tos.EffectiveDate)
	}
	if privacy.DocumentType != "privacy" || privacy.Document != "Privacy Notice" || privacy.Version != "2.0.0" || privacy.EffectiveDate != srv.privacy.EffectiveDate || privacy.Changed {
		t.Errorf("privacy notice = %+v, want version 2.0.0 effective %s, not Changed (first acceptance)", privacy, srv.privacy.EffectiveDate)
	}

	// A later session on the same versions recorded nothing: no notice. The
	// list is present and empty, so the OS can tell "nothing to say" from
	// "could not read".
	rec, page = overviewFor(t, srv, mux, signIn, caller, "v1:identity:authSession:later")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/ (later session) = %d", rec.Code)
	}
	if page.Data.LegalNotices == nil || len(page.Data.LegalNotices) != 0 {
		t.Errorf("a session that recorded nothing got notices %+v, want an empty list", page.Data.LegalNotices)
	}
	if !strings.Contains(rec.Body.String(), `"LegalNotices":[]`) {
		t.Errorf("LegalNotices is not an empty list on the wire: %s", rec.Body.String())
	}
}

// A history that cannot be read must not keep a person out of their account
// overview, and must not be reported as "nothing to say".
func TestAccountOverviewSurvivesAnUnreadableHistory(t *testing.T) {
	const caller = "v1:identity:user:me"
	eng := &settingsLegalEngine{userId: caller, err: errors.New("the database is away")}
	srv, mux, signIn := legalNoticeServer(t, eng)
	rec, page := overviewFor(t, srv, mux, signIn, caller, "v1:identity:authSession:now")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/ = %d with an unreadable history; the overview must still render: %s", rec.Code, rec.Body.String())
	}
	if page.Data.Profile["Email"] == "" {
		t.Errorf("the profile is missing: %+v", page.Data)
	}
	if page.Data.LegalNotices != nil {
		t.Errorf("LegalNotices = %+v after a failed read, want absent (null), not an empty list", page.Data.LegalNotices)
	}
	_ = context.Background
}
