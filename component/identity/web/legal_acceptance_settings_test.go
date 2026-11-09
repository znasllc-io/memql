package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// legal_acceptance_settings_test.go -- the "Legal acceptance" card on
// /me/settings shows the versions THIS person accepted, server-rendered and
// in the native representation MemQL OS draws, and the web server hands the
// versions in force to the session seam that records them.
//
// The card was a permanent spinner that ended in "Could not load acceptance
// history": its script fetched /me/api/legal-acceptance, which nothing
// mounted. It is rendered from the caller's own row now, the way the
// sessions panel on /me/devices is (me_sessions.go), so a read that fails
// says so instead of painting an empty history.

var settingsLegalReadRe = regexp.MustCompile(`^query userLegalAcceptanceById\(userId: "([^"]*)"\)$`)

// settingsLegalEngine answers the acceptance read for ONE user and records
// which user it was asked about.
type settingsLegalEngine struct {
	mu      sync.Mutex
	userId  string
	entries []map[string]any
	err     error
	asked   []string
	unknown []string
}

func (e *settingsLegalEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if strings.HasPrefix(q, "query userByIdSystem(") {
		// The sign-in-security card beside ours reads the caller's row.
		st, err := structpb.NewStruct(map[string]any{"id": e.userId, "primaryEmail": "person@example.test", "role": "writer", "createdAt": "2026-11-09T16:00:00Z"})
		if err != nil {
			return nil, err
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{Id: e.userId, Payload: st}}}}, nil
	}
	if m := settingsLegalReadRe.FindStringSubmatch(q); m != nil {
		e.asked = append(e.asked, m[1])
		if e.err != nil {
			return nil, e.err
		}
		if m[1] != e.userId {
			return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
		}
		list := make([]any, 0, len(e.entries))
		for _, en := range e.entries {
			list = append(list, en)
		}
		st, err := structpb.NewStruct(map[string]any{"id": m[1], "legalAcceptance": list, "createdAt": "2026-11-09T16:00:00Z"})
		if err != nil {
			return nil, err
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{Id: m[1], Payload: st}}}}, nil
	}
	e.unknown = append(e.unknown, q)
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

