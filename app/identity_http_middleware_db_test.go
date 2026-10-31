package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"google.golang.org/grpc"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
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
	f := newRevocationNode(t)
	handler := f.node.identityHTTPMiddleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
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
	if got := f.reads.take(); got != 0 {
		t.Fatalf("a junk bearer on a public path cost %d session reads, want none", got)
	}
	if got := do("/memql/query", "not-a-token").Code; got != http.StatusUnauthorized {
		t.Fatalf("a junk bearer on a gated path: status %d, want 401", got)
	}
	if got := f.reads.take(); got != 0 {
		t.Fatalf("a junk bearer on a gated path cost %d session reads, want none", got)
	}

	token := f.liveSession(t)
	if got := do("/memql/query", token).Code; got != http.StatusOK {
		t.Fatalf("a live session on a gated path: status %d, want 200", got)
	}
	f.revoke(t, token)
	refused := do("/memql/query", token)
	if refused.Code != http.StatusUnauthorized || !strings.Contains(refused.Body.String(), "session revoked") {
		t.Fatalf("a revoked session on a gated path: status %d body %q, want 401 session revoked", refused.Code, refused.Body)
	}
}

// A PUBLIC PREFIX REACHES THE GATEWAY, AND ONLY THE STREAM CHAIN REFUSES THERE.
//
// The verifier matches public paths by prefix and the gateway claims any POST
// ending in /memql/query, so POST /healthz/memql/query skips the verifier and
// still forwards its bearer to MemqlService.Stream. With no claims on the request
// the HTTP check steps aside, which leaves the stream chain as the only thing
// between a revoked session and a query. So this drives the node's own HTTP
// middleware and its own stream chain, over a real gRPC connection, and would
// fail if a later change dropped the stream check because the HTTP one looks
// sufficient.
func TestARevokedSessionIsRefusedThroughTheGatewayOnAPublicPrefix(t *testing.T) {
	f := newRevocationNode(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	opened := &atomic.Int32{}
	server := grpc.NewServer(grpc.StreamInterceptor(f.node.verifiedStreamChain()))
	memqlv1.RegisterMemqlServiceServer(server, &gatewayHopService{opened: opened})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	gateway, err := memqlgrpc.NewGateway(memqlgrpc.GatewayOptions{Logger: f.node.Logger, Endpoint: listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gateway.Close)
	handler := f.node.identityHTTPMiddleware()(gateway.Middleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the gateway did not claim the path, so this no longer tests the hop it names")
	})))
	post := func(token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/healthz"+memqlgrpc.GatewayPath, strings.NewReader(`{"query":"query anything()"}`))
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	// The control: this spelling really does skip the verifier's refusal and open
	// a stream, for a session nothing is wrong with.
	token := f.liveSession(t)
	if got := post(token).Code; got != http.StatusOK || opened.Load() != 1 {
		t.Fatalf("a live session through the gateway: status %d, %d streams opened; want 200 and 1", got, opened.Load())
	}

	f.revoke(t, token)
	f.reads.take()
	// Not asserted to a status: a refusal at stream open can reach the gateway
	// through its first send as a bare EOF, which it answers as a 500. What matters
	// is that no stream opened.
	refused := post(token)
	if opened.Load() != 1 {
		t.Fatalf("a revoked session opened a stream through the gateway: status %d body %q", refused.Code, refused.Body)
	}
	if refused.Code == http.StatusOK {
		t.Fatalf("a revoked session through the gateway got a 200: %q", refused.Body)
	}
	if got := f.reads.take(); got != 1 {
		t.Fatalf("the refusal cost %d session reads, want the one at stream open", got)
	}
}

// gatewayHopService counts the streams the chain admitted and answers any query
// with an empty result.
type gatewayHopService struct {
	memqlv1.UnimplementedMemqlServiceServer
	opened *atomic.Int32
}

func (s *gatewayHopService) Stream(stream memqlv1.MemqlService_StreamServer) error {
	s.opened.Add(1)
	for {
		message, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if query := message.GetExecuteQuery(); query != nil {
			return stream.Send(&memqlv1.MemqlServerMessage{Payload: &memqlv1.MemqlServerMessage_QueryResult{
				QueryResult: &memqlv1.QueryResultChunk{RequestId: query.GetRequestId(), Result: &memqlv1.Result{}, Done: true},
			}})
		}
	}
}

// revocationNode is an App holding what the revocation wiring reads: a real
// verifier over a real signing key, and a real engine over the test database.
type revocationNode struct {
	node  *App
	reads *revocationReads
	issue func(user string) string
}

func newRevocationNode(t *testing.T) *revocationNode {
	t.Helper()
	ctx := context.Background()
	if available, err := dbtest.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	} else if !available {
		dbtest.Unreachable(t, "the node's revocation wiring", dbtest.DSN(), nil)
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
	return &revocationNode{node: &App{Logger: logger, engine: engine, identityVerifier: v}, reads: reads, issue: issue}
}

func (f *revocationNode) liveSession(t *testing.T) string {
	t.Helper()
	user := fmt.Sprintf("v1:identity:user:wiring-%d", time.Now().UnixNano())
	token := f.issue(user)
	if err := memqlgrpc.CreateAuthSessionRow(identity.ContextWithSystemActor(context.Background()), f.node.engine, memqlgrpc.AuthSessionInsert{
		SessionId: fmt.Sprintf("wiring-%d", time.Now().UnixNano()),
		UserId:    user,
		Subject:   user,
		TokenHash: memqlgrpc.HashBearerToken(token),
		Source:    "bff_exchange",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

func (f *revocationNode) revoke(t *testing.T, token string) {
	t.Helper()
	system := identity.ContextWithSystemActor(context.Background())
	session, err := memqlgrpc.LookupAuthSessionByTokenHash(system, f.node.engine, memqlgrpc.HashBearerToken(token))
	if err != nil || session == nil {
		t.Fatalf("the session must be found by its token hash: %v %v", session, err)
	}
	if err := memqlgrpc.RevokeAuthSessionRow(system, f.node.engine, session, "user_action"); err != nil {
		t.Fatal(err)
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
