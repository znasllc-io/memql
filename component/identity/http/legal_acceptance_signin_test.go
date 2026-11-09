package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// legal_acceptance_signin_test.go -- a sign-in records the legal-document
// versions in force, once per version, from the one seam every session runs
// through (createSessionRow). The browser-cookie path (magic link, passkey,
// upstream OIDC) and the token-grant path (authorization code, device code)
// are both driven here, because a path that skipped the record would look
// exactly like a person who never signed in.

var (
	fakeLegalReadRe  = regexp.MustCompile(`^query userLegalAcceptanceById\(userId: "([^"]*)"\)$`)
	fakeLegalArrayRe = regexp.MustCompile(`legalAcceptance: (\[.*?\])(?:,|\))`)
)

// legalSessionEngine is sessionEngine plus the user's acceptance history.
type legalSessionEngine struct {
	mu          sync.Mutex
	entries     []map[string]any
	version     time.Time
	failWrites  bool
	sessions    int
	legalReads  int
	legalWrites [][]map[string]any
	unknown     []string
}

func (e *legalSessionEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "query userByEmail("), strings.HasPrefix(q, "query userByIdSystem("), strings.HasPrefix(q, "query userById("):
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
			Id: "v1:identity:user:u1",
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"id":           structpb.NewStringValue("v1:identity:user:u1"),
				"primaryEmail": structpb.NewStringValue("team@acme.test"),
				"role":         structpb.NewStringValue("owner"),
			}},
		}}}}, nil
	case strings.HasPrefix(q, "mutation createAuthSession("):
		e.sessions++
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	case strings.HasPrefix(q, "mutation rotateAuthSession("), strings.HasPrefix(q, "mutation createAuditEvent("):
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	if m := fakeLegalReadRe.FindStringSubmatch(q); m != nil {
		e.legalReads++
		list := make([]any, 0, len(e.entries))
		for _, en := range e.entries {
			list = append(list, en)
		}
		st, err := structpb.NewStruct(map[string]any{
			"id": m[1], "legalAcceptance": list, "createdAt": e.version.UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			return nil, err
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{Id: m[1], Payload: st}}}}, nil
	}
	if strings.HasPrefix(q, "mutation recordLegalAcceptance(") {
		arr := fakeLegalArrayRe.FindStringSubmatch(q)
		if arr == nil {
			return nil, errors.New("fake: no legalAcceptance array in " + q)
		}
		var entries []map[string]any
		if err := json.Unmarshal([]byte(arr[1]), &entries); err != nil {
			return nil, err
		}
		e.legalWrites = append(e.legalWrites, entries)
		if e.failWrites {
			return nil, errors.New("fake: the database is away")
		}
		e.entries = entries
		e.version = e.version.Add(time.Millisecond)
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	e.unknown = append(e.unknown, q)
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

type legalAuditRecorder struct {
	mu     sync.Mutex
	events []identity.AuditEvent
}

func (a *legalAuditRecorder) Log(_ context.Context, ev identity.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
}

func (a *legalAuditRecorder) find(action string) (identity.AuditEvent, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ev := range a.events {
		if ev.Action == action {
			return ev, true
		}
	}
	return identity.AuditEvent{}, false
}

// newLegalSessionServer wires a Server whose sessions record acceptance of
// whatever `docs` returns at the time of the sign-in.
func newLegalSessionServer(t *testing.T, docs *[]identity.LegalDocumentVersion) (*Server, *legalSessionEngine, *legalAuditRecorder) {
	t.Helper()
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	if err != nil {
		t.Fatalf("NewKeyManager: %v", err)
	}
	if err := km.Load(); err != nil {
		t.Fatalf("KeyManager.Load: %v", err)
	}
	cfg := identity.Config{Enabled: true, BaseURL: "https://identity.test", JWTAudience: "memql", KeyDir: dir}
	issuer, err := identity.NewJWTIssuer(km, cfg)
	if err != nil {
		t.Fatalf("NewJWTIssuer: %v", err)
	}
	eng := &legalSessionEngine{version: time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)}
	audit := &legalAuditRecorder{}
	s := &Server{
		Cfg:    cfg,
		Store:  &identity.Store{Engine: eng, Logger: slog.Default()},
		Issuer: issuer,
		Logger: slog.Default(),
		Audit:  audit,
	}
	if docs != nil {
		s.LegalDocuments = func() []identity.LegalDocumentVersion { return *docs }
	}
	return s, eng, audit
}