// legalSettingsServer wires a Server with a real issuer so /me/settings has a
// caller, and the fake store behind it.
func legalSettingsServer(t *testing.T, eng *settingsLegalEngine) (*Server, http.Handler, func(*http.Request, string)) {
	t.Helper()
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	if err != nil {
		t.Fatalf("NewKeyManager: %v", err)
	}
	if err := km.Load(); err != nil {
		t.Fatalf("KeyManager.Load: %v", err)
	}
	cfg := identity.Config{Enabled: true, BaseURL: "https://identity.example.test", JWTAudience: "memql", KeyDir: dir}
	iss, err := identity.NewJWTIssuer(km, cfg)
	if err != nil {
		t.Fatalf("NewJWTIssuer: %v", err)
	}
	srv, err := NewServer(cfg, slog.Default(), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.CountUsers = func(context.Context) (int, error) { return 1, nil }
	srv.Store = &identity.Store{Engine: eng, Logger: slog.Default()}
	srv.SetMeTokens(&MeTokens{Issuer: iss})
	mux := http.NewServeMux()
	srv.Mount(mux)
	signIn := func(r *http.Request, userId string) {
		tok, _, err := iss.IssueAccessToken(identity.IssueInput{UserId: userId, Email: "person@example.test", Role: "writer"}, time.Now().UTC())
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		r.AddCookie(&http.Cookie{Name: "memql_admin", Value: tok})
	}
	return srv, mux, signIn
}

type nativeSettingsPage struct {
	Page string `json:"page"`
	Data struct {
		LegalAcceptance struct {
			Available bool
			Items     []struct{ DocumentType, Document, Version, AcceptedAt string }
		}
	} `json:"data"`
}

func TestLegalDocumentsInForceNamesTheVersionsThePagesShow(t *testing.T) {
	srv, _ := newLegalServer(t, identity.Config{}, nil)
	got := srv.LegalDocumentsInForce()
	want := []identity.LegalDocumentVersion{{DocumentType: "tos", Version: srv.tos.Version}, {DocumentType: "privacy", Version: srv.privacy.Version}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("LegalDocumentsInForce = %+v, want %+v: what a sign-in records must be what the page shows", got, want)
	}
	if !legalVersionRe.MatchString(got[0].Version) || !legalVersionRe.MatchString(got[1].Version) {
		t.Errorf("the embedded documents carry no real version: %+v", got)
	}

	// A document whose front matter declares no version is left out: a
	// record saying somebody accepted version "unknown" is not evidence.
	dir := t.TempDir()
	path := filepath.Join(dir, "privacy.md")
	if err := os.WriteFile(path, []byte("No front matter here.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv2, _ := newLegalServer(t, identity.Config{PrivacyOverridePath: path}, nil)
	got2 := srv2.LegalDocumentsInForce()
	if len(got2) != 1 || got2[0].DocumentType != "tos" {
		t.Errorf("with a versionless privacy override, LegalDocumentsInForce = %+v, want only tos", got2)
	}
}

func TestSettingsShowsTheCallersOwnAcceptanceHistory(t *testing.T) {
	const caller = "v1:identity:user:me"
	eng := &settingsLegalEngine{userId: caller, entries: []map[string]any{
		{"documentType": "tos", "version": "1.0.0", "acceptedAt": "2026-01-02T03:04:05Z", "sourceIP": "192.0.2.1"},
		{"documentType": "privacy", "version": "1.0.0", "acceptedAt": "2026-01-02T03:04:05Z", "sourceIP": "192.0.2.1"},
		{"documentType": "tos", "version": "2.0.0", "acceptedAt": "2026-11-09T16:10:00Z", "sourceIP": "203.0.113.5"},
	}}
	srv, mux, signIn := legalSettingsServer(t, eng)

	// The HTML representation, below the middleware that sends a browser on
	// identity's host to the OS.
	req := httptest.NewRequest(http.MethodGet, "/me/settings", nil)
	req.Header.Set("Accept", "text/html")
	signIn(req, caller)
	rec := httptest.NewRecorder()
	srv.handleMeSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/settings = %d; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Terms of Service", "Privacy Notice", "2.0.0", "1.0.0", "9 Nov 2026", "2 Jan 2026"} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings page does not show %q", want)
		}
	}
	if strings.Contains(body, `data-content="legal"`) {
		t.Errorf("the card still renders a zone for a script to fill; it is server-rendered now")
	}
	if strings.Contains(body, "203.0.113.5") {
		t.Errorf("the page shows the source IP of an acceptance; the person's own card lists documents, versions and dates")
	}
	if len(eng.asked) != 1 || eng.asked[0] != caller {
		t.Fatalf("the page asked about %v, want exactly the caller %q and nobody else", eng.asked, caller)
	}

	// The native representation MemQL OS renders.
	nreq := httptest.NewRequest(http.MethodGet, "/me/settings", nil)
	nreq.Header.Set("Accept", NativeMediaType)
	nreq.Header.Set("Origin", srv.nativeOrigin(nreq))
	signIn(nreq, caller)
	nrec := httptest.NewRecorder()
	mux.ServeHTTP(nrec, nreq)
	if nrec.Code != http.StatusOK {
		t.Fatalf("native GET /me/settings = %d; body: %s", nrec.Code, nrec.Body.String())
	}
	var page nativeSettingsPage
	if err := json.Unmarshal(nrec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v; body: %s", err, nrec.Body.String())
	}
	if page.Page != "me/settings" {
		t.Errorf("page = %q", page.Page)
	}
	la := page.Data.LegalAcceptance
	if !la.Available {
		t.Fatalf("LegalAcceptance.Available = false with a readable history; body: %s", nrec.Body.String())
	}
	if len(la.Items) != 3 {
		t.Fatalf("%d items, want 3; body: %s", len(la.Items), nrec.Body.String())
	}
	// Newest first, as the sessions list is: the version a person is looking
	// for is the one they accepted last.
	if la.Items[0].DocumentType != "tos" || la.Items[0].Version != "2.0.0" || la.Items[0].Document != "Terms of Service" || !strings.Contains(la.Items[0].AcceptedAt, "9 Nov 2026") {
		t.Errorf("Items[0] = %+v, want the newest acceptance, tos 2.0.0 on 9 Nov 2026", la.Items[0])
	}
	if la.Items[2].Document != "Privacy Notice" && la.Items[1].Document != "Privacy Notice" {
		t.Errorf("no item names the Privacy Notice: %+v", la.Items)
	}
	if len(eng.unknown) > 0 {
		t.Errorf("the page issued constructs the fake does not model: %s", strings.Join(eng.unknown, "; "))
	}
}

