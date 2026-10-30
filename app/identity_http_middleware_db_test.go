package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/verifier"
	"github.com/znasllc-io/memql/component/memql"
)

// THE REVOCATION CHECK IS ONLY AS GOOD AS ITS PLACE IN THE CHAIN. It reads a
// session for a request carrying verified claims, so it enforces nothing unless
// the verifier ran first. identityHTTPMiddleware is what makes that true: it
// builds the two as one, with no second append for a later change to reorder.
// This drives that exact middleware, with the node's own path declarations.
func TestIdentityHTTPMiddlewareChecksRevocationBehindTheVerifier(t *testing.T) {
	ctx := context.Background()
	if available, err := dbtest.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	} else if !available {
		dbtest.Unreachable(t, "the node's revocation wiring", dbtest.DSN(), nil)
		return
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	engine, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	engine.Logger = logger
	if err := engine.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	reads := &revocationReads{}
	db.AddQueryHook(reads)

	issue, v := revocationVerifier(t, logger)
	node := &App{Logger: logger, engine: engine, identityVerifier: v}
	handler := node.identityHTTPMiddleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	do := func(path, token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	if got := do("/healthz", "not-a-token").Code; got != http.StatusOK {
		t.Fatalf("a junk bearer on a public path: status %d, want 200", got)
	}
	if got := reads.take(); got != 0 {
		t.Fatalf("a junk bearer on a public path cost %d session reads, want none", got)
	}
	if got := do("/memql/query", "not-a-token").Code; got != http.StatusUnauthorized {
		t.Fatalf("a junk bearer on a gated path: status %d, want 401", got)
	}
	if got := reads.take(); got != 0 {
		t.Fatalf("a junk bearer on a gated path cost %d session reads, want none", got)
	}

	user := fmt.Sprintf("v1:identity:user:wiring-%d", time.Now().UnixNano())
	token := issue(user)
	system := identity.ContextWithSystemActor(ctx)
	if err := memqlgrpc.CreateAuthSessionRow(system, engine, memqlgrpc.AuthSessionInsert{
		SessionId: fmt.Sprintf("wiring-%d", time.Now().UnixNano()),
		UserId:    user,
		Subject:   user,
		TokenHash: memqlgrpc.HashBearerToken(token),
		Source:    "bff_exchange",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if got := do("/memql/query", token).Code; got != http.StatusOK {
		t.Fatalf("a live session on a gated path: status %d, want 200", got)
	}
	session, err := memqlgrpc.LookupAuthSessionByTokenHash(system, engine, memqlgrpc.HashBearerToken(token))
	if err != nil || session == nil {
		t.Fatalf("the session must be found by its token hash: %v %v", session, err)
	}
	if err := memqlgrpc.RevokeAuthSessionRow(system, engine, session, "user_action"); err != nil {
		t.Fatal(err)
	}
	refused := do("/memql/query", token)
	if refused.Code != http.StatusUnauthorized || !strings.Contains(refused.Body.String(), "session revoked") {
		t.Fatalf("a revoked session on a gated path: status %d body %q, want 401 session revoked", refused.Code, refused.Body)
	}
}

type revocationReads struct {
	mu    sync.Mutex
	count int
}

func (h *revocationReads) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *revocationReads) AfterQuery(_ context.Context, event *bun.QueryEvent) {
	if strings.HasPrefix(event.Query, "SELECT") && strings.Contains(event.Query, "tokenHash") &&
		strings.Contains(event.Query, "v1:identity:authSession") {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.count++
	}
}

func (h *revocationReads) take() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := h.count
	h.count = 0
	return count
}

// revocationVerifier is the real verifier over a real signing key, and a mint for
// bearers it accepts.
func revocationVerifier(t *testing.T, logger *slog.Logger) (func(user string) string, *verifier.Verifier) {
	t.Helper()
	dir := t.TempDir()
	keys, err := identity.NewKeyManager(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.Load(); err != nil {
		t.Fatal(err)
	}
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(identity.BuildJWKS(keys, time.Now().UTC()))
	}))
	t.Cleanup(feed.Close)
	issuer, err := identity.NewJWTIssuer(keys, identity.Config{Enabled: true, BaseURL: feed.URL, JWTAudience: "memql", KeyDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	config := verifier.Config{BaseURL: feed.URL, ExpectedAudience: "memql", JWKSRefreshInterval: time.Hour, JWKSFetchTimeout: 5 * time.Second}
	cache, err := verifier.NewJWKSCache(config, logger)
	if err != nil {
		t.Fatal(err)
	}
	v, err := verifier.New(config, cache, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	return func(user string) string {
		token, _, err := issuer.IssueAccessToken(identity.IssueInput{UserId: user, Email: "wiring@example.test", Name: "wiring", Role: string(auth.RoleReader)}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		return token
	}, v
}
