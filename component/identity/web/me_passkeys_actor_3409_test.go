package web

// me_passkeys_actor_3409_test.go is the memql#3409 acceptance criterion
// that the issue insisted be a TEST rather than an inspection: rows scope
// to the authenticated user, and one user cannot see or revoke another's
// credentials.
//
// Shaped after me_tokens_actor_3178_test.go, which pins the same property
// one page over, and extended in the direction that matters here. That
// test asserts the CONTEXT is stamped; these assert the CONSEQUENCE --
// that a request naming somebody else's passkey performs no write. Both
// halves are here because they fail differently: an unstamped context
// renders an empty page (annoying), while an unscoped resolve revokes a
// stranger's credential (a remote denial of service against their
// account).
//
// The fake adapter derives its rows from the ACTOR ENVELOPE, exactly as
// `passkeysForSelf` (`userId==actor.userId`) does. That fidelity is the
// whole test: a fake that took a userId parameter could not express the
// bug, because the bug is a handler passing the wrong one.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/webauthn"
)

const (
	aliceId        = "v1:identity:user:alice-3409"
	malloryId      = "v1:identity:user:mallory-3409"
	alicePasskeyId = "v1:identity:identity:alice-yubikey"
)

// fakePasskeyAdapter is a PasskeyAdapter whose row set comes from the
// actor on the context, mirroring the self-scoped DSL queries behind the
// real store.
type fakePasskeyAdapter struct {
	rows   map[string][]webauthn.Row
	routes map[string][]webauthn.SignInRoute

	listErr   error
	routesErr error

	renames []renameCall
	revokes []string
}

type renameCall struct {
	ID    string
	Label string
}

// actorOf is the fake's stand-in for `actor.userId`. An unstamped
// context resolves to "" and therefore to zero rows -- the same silent
// emptiness the real query produces, so a handler that forgets
// callerActorCtx fails here for the real reason.
func actorOf(ctx context.Context) string {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return ""
	}
	v, _ := auth.ActorEnvelopeValue(ac, "userId")
	s, _ := v.(string)
	return s
}

func (f *fakePasskeyAdapter) ListForSelf(ctx context.Context) ([]webauthn.Row, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows[actorOf(ctx)], nil
}

func (f *fakePasskeyAdapter) ListSignInRoutesForSelf(ctx context.Context) ([]webauthn.SignInRoute, error) {
	if f.routesErr != nil {
		return nil, f.routesErr
	}
	return f.routes[actorOf(ctx)], nil
}

func (f *fakePasskeyAdapter) Rename(_ context.Context, identityId, label string) error {
	f.renames = append(f.renames, renameCall{ID: identityId, Label: label})
	return nil
}

func (f *fakePasskeyAdapter) Revoke(_ context.Context, identityId string) error {
	f.revokes = append(f.revokes, identityId)
	// SOFT: the row survives with active=false. Modelled here so the
	// handler tests can assert the listing still shows it, which is what
	// "not a hard delete" means from the user's side.
	for owner, rows := range f.rows {
		for i := range rows {
			if rows[i].ID == identityId {
				f.rows[owner][i].Active = false
			}
		}
	}
	return nil
}

// recordingAudit captures emitted audit events.
type recordingAudit struct{ events []identity.AuditEvent }

func (a *recordingAudit) Log(_ context.Context, ev identity.AuditEvent) {
	a.events = append(a.events, ev)
}

func (a *recordingAudit) find(action string) (identity.AuditEvent, bool) {
	for _, ev := range a.events {
		if ev.Action == action {
			return ev, true
		}
	}
	return identity.AuditEvent{}, false
}

