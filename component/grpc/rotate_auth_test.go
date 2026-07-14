package memql

// Tests for handleRotateAuth -- the in-stream bearer rotation path
// memql-cockpit hits from its background token refresher.
//
// The handler is the load-bearing piece of the "no browser tab, ever"
// UX goal: a refreshed JWT must propagate to the live stream so the
// server's identity / partition-ACL stays aligned with whatever
// session-revocation policy the admin has in place. Regressions here
// would mean revocation only takes effect on the next stream drop,
// which can be minutes after the admin clicks the kill switch.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/verifier"
)

// captureStream is the minimum MemqlService_StreamServer that the
// handlers need to call Send. Recv is never invoked in these tests --
// we feed envelopes directly to handleMessage. SetHeader / SendHeader /
// SetTrailer / Context return the stored values so the handler can
// thread them through (currently it doesn't, but the interface
// requires them).
type captureStream struct {
	ctx  context.Context
	mu   sync.Mutex
	sent []*memqlv1.MemqlServerMessage
}

func newCaptureStream(t *testing.T) *captureStream {
	t.Helper()
	return &captureStream{ctx: context.Background()}
}

func (c *captureStream) Send(msg *memqlv1.MemqlServerMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, msg)
	return nil
}

func (c *captureStream) Recv() (*memqlv1.MemqlClientMessage, error) {
	// Block forever; tests don't drive the read side of the stream.
	<-c.ctx.Done()
	return nil, c.ctx.Err()
}

func (c *captureStream) SetHeader(metadata.MD) error  { return nil }
func (c *captureStream) SendHeader(metadata.MD) error { return nil }
func (c *captureStream) SetTrailer(metadata.MD)       {}
func (c *captureStream) Context() context.Context     { return c.ctx }
func (c *captureStream) SendMsg(any) error            { return nil }
func (c *captureStream) RecvMsg(any) error            { return nil }

func (c *captureStream) lastSent() *memqlv1.MemqlServerMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		return nil
	}
	return c.sent[len(c.sent)-1]
}