func signInRequest(ip string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/auth/landing", nil)
	req.Header.Set("User-Agent", "Firefox/1.0")
	req.Header.Set("X-Forwarded-For", ip)
	return req
}

func TestASignInRecordsTheLegalVersionsInForceOnce(t *testing.T) {
	docs := []identity.LegalDocumentVersion{{DocumentType: "tos", Version: "2.0.0"}, {DocumentType: "privacy", Version: "2.0.0"}}
	s, eng, audit := newLegalSessionServer(t, &docs)

	// The browser-cookie seam: magic link, passkey and upstream OIDC all
	// arrive here.
	if err := s.StartBrowserSessionFor(httptest.NewRecorder(), signInRequest("203.0.113.5"), "v1:identity:user:u1", "team@acme.test", "admin_session_started"); err != nil {
		t.Fatalf("StartBrowserSessionFor: %v", err)
	}
	if len(eng.unknown) > 0 {
		t.Fatalf("the sign-in issued constructs the fake does not model: %s", strings.Join(eng.unknown, "; "))
	}
	if len(eng.legalWrites) != 1 {
		t.Fatalf("%d acceptance writes after the first sign-in, want 1", len(eng.legalWrites))
	}
	first := eng.legalWrites[0]
	if len(first) != 2 || first[0]["documentType"] != "tos" || first[1]["documentType"] != "privacy" ||
		first[0]["version"] != "2.0.0" || first[1]["version"] != "2.0.0" {
		t.Errorf("first write = %v, want one entry per document at the version in force", first)
	}
	if first[0]["sourceIP"] != "203.0.113.5" {
		t.Errorf("sourceIP = %v, want the client IP the request arrived from", first[0]["sourceIP"])
	}
	if at, _ := first[0]["acceptedAt"].(string); at == "" {
		t.Errorf("acceptedAt is empty: %v", first[0])
	}
	ev, ok := audit.find("legal_acceptance_recorded")
	if !ok {
		t.Fatalf("no legal_acceptance_recorded audit event; actions seen: %v", auditActions(audit))
	}
	if ev.ActorUserId != "v1:identity:user:u1" || ev.TargetType != "user" || ev.TargetId != "v1:identity:user:u1" || ev.Outcome != identity.AuditOutcomeSuccess {
		t.Errorf("audit event = %+v, want a success naming the user", ev)
	}
	if docsDetail, _ := json.Marshal(ev.Detail["documents"]); !strings.Contains(string(docsDetail), `"tos"`) || !strings.Contains(string(docsDetail), `"2.0.0"`) {
		t.Errorf("audit detail does not name the documents and versions recorded: %s", docsDetail)
	}

	// A second sign-in on the same versions: a session, a notice, no record.
	if err := s.StartBrowserSessionFor(httptest.NewRecorder(), signInRequest("203.0.113.6"), "v1:identity:user:u1", "team@acme.test", "admin_session_started"); err != nil {
		t.Fatalf("second StartBrowserSessionFor: %v", err)
	}
	if eng.sessions != 2 {
		t.Fatalf("%d sessions, want 2", eng.sessions)
	}
	if len(eng.legalWrites) != 1 {
		t.Fatalf("%d acceptance writes after a second sign-in on the same versions, want still 1: the record is per version, not per session", len(eng.legalWrites))
	}

	// The token-grant seam (authorization code, device code), after the
	// operator published new terms: exactly the changed document is appended.
	docs = []identity.LegalDocumentVersion{{DocumentType: "tos", Version: "2.1.0"}, {DocumentType: "privacy", Version: "2.0.0"}}
	if _, err := s.issueSessionForUser(httptest.NewRecorder(), signInRequest("203.0.113.7"), sessionMintInput{UserId: "v1:identity:user:u1", Source: "device_code"}); err != nil {
		t.Fatalf("issueSessionForUser: %v", err)
	}
	if eng.sessions != 3 {
		t.Fatalf("%d sessions, want 3", eng.sessions)
	}
	if len(eng.legalWrites) != 2 {
		t.Fatalf("%d acceptance writes after new terms, want 2", len(eng.legalWrites))
	}
	last := eng.legalWrites[1]
	if len(last) != 3 || last[2]["documentType"] != "tos" || last[2]["version"] != "2.1.0" || last[2]["sourceIP"] != "203.0.113.7" {
		t.Errorf("write after new terms = %v, want the two earlier entries kept and tos 2.1.0 appended", last)
	}
	if len(eng.unknown) > 0 {
		t.Fatalf("the token path issued constructs the fake does not model: %s", strings.Join(eng.unknown, "; "))
	}
}

