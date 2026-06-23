package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/refresh"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// refreshFakeEngine answers the queries/mutations the refresh-token
// rotation path drives: it serves a single auth-session row keyed by its
// current refreshTokenHash, records the rotation, and rotates the stored
// hash forward so a replay of the old token misses (mirroring the real
// store's behaviour). The user lookup returns a minimal directory row.
type refreshFakeEngine struct {
	mu sync.Mutex

	// curHash is the hash currently accepted by
	// authSessionByRefreshTokenHash. It moves forward on every
	// rotateAuthSession call, so the previous token stops
	// resolving -- exactly what proves the old refresh token is
	// invalidated.
	curHash    string
	sessionId  string
	userId     string
	rotations  int
	lastNewHsh string
}

func (f *refreshFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case strings.HasPrefix(q, "authSessionByRefreshTokenHash("):
		presented := extractField(q, "refreshTokenHash")
		if presented != f.curHash || f.curHash == "" {
			// No match -> empty bundle. The rotator then tries the
			// previous-hash grace path (also empty here) and returns
			// ErrSessionNotFound.
			return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
		}
		node := &memqlv1.MemoryNode{
			Id: f.sessionId,
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"id":               structpb.NewStringValue(f.sessionId),
				"userId":           structpb.NewStringValue(f.userId),
				"refreshTokenHash": structpb.NewStringValue(f.curHash),
			}},
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{node}}}, nil

	case strings.HasPrefix(q, "authSessionByPreviousRefreshTokenHash("):
		// No grace-window row in these tests.
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil

	case strings.HasPrefix(q, "rotateAuthSession("):
		f.rotations++
		newHash := extractField(q, "newRefreshTokenHash")
		f.lastNewHsh = newHash
		f.curHash = newHash // old token no longer resolves
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil

	case strings.HasPrefix(q, "userById("):
		node := &memqlv1.MemoryNode{
			Id: f.userId,
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"id":           structpb.NewStringValue(f.userId),
				"primaryEmail": structpb.NewStringValue("owner@example.com"),
				"role":         structpb.NewStringValue("owner"),
			}},
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{node}}}, nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

// newRefreshGrantTestServer wires a Server whose token endpoint can
// process the refresh_token grant: a fake engine seeded with one session
// row and a real JWT issuer (temp keys) so the minted access token is a
// genuine signed JWT.
func newRefreshGrantTestServer(t *testing.T) (*Server, *refreshFakeEngine, string) {
	t.Helper()

	plain, hash, err := refresh.NewRefreshToken()
	if err != nil {
		t.Fatalf("mint refresh token: %v", err)
	}
	eng := &refreshFakeEngine{
		curHash:   hash,
		sessionId: "v1:identity:authSession:sess-1",
		userId:    "v1:identity:user:owner-1",
	}
	store := &identity.Store{Engine: eng}

	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	if err != nil {
		t.Fatalf("new key manager: %v", err)
	}
	if err := km.Load(); err != nil {
		t.Fatalf("load keys: %v", err)
	}
	cfg := identity.Config{
		Enabled:     true,
		BaseURL:     "https://identity.test",
		JWTAudience: "memql",
		KeyDir:      dir,
	}
	iss, err := identity.NewJWTIssuer(km, cfg)
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}

	s := &Server{
		Cfg:   cfg,
		Store: store,
		Rotator: &refresh.Rotator{
			Cfg:    cfg,
			Store:  store,
			Issuer: iss,
		},
	}
	return s, eng, plain
}

// postTokenForm posts an x-www-form-urlencoded body to /oauth/token,
// the OAuth-canonical shape a stock client (claude.ai) uses.
func postTokenForm(t *testing.T, s *Server, form string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleToken(rec, req)
	return rec
}

func TestHandleToken_RefreshGrant_HappyPath(t *testing.T) {
	s, eng, refreshTok := newRefreshGrantTestServer(t)

	rec := postTokenForm(t, s, "grant_type=refresh_token&refresh_token="+refreshTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if resp.AccessToken == "" {
		t.Fatalf("expected a fresh access token")
	}
	if resp.TokenType != "Bearer" {
		t.Fatalf("token_type = %q, want Bearer", resp.TokenType)
	}
	if resp.RefreshToken == "" {
		t.Fatalf("expected a rotated refresh token")
	}
	if resp.RefreshToken == refreshTok {
		t.Fatalf("refresh token must rotate; got the same value back")
	}
	if eng.rotations != 1 {
		t.Fatalf("expected exactly one session rotation, got %d", eng.rotations)
	}
	// A refresh cookie must be set so a browser-based OAuth client keeps
	// a live cookie.
	if len(rec.Result().Cookies()) == 0 {
		t.Fatalf("expected a rotated refresh cookie on the response")
	}
}

func TestHandleToken_RefreshGrant_OldTokenInvalidated(t *testing.T) {
	s, _, refreshTok := newRefreshGrantTestServer(t)

	// First refresh succeeds and rotates the stored hash forward.
	if rec := postTokenForm(t, s, "grant_type=refresh_token&refresh_token="+refreshTok); rec.Code != http.StatusOK {
		t.Fatalf("first refresh status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Replaying the ORIGINAL refresh token must now fail -- its hash no
	// longer resolves to a session row (outside the grace window).
	rec := postTokenForm(t, s, "grant_type=refresh_token&refresh_token="+refreshTok)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("replay status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "invalid_grant" {
		t.Fatalf("replay error = %q, want invalid_grant", got)
	}
}

func TestHandleToken_RefreshGrant_MissingToken(t *testing.T) {
	s, _, _ := newRefreshGrantTestServer(t)
	rec := postTokenForm(t, s, "grant_type=refresh_token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "invalid_request" {
		t.Fatalf("error = %q, want invalid_request", got)
	}
}

func TestHandleToken_RefreshGrant_UnknownToken(t *testing.T) {
	s, _, _ := newRefreshGrantTestServer(t)
	rec := postTokenForm(t, s, "grant_type=refresh_token&refresh_token=not-a-real-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "invalid_grant" {
		t.Fatalf("error = %q, want invalid_grant", got)
	}
}

func TestHandleToken_UnsupportedGrant(t *testing.T) {
	s, _, _ := newRefreshGrantTestServer(t)
	rec := postTokenForm(t, s, "grant_type=client_credentials")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "unsupported_grant_type" {
		t.Fatalf("error = %q, want unsupported_grant_type", got)
	}
}
