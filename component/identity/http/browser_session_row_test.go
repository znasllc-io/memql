package http

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// browser_session_row_test.go -- memql#4303's negative pin: "a browser
// session with no authSession row can never return."
//
// # What the absence of the row cost
//
// startBrowserSession minted a first-party `memql_admin` cookie and wrote
// nothing. The concept had DECLARED an `oidc_cookie` source for exactly this
// session since it was written, so the intent was recorded and the code
// simply did not do it. Three things followed, and all three were invisible:
// the session was not listable (the "Active sessions" panel had nothing to
// show, which is why it was a permanent spinner), not revocable (revoke-one
// and revoke-all operate on rows), and not notifiable.
//
// Put together with the group-alias race, that meant a colleague who clicked
// your magic link first held a session on their machine that you could not
// see, could not end, and were never told about.

// sessionEngine records the session writes and answers the user lookup.
type sessionEngine struct {
	mu       sync.Mutex
	sessions []map[string]string
	unknown  []string
}

func (e *sessionEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "query userByEmail("), strings.HasPrefix(q, "query userById("):
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
			Id: "v1:identity:user:u1",
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"id":           structpb.NewStringValue("v1:identity:user:u1"),
				"primaryEmail": structpb.NewStringValue("team@acme.test"),
				"role":         structpb.NewStringValue("owner"),
			}},
		}}}}, nil
	case strings.HasPrefix(q, "mutation createAuthSession("):
		e.sessions = append(e.sessions, parseArgs(q))
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	case strings.HasPrefix(q, "mutation createAuditEvent("):
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	e.unknown = append(e.unknown, q)
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

// parseArgs pulls `name: "value"` pairs out of a rendered mutation. Crude on
// purpose: the assertions below are about which VALUES landed, and a real
// parse would be a second implementation of the renderer.
func parseArgs(q string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(q[strings.Index(q, "(")+1:], "("), ")"), `",`) {
		kv := strings.SplitN(part, `: "`, 2)
		if len(kv) != 2 {
			continue
		}
		out[strings.TrimSpace(strings.Trim(kv[0], `,"`))] = strings.Trim(kv[1], `")`)
	}
	return out
}

// countingNotifier records how many new-sign-in notices were sent.
type countingNotifier struct {
	mu    sync.Mutex
	sent  []identity.SignInNotice
	fails bool
}

func (n *countingNotifier) SendNewSignIn(_ context.Context, in identity.SignInNotice) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, in)
	if n.fails {
		return context.DeadlineExceeded
	}
	return nil
}

func newSessionRowServer(t *testing.T) (*Server, *sessionEngine, *countingNotifier) {
	t.Helper()
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	if err != nil {
		t.Fatalf("NewKeyManager: %v", err)
	}
	if err := km.Load(); err != nil {
		t.Fatalf("KeyManager.Load: %v", err)
	}
	cfg := identity.Config{
		Enabled:     true,
		BaseURL:     "https://identity.test",
		JWTAudience: "memql",
		KeyDir:      dir,
	}
	issuer, err := identity.NewJWTIssuer(km, cfg)
	if err != nil {
		t.Fatalf("NewJWTIssuer: %v", err)
	}
	eng := &sessionEngine{}
	notifier := &countingNotifier{}
	return &Server{
		Cfg:            cfg,
		Store:          &identity.Store{Engine: eng, Logger: slog.Default()},
		Issuer:         issuer,
		Logger:         slog.Default(),
		SignInNotifier: notifier,
	}, eng, notifier
}

func TestBrowserSessionCreatesARow(t *testing.T) {
	s, eng, notifier := newSessionRowServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/landing", nil)
	req.Header.Set("User-Agent", "Firefox/1.0")

	if err := s.StartBrowserSessionFor(rec, req, "v1:identity:user:u1", "team@acme.test", "admin_session_started"); err != nil {
		t.Fatalf("StartBrowserSessionFor: %v", err)
	}

	if len(eng.unknown) > 0 {
		t.Fatalf("the session path issued constructs the fake does not model: %s", strings.Join(eng.unknown, "; "))
	}
	if len(eng.sessions) != 1 {
		t.Fatalf("a browser session wrote %d authSession row(s), want 1.\n\n"+
			"A cookie with no row is a session its owner can neither see nor end -- which is "+
			"exactly the state a colleague's click used to leave behind.", len(eng.sessions))
	}
	row := eng.sessions[0]
	if row["source"] != "oidc_cookie" {
		t.Errorf("session source = %q, want oidc_cookie -- the variant the concept has declared "+
			"for this session since it was written", row["source"])
	}
	if row["clientLabel"] != "Firefox/1.0" {
		t.Errorf("clientLabel = %q, want the User-Agent -- it is what distinguishes two rows in "+
			"the device list", row["clientLabel"])
	}
	if row["userId"] == "" || row["subject"] == "" {
		t.Errorf("session row is missing its subject/userId: %v", row)
	}
	if row["expiresAt"] == "" {
		t.Error("session row has no expiresAt; the list would render a session that never ends")
	}

	// The cookie is still set -- the row is an addition, not a replacement.
	var admin bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == adminCookieName && c.Value != "" {
			admin = true
		}
	}
	if !admin {
		t.Error("no memql_admin cookie was set; the row must not have replaced the session")
	}

	// And one notice went out, for the one session.
	if len(notifier.sent) != 1 {
		t.Fatalf("%d new-sign-in notice(s) sent, want exactly 1", len(notifier.sent))
	}
	if notifier.sent[0].Source != "oidc_cookie" {
		t.Errorf("notice source = %q, want oidc_cookie", notifier.sent[0].Source)
	}
}

// TestNotifyFailureDoesNotBlockTheSignIn pins the asymmetry in
// createSessionRow: a persist failure is fatal, a notify failure is not.
//
// A session with no row cannot be seen or revoked, so minting one silently is
// worse than refusing to sign in. A session nobody was emailed about is a
// session that works, held by somebody who will find it on their profile
// page. A mail outage must not lock people out.
func TestNotifyFailureDoesNotBlockTheSignIn(t *testing.T) {
	s, eng, notifier := newSessionRowServer(t)
	notifier.fails = true

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/landing", nil)

	if err := s.StartBrowserSessionFor(rec, req, "v1:identity:user:u1", "team@acme.test", "admin_session_started"); err != nil {
		t.Fatalf("a failing notifier blocked the sign-in: %v", err)
	}
	if len(eng.sessions) != 1 {
		t.Fatalf("the session row was not written: %d", len(eng.sessions))
	}
}