// A sign-in is the acceptance (the terms say so); the record is evidence of
// it. A record that cannot be written is logged and audited, and the next
// sign-in writes it -- failing the sign-in would turn a bookkeeping outage
// into a lockout.
func TestARecordingFailureNeverFailsTheSignIn(t *testing.T) {
	docs := []identity.LegalDocumentVersion{{DocumentType: "tos", Version: "2.0.0"}, {DocumentType: "privacy", Version: "2.0.0"}}
	s, eng, audit := newLegalSessionServer(t, &docs)
	eng.failWrites = true

	rec := httptest.NewRecorder()
	if err := s.StartBrowserSessionFor(rec, signInRequest("203.0.113.5"), "v1:identity:user:u1", "team@acme.test", "admin_session_started"); err != nil {
		t.Fatalf("a failed acceptance record failed the sign-in: %v", err)
	}
	if eng.sessions != 1 {
		t.Fatalf("%d sessions, want 1", eng.sessions)
	}
	var admin bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == adminCookieName && c.Value != "" {
			admin = true
		}
	}
	if !admin {
		t.Error("no memql_admin cookie: the session was not completed")
	}
	ev, ok := audit.find("legal_acceptance_record_failed")
	if !ok {
		t.Fatalf("no legal_acceptance_record_failed audit event; actions seen: %v", auditActions(audit))
	}
	if ev.Outcome != identity.AuditOutcomeFailure || ev.ActorUserId != "v1:identity:user:u1" {
		t.Errorf("audit event = %+v, want a failure naming the user", ev)
	}
	if _, recorded := audit.find("legal_acceptance_recorded"); recorded {
		t.Error("a failed write was audited as recorded")
	}
}

// A binary with no legal documents wired (an engine-less harness, a test
// that models only the session row) records nothing and READS nothing.
func TestNoLegalDocumentsWiredMeansNoAcceptanceReads(t *testing.T) {
	s, eng, audit := newLegalSessionServer(t, nil)
	if err := s.StartBrowserSessionFor(httptest.NewRecorder(), signInRequest("203.0.113.5"), "v1:identity:user:u1", "team@acme.test", "admin_session_started"); err != nil {
		t.Fatalf("StartBrowserSessionFor: %v", err)
	}
	if eng.legalReads != 0 || len(eng.legalWrites) != 0 {
		t.Errorf("reads=%d writes=%d with no documents wired, want none", eng.legalReads, len(eng.legalWrites))
	}
	if _, ok := audit.find("legal_acceptance_recorded"); ok {
		t.Error("an acceptance was audited with no documents wired")
	}
}

func auditActions(a *legalAuditRecorder) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.events))
	for _, ev := range a.events {
		out = append(out, ev.Action)
	}
	return out
}