// passkeyServer wires a Server with the passkey adapter, a real issuer,
// and a recording audit sink, plus a helper that signs a request in as a
// given user.
func passkeyServer(t *testing.T, adapter *fakePasskeyAdapter) (*Server, *recordingAudit, func(*http.Request, string)) {
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
	iss, err := identity.NewJWTIssuer(km, cfg)
	if err != nil {
		t.Fatalf("NewJWTIssuer: %v", err)
	}

	audit := &recordingAudit{}
	s := &Server{Cfg: cfg, Logger: slog.Default()}
	s.SetMePasskeys(&MePasskeys{Adapter: adapter, Issuer: iss, Audit: audit})

	signIn := func(r *http.Request, userId string) {
		tok, _, err := iss.IssueAccessToken(identity.IssueInput{
			UserId: userId,
			Email:  userId + "@example.com",
			Role:   "writer",
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		r.AddCookie(&http.Cookie{Name: "memql_admin", Value: tok})
	}
	return s, audit, signIn
}

// postForm builds a signed-in form POST.
func postForm(t *testing.T, signIn func(*http.Request, string), userId, path string, form url.Values) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signIn(r, userId)
	return r
}

// aliceOwnsOnePasskey is the fixture the cross-user tests share: Alice
// has one passkey and a magic-link route; Mallory has neither.
func aliceOwnsOnePasskey() *fakePasskeyAdapter {
	return &fakePasskeyAdapter{
		rows: map[string][]webauthn.Row{
			aliceId: {{
				ID:     alicePasskeyId,
				UserId: aliceId,
				Label:  "Alice's YubiKey",
				Active: true,
			}},
		},
		routes: map[string][]webauthn.SignInRoute{
			aliceId: {
				{ID: alicePasskeyId, UserId: aliceId, IdentityType: "passkey", Active: true},
				{ID: "v1:identity:identity:alice-magic", UserId: aliceId, IdentityType: "magic_link", Active: true},
			},
		},
	}
}

// TestMallorySeesNoneOfAlicesPasskeys is the "cannot SEE" half. Mallory
// is fully authenticated -- this is not an auth bypass -- and the page
// must simply contain nothing of Alice's.
func TestMallorySeesNoneOfAlicesPasskeys(t *testing.T) {
	adapter := aliceOwnsOnePasskey()
	s, _, signIn := passkeyServer(t, adapter)

	r := httptest.NewRequest(http.MethodGet, "/me/devices", nil)
	signIn(r, malloryId)
	rec := httptest.NewRecorder()
	s.handleMeDevices(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/devices as Mallory: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "Alice's YubiKey") || strings.Contains(body, alicePasskeyId) {
		t.Errorf("Mallory's /me/devices page carries Alice's credential. The listing must derive "+
			"from userId==actor.userId and nothing else.\nbody=%s", body)
	}

	// And the control: Alice's own page DOES show it, so the assertion
	// above is not passing because the page renders nothing at all.
	r2 := httptest.NewRequest(http.MethodGet, "/me/devices", nil)
	signIn(r2, aliceId)
	rec2 := httptest.NewRecorder()
	s.handleMeDevices(rec2, r2)
	if !strings.Contains(rec2.Body.String(), "Alice&#39;s YubiKey") &&
		!strings.Contains(rec2.Body.String(), "Alice's YubiKey") {
		t.Errorf("Alice's own page does not list her passkey -- the cross-user assertion above "+
			"proves nothing until this passes.\nbody=%s", rec2.Body.String())
	}
}

// TestMalloryCannotRevokeAlicesPasskey is the "cannot REVOKE" half, and
// the sharper one: naming another user's credential id in a form post
// must reach no write at all.
func TestMalloryCannotRevokeAlicesPasskey(t *testing.T) {
	adapter := aliceOwnsOnePasskey()
	s, audit, signIn := passkeyServer(t, adapter)

	form := url.Values{"id": {alicePasskeyId}, "confirm": {"yes"}}
	r := postForm(t, signIn, malloryId, "/me/devices/passkeys/revoke", form)
	rec := httptest.NewRecorder()
	s.handleMePasskeysRevoke(rec, r)

	if len(adapter.revokes) != 0 {
		t.Fatalf("Mallory revoked %v -- a caller-supplied id reached the write without being "+
			"resolved out of the CALLER's own row set", adapter.revokes)
	}
	if adapter.rows[aliceId][0].Active != true {
		t.Error("Alice's passkey was deactivated by Mallory's request")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status %d, want 303 back to /me/devices with a refusal flash", rec.Code)
	}
	ev, ok := audit.find("passkey_revoke_blocked")
	if !ok {
		t.Fatalf("no passkey_revoke_blocked audit event; got %+v", audit.events)
	}
	if ev.Outcome != identity.AuditOutcomeBlocked || ev.FailureReason != "not_owner" {
		t.Errorf("blocked event = outcome %q reason %q, want blocked/not_owner", ev.Outcome, ev.FailureReason)
	}
	if ev.ActorUserId != malloryId {
		t.Errorf("blocked event attributed to %q, want the caller %q", ev.ActorUserId, malloryId)
	}
}

// TestMalloryCannotRenameAlicesPasskey closes the same door on the other
// write. Rename is the less alarming of the two and is tested for
// exactly that reason: it is the one somebody would be tempted to leave
// ungated because "it only changes a label" -- on a row that is not
// theirs.
func TestMalloryCannotRenameAlicesPasskey(t *testing.T) {
	adapter := aliceOwnsOnePasskey()
	s, audit, signIn := passkeyServer(t, adapter)

	form := url.Values{"id": {alicePasskeyId}, "label": {"pwned"}}
	r := postForm(t, signIn, malloryId, "/me/devices/passkeys/rename", form)
	rec := httptest.NewRecorder()
	s.handleMePasskeysRename(rec, r)

	if len(adapter.renames) != 0 {
		t.Fatalf("Mallory renamed %v", adapter.renames)
	}
	if _, ok := audit.find("passkey_rename_blocked"); !ok {
		t.Errorf("no passkey_rename_blocked audit event; got %+v", audit.events)
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status %d, want 303", rec.Code)
	}
}

// TestPasskeyPageStampsTheCallerActor is the memql#3178 assertion for
// this page, kept as a direct check rather than only as a consequence.
// Every identity web route is wrapped in SystemActorMiddleware, which
// stamps claims and a TokenInfo but NO AccessContext -- and actor.userId
// resolves from the AccessContext alone. Without callerActorCtx the
// listing is empty for everyone, which is a bug that reads like "you
// have no passkeys" rather than like a failure.
func TestPasskeyPageStampsTheCallerActor(t *testing.T) {
	var wrapped *http.Request
	identity.SystemActorMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		wrapped = r
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/me/devices", nil))
	if wrapped == nil {
		t.Fatal("middleware did not call through")
	}
	if ac, ok := auth.AccessFromContext(wrapped.Context()); ok && ac != nil && ac.UserId != "" {
		t.Fatalf("precondition changed: the request already carries an AccessContext for %q", ac.UserId)
	}
	if got := actorOf(callerActorCtx(wrapped, claimsFor(aliceId))); got != aliceId {
		t.Errorf("actor.userId = %q, want %q -- passkeysForSelf keys on this", got, aliceId)
	}
}