// jwksTestHandler serves the live JWKS from a KeyManager. Mirrors the
// helper in component/identity/verifier/verifier_test.go so we don't
// have to import a test-only package.
func jwksTestHandler(km *identity.KeyManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := json.Marshal(identity.BuildJWKS(km, time.Now().UTC()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

// newRotateAuthFixture wires up an identity issuer + JWKS endpoint +
// matching verifier, all backed by a per-test temp dir for keys. The
// returned function mints valid access tokens for the supplied user.
func newRotateAuthFixture(t *testing.T) (
	issue func(user, email, role string) string,
	v *verifier.Verifier,
) {
	issue, _, v = newRotateAuthFixtureWithBadge(t)
	return issue, v
}

// newRotateAuthFixtureWithBadge is newRotateAuthFixture plus a badge
// grant issuer, so tests can prove a rotated-in class="badge" token
// arms the expiry gate end to end (memql#2513).
func newRotateAuthFixtureWithBadge(t *testing.T) (
	issue func(user, email, role string) string,
	issueBadge func(operator, roleCeiling string, ttl time.Duration) string,
	v *verifier.Verifier,
) {
	t.Helper()
	srv := httptest.NewServer(http.NewServeMux())
	t.Cleanup(srv.Close)
	mux := http.NewServeMux()
	srv.Config.Handler = mux

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
		BaseURL:     srv.URL,
		JWTAudience: "memql",
		KeyDir:      dir,
	}
	iss, err := identity.NewJWTIssuer(km, cfg)
	if err != nil {
		t.Fatalf("NewJWTIssuer: %v", err)
	}
	mux.Handle("/.well-known/jwks.json", jwksTestHandler(km))

	vcfg := verifier.Config{
		BaseURL:             srv.URL,
		ExpectedAudience:    "memql",
		JWKSRefreshInterval: time.Hour,
		JWKSFetchTimeout:    5 * time.Second,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cache, err := verifier.NewJWKSCache(vcfg, logger)
	if err != nil {
		t.Fatalf("NewJWKSCache: %v", err)
	}
	v, err = verifier.New(vcfg, cache, nil, logger)
	if err != nil {
		t.Fatalf("verifier.New: %v", err)
	}

	issue = func(user, email, role string) string {
		tok, _, err := iss.IssueAccessToken(identity.IssueInput{
			UserId: user,
			Email:  email,
			Name:   email,
			Role:   role,
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		return tok
	}
	issueBadge = func(operator, roleCeiling string, ttl time.Duration) string {
		tok, _, err := iss.IssueBadgeGrantToken(identity.BadgeGrantIssueInput{
			UserId:      operator,
			RoleCeiling: roleCeiling,
			TTLOverride: ttl,
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("IssueBadgeGrantToken: %v", err)
		}
		return tok
	}
	return issue, issueBadge, v
}

// newSessionForRotate builds a streamSession wired to the supplied
// service. The initial identity is a placeholder ("v1:identity:user:
// before-rotate") so the test can prove the swap actually happens.
// eventChan / closeChan are left nil -- handleRotateAuth never reads
// or writes them, and the zero-valued sendMu is a usable Mutex.
func newSessionForRotate(svc *service) (*streamSession, *captureStream) {
	cs := &captureStream{ctx: context.Background()}
	sess := &streamSession{
		service: svc,
		stream:  cs,
		logger:  svc.logger,
		identity: auth.UserIdentity{
			Subject: "v1:identity:user:before-rotate",
			Email:   "before@example.com",
			Role:    "reader",
		},
		closeChan: make(chan struct{}),
	}
	return sess, cs
}

// TestRotateAuth_NilVerifier proves that a node started without a
// verifier responds with "verifier_unconfigured" and leaves the
// session's identity intact. The stream MUST stay open in this case --
// the client is expected to fall back to reconnect-with-fresh-token.
func TestRotateAuth_NilVerifier(t *testing.T) {
	svc := &service{logger: nil} // no verifier, no identityResolver
	sess, cs := newSessionForRotate(svc)

	err := sess.handleRotateAuth(
		&memqlv1.MemqlClientMessage{MessageId: "req-1"},
		&memqlv1.RotateAuthMsg{AccessToken: "anything"},
	)
	if err != nil {
		t.Fatalf("handleRotateAuth: %v", err)
	}

	reply := cs.lastSent()
	res := reply.GetRotateAuthResult()
	if res == nil {
		t.Fatalf("expected RotateAuthResult payload; got %T", reply.GetPayload())
	}
	if res.GetOk() {
		t.Errorf("expected ok=false when verifier is nil; got ok=true")
	}
	if res.GetError() != "verifier_unconfigured" {
		t.Errorf("expected error=verifier_unconfigured; got %q", res.GetError())
	}
	if sess.identity.Subject != "v1:identity:user:before-rotate" {
		t.Errorf("identity should be unchanged on reject; got %q", sess.identity.Subject)
	}
}

// TestRotateAuth_EmptyToken guards the precondition: a blank
// access_token must be refused with error=invalid_token. Without
// this check the verifier itself would fail later with a less-helpful
// error, and the log would suggest a network issue rather than a
// caller bug.
func TestRotateAuth_EmptyToken(t *testing.T) {
	_, v := newRotateAuthFixture(t)
	svc := &service{verifier: v}
	sess, cs := newSessionForRotate(svc)

	err := sess.handleRotateAuth(
		&memqlv1.MemqlClientMessage{MessageId: "req-2"},
		&memqlv1.RotateAuthMsg{AccessToken: "   "},
	)
	if err != nil {
		t.Fatalf("handleRotateAuth: %v", err)
	}
	res := cs.lastSent().GetRotateAuthResult()
	if res == nil || res.GetOk() {
		t.Fatalf("expected ok=false; got %+v", res)
	}
	if res.GetError() != "invalid_token" {
		t.Errorf("expected error=invalid_token; got %q", res.GetError())
	}
}

// TestRotateAuth_HappyPath is the load-bearing assertion: a freshly-
// minted JWT for a different user swaps the session's identity in
// place. Both the swap AND the success reply are required for the
// cockpit's refresher to consider rotation effective.
func TestRotateAuth_HappyPath(t *testing.T) {
	issue, v := newRotateAuthFixture(t)
	svc := &service{
		verifier: v,
		// identityResolver left nil so the handler falls into the
		// FallbackFromClaims branch (no DB needed). The contract under
		// test is the identity swap + the reply shape.
		identityResolver: nil,
	}
	sess, cs := newSessionForRotate(svc)

	newToken := issue("v1:identity:user:after-rotate", "after@example.com", "admin")

	err := sess.handleRotateAuth(
		&memqlv1.MemqlClientMessage{MessageId: "req-3"},
		&memqlv1.RotateAuthMsg{AccessToken: newToken},
	)
	if err != nil {
		t.Fatalf("handleRotateAuth: %v", err)
	}
	res := cs.lastSent().GetRotateAuthResult()
	if res == nil || !res.GetOk() {
		t.Fatalf("expected ok=true; got %+v", res)
	}
	if sess.identity.Subject != "v1:identity:user:after-rotate" {
		t.Errorf("identity not swapped; subject still %q", sess.identity.Subject)
	}
	if sess.identity.Email != "after@example.com" {
		t.Errorf("identity email not swapped; got %q", sess.identity.Email)
	}
	if sess.identity.Role != "admin" {
		t.Errorf("identity role not swapped; got %q", sess.identity.Role)
	}
	// Access cache must be populated so the next message doesn't
	// re-trigger the full ensureAccess load path against stale claims.
	if !sess.accessLoaded {
		t.Errorf("accessLoaded should be true after a successful rotate")
	}
	if sess.access == nil {
		t.Errorf("access should be non-nil after a successful rotate")
	}
}

// TestRotateAuth_InvalidJWTKeepsOldIdentity exercises the negative
// path the cockpit will hit if it ever sends a token the server's
// verifier can't validate (clock skew, wrong audience, signature
// from a key the JWKS doesn't have). The session's identity must
// stay as it was -- we never want to half-rotate to an unknown
// subject.
func TestRotateAuth_InvalidJWTKeepsOldIdentity(t *testing.T) {
	_, v := newRotateAuthFixture(t)
	svc := &service{verifier: v}
	sess, cs := newSessionForRotate(svc)

	err := sess.handleRotateAuth(
		&memqlv1.MemqlClientMessage{MessageId: "req-4"},
		&memqlv1.RotateAuthMsg{AccessToken: "definitely.not.a.jwt"},
	)
	if err != nil {
		t.Fatalf("handleRotateAuth: %v", err)
	}
	res := cs.lastSent().GetRotateAuthResult()
	if res == nil || res.GetOk() {
		t.Fatalf("expected ok=false on a garbage token; got %+v", res)
	}
	if res.GetError() != "invalid_token" {
		t.Errorf("expected error=invalid_token; got %q", res.GetError())
	}
	if sess.identity.Subject != "v1:identity:user:before-rotate" {
		t.Errorf("identity changed on reject; subject is now %q", sess.identity.Subject)
	}
}

// TestRotateAuth_BadgeGrantArmsExpiryGate proves the full memql#2513
// path through the real verifier: a kiosk stream (user-class) rotates
// in a class="badge" grant, the badge expiry gate arms from the
// verified claims (NOT the lazy stream-open stamp), and once the grant
// expires ordinary work is rejected with badge_grant_expired while
// RotateAuth stays admitted so the client can rotate back.
func TestRotateAuth_BadgeGrantArmsExpiryGate(t *testing.T) {
	_, issueBadge, v := newRotateAuthFixtureWithBadge(t)
	svc := &service{verifier: v, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sess, cs := newSessionForRotate(svc)

	// A ~1s grant so the test observes both the live and expired states
	// without sleeping long.
	grant := issueBadge("v1:identity:user:operator-9", "writer", 1200*time.Millisecond)
	if err := sess.handleRotateAuth(
		&memqlv1.MemqlClientMessage{MessageId: "rot-badge"},
		&memqlv1.RotateAuthMsg{AccessToken: grant},
	); err != nil {
		t.Fatalf("handleRotateAuth: %v", err)
	}
	if res := cs.lastSent().GetRotateAuthResult(); res == nil || !res.GetOk() {
		t.Fatalf("badge rotate should succeed; got %+v", res)
	}
	if sess.identity.Subject != "v1:identity:user:operator-9" {
		t.Fatalf("identity not swapped to operator; got %q", sess.identity.Subject)
	}

	q := &memqlv1.MemqlClientMessage{
		MessageId: "q-live",
		Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
			ExecuteQuery: &memqlv1.ExecuteQueryMsg{RequestId: "q-live", Query: "concept==v1:demo"},
		},
	}
	// While live, the gate admits work (it fails later for lack of an
	// engine, but NOT with the gate's typed rejection).
	if v := sess.badgeGate(q); v != badgeGateAllow {
		t.Fatalf("live badge grant should allow ordinary work; gate verdict=%v", v)
	}

	// After expiry the gate rejects work but still admits RotateAuth.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sess.badgeGate(q) == badgeGateExpired {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if v := sess.badgeGate(q); v != badgeGateExpired {
		t.Fatalf("expired badge grant should reject work; gate verdict=%v", v)
	}
	rotateBack := &memqlv1.MemqlClientMessage{
		MessageId: "rot-back",
		Payload:   &memqlv1.MemqlClientMessage_RotateAuth{RotateAuth: &memqlv1.RotateAuthMsg{}},
	}
	if v := sess.badgeGate(rotateBack); v != badgeGateAllow {
		t.Fatalf("RotateAuth must stay admitted on an expired grant so the client can recover; verdict=%v", v)
	}
}