// A LISTING ERROR IS NOT AN EMPTY LIST: a failed read must not paint "no
// acceptance records", and a page with no caller has nothing to show.
func TestSettingsReportsAnUnreadableAcceptanceHistoryAsUnavailable(t *testing.T) {
	const caller = "v1:identity:user:me"
	eng := &settingsLegalEngine{userId: caller, err: errors.New("the database is away")}
	srv, mux, signIn := legalSettingsServer(t, eng)

	req := httptest.NewRequest(http.MethodGet, "/me/settings", nil)
	req.Header.Set("Accept", "text/html")
	signIn(req, caller)
	rec := httptest.NewRecorder()
	srv.handleMeSettings(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "couldn't load your acceptance history") {
		t.Errorf("a failed read is not reported as unavailable; body: %s", body)
	}
	if strings.Contains(body, "No acceptance records") {
		t.Errorf("a failed read is painted as an empty history")
	}

	nreq := httptest.NewRequest(http.MethodGet, "/me/settings", nil)
	nreq.Header.Set("Accept", NativeMediaType)
	nreq.Header.Set("Origin", srv.nativeOrigin(nreq))
	signIn(nreq, caller)
	nrec := httptest.NewRecorder()
	mux.ServeHTTP(nrec, nreq)
	var page nativeSettingsPage
	if err := json.Unmarshal(nrec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v; body: %s", err, nrec.Body.String())
	}
	if page.Data.LegalAcceptance.Available {
		t.Errorf("native Available = true after a failed read")
	}

	// No caller: nothing to read, and no read is made.
	eng.err = nil
	anon := httptest.NewRequest(http.MethodGet, "/me/settings", nil)
	anon.Header.Set("Accept", "text/html")
	arec := httptest.NewRecorder()
	srv.handleMeSettings(arec, anon)
	if strings.Contains(arec.Body.String(), "No acceptance records") {
		t.Errorf("a signed-out render claims an empty history")
	}
	eng.mu.Lock()
	asked := len(eng.asked)
	eng.mu.Unlock()
	if asked != 2 {
		t.Errorf("%d reads, want 2 (the two signed-in renders) -- a signed-out render must not read anybody's row", asked)
	}
}

// An empty history after a successful read is the one case that may say so.
func TestSettingsSaysWhenNothingIsRecordedYet(t *testing.T) {
	const caller = "v1:identity:user:me"
	eng := &settingsLegalEngine{userId: caller}
	srv, _, signIn := legalSettingsServer(t, eng)
	req := httptest.NewRequest(http.MethodGet, "/me/settings", nil)
	req.Header.Set("Accept", "text/html")
	signIn(req, caller)
	rec := httptest.NewRecorder()
	srv.handleMeSettings(rec, req)
	if !strings.Contains(rec.Body.String(), "No acceptance records") {
		t.Errorf("an empty history is not said to be empty; body: %s", rec.Body.String())
	}
}

// The script that used to fill the card fetched an endpoint nothing mounts
// and would now overwrite the server-rendered card with its error.
func TestMeSettingsScriptNoLongerFetchesTheUnmountedEndpoint(t *testing.T) {
	_, mux := newLegalServer(t, identity.Config{}, nil)
	rec := browserGet(mux, "/static/me-settings.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /static/me-settings.js = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "/me/api/legal-acceptance") {
		t.Error("me-settings.js still fetches /me/api/legal-acceptance, which nothing mounts; it would replace the server-rendered card with \"Could not load acceptance history.\"")
	}
}
