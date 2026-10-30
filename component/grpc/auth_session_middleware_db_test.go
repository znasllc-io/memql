package memql

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/verifier"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// THE REVOCATION CHECK READS A SESSION ONLY FOR A BEARER THE VERIFIER ACCEPTED.
//
// It used to hash and look up whatever bearer it found, before anything had
// checked it. The verifier steps aside on a public or self-authenticated path,
// so `Authorization: Bearer x` on /healthz ran authSessionByTokenHash, which had
// no index: 1.1 s of database time at 120,000 session rows, for a request with
// no credential at all. `?token=` is a carrier, so every unsubscribe and email
// link click paid the same.
//
// These drive the real verifier over a real engine over a real database, so what
// they count is the SQL the engine really runs. Postgres-gated: openWireTestDB
// skips with no database, and the db-tests lane makes that skip a failure.

// sessionReads counts the statements that compare a session's tokenHash, which
// is what the revocation check costs and nothing else on the path does.
type sessionReads struct {
	mu    sync.Mutex
	count int
}

func (h *sessionReads) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *sessionReads) AfterQuery(_ context.Context, event *bun.QueryEvent) {
	if !strings.HasPrefix(event.Query, "SELECT") || !strings.Contains(event.Query, "tokenHash") ||
		!strings.Contains(event.Query, "v1:identity:authSession") {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
}

func (h *sessionReads) take() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := h.count
	h.count = 0
	return count
}

type revocationFixture struct {
	engine   *memqlengine.MemQLEngine
	reads    *sessionReads
	verifier *verifier.Verifier
	issue    func(user, email, role string) string
	logger   *slog.Logger
}

func newRevocationFixture(t *testing.T) *revocationFixture {
	t.Helper()
	db := openWireTestDB(t)
	_, err := memqlengine.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	engine, err := memqlengine.New(db)
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	engine.Logger = logger
	require.NoError(t, engine.Init(concept.DefaultRegistry()))
	reads := &sessionReads{}
	db.AddQueryHook(reads)
	issue, v := newRotateAuthFixture(t)
	return &revocationFixture{engine: engine, reads: reads, verifier: v, issue: issue, logger: logger}
}

// newToken is a verifiable bearer for a fresh reader, the least privileged role:
// the check runs under the caller's own context, so it has to find a session row
// for the weakest caller there is. The second result is the user it belongs to.
func (f *revocationFixture) newToken(t *testing.T) (string, string) {
	t.Helper()
	user := "v1:identity:user:revocation-" + id.NewShortId()
	return f.issue(user, "revocation@example.test", string(auth.RoleReader)), user
}

func (f *revocationFixture) liveSession(t *testing.T) string {
	t.Helper()
	token, user := f.newToken(t)
	require.NoError(t, CreateAuthSessionRow(contextWithSystemActor(context.Background()), f.engine, AuthSessionInsert{
		SessionId: "revocation-" + id.NewShortId(),
		UserId:    user,
		Subject:   user,
		TokenHash: HashBearerToken(token),
		Source:    "bff_exchange",
		ExpiresAt: time.Now().Add(time.Hour),
	}))
	f.reads.take()
	return token
}

func (f *revocationFixture) revokedSession(t *testing.T) string {
	t.Helper()
	token := f.liveSession(t)
	system := contextWithSystemActor(context.Background())
	session, err := LookupAuthSessionByTokenHash(system, f.engine, HashBearerToken(token))
	require.NoError(t, err)
	require.NotNil(t, session, "the session just created must be found by its token hash")
	require.NoError(t, RevokeAuthSessionRow(system, f.engine, session, "user_action"))
	f.reads.take()
	return token
}

// httpChain is the order the node installs them in: the verifier, then this.
func (f *revocationFixture) httpChain(next http.Handler) http.Handler {
	return verifier.HTTPMiddleware(f.verifier, verifier.MiddlewareOptions{
		Logger:                 f.logger,
		PublicPaths:            []string{"/healthz"},
		SelfAuthenticatedPaths: []string{"/inbound/"},
	})(NewSessionRevocationHTTPMiddleware(f.engine, f.logger)(next))
}

// bearerCarriers is every way a request can present a bearer, the set
// extractBearerTokenHTTP walks.
var bearerCarriers = []struct {
	name  string
	carry func(*http.Request, string)
}{
	{"authorization header", func(r *http.Request, token string) { r.Header.Set("Authorization", "Bearer "+token) }},
	{"websocket subprotocol", func(r *http.Request, token string) { r.Header.Set("Sec-WebSocket-Protocol", "bearer, "+token) }},
	{"auth cookie", func(r *http.Request, token string) { r.AddCookie(&http.Cookie{Name: authCookieName, Value: token}) }},
	{"query parameter", func(r *http.Request, token string) {
		query := r.URL.Query()
		query.Set("token", token)
		r.URL.RawQuery = query.Encode()
	}},
}

// serve runs one request through chain and reports its status, its body, and
// whether the handler behind the chain ran and with what claims.
func serve(chain func(http.Handler) http.Handler, path, token string, carry func(*http.Request, string)) (int, string, bool, map[string]any) {
	var reached bool
	var claims map[string]any
	handler := chain(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		claims, _ = auth.ClaimsFromContext(r.Context())
	}))
	request := httptest.NewRequest(http.MethodGet, path, nil)
	carry(request, token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String(), reached, claims
}

func TestSessionRevocationReadsNothingForABearerNobodyVerified(t *testing.T) {
	f := newRevocationFixture(t)
	for _, path := range []struct {
		name, path string
		status     int
		reached    bool
	}{
		{"public path", "/healthz", http.StatusOK, true},
		{"self-authenticated path", "/inbound/shopify", http.StatusOK, true},
		{"gated path", "/memql/query", http.StatusUnauthorized, false},
	} {
		for _, carrier := range bearerCarriers {
			t.Run(path.name+"/"+carrier.name, func(t *testing.T) {
				code, _, reached, claims := serve(f.httpChain, path.path, "not-a-token", carrier.carry)
				require.Equal(t, path.status, code)
				require.Equal(t, path.reached, reached)
				require.Empty(t, claims, "nothing verified this bearer, so nothing may present it as an identity")
				require.Zero(t, f.reads.take(), "a bearer that no verifier accepted must not cost a session read")
			})
		}
	}
}

// A signed token on a path the verifier skips is no different: nobody verified
// it, the handler gets no claims from it, and a public path answers everyone. So
// a revoked one is served there too, and that is deliberate. It used to be
// refused, so a browser still holding a revoked cookie got a 401 from a public
// path; nothing on those paths acts on the credential.
func TestSessionRevocationLeavesAPathTheVerifierSkipped(t *testing.T) {
	f := newRevocationFixture(t)
	for _, carrier := range bearerCarriers {
		t.Run(carrier.name, func(t *testing.T) {
			code, _, reached, claims := serve(f.httpChain, "/healthz", f.revokedSession(t), carrier.carry)
			require.Equal(t, http.StatusOK, code)
			require.True(t, reached)
			require.Empty(t, claims)
			require.Zero(t, f.reads.take())
		})
	}
}

func TestSessionRevocationRefusesARevokedSessionOnEveryCarrier(t *testing.T) {
	f := newRevocationFixture(t)
	for _, carrier := range bearerCarriers {
		t.Run(carrier.name, func(t *testing.T) {
			code, body, reached, _ := serve(f.httpChain, "/memql/query", f.revokedSession(t), carrier.carry)
			require.Equal(t, http.StatusUnauthorized, code)
			require.Contains(t, body, "session revoked", "refused by the revocation check, not by the verifier")
			require.False(t, reached)
			require.Equal(t, 1, f.reads.take())

			code, _, reached, claims := serve(f.httpChain, "/memql/query", f.liveSession(t), carrier.carry)
			require.Equal(t, http.StatusOK, code)
			require.True(t, reached)
			require.NotEmpty(t, claims)
			require.Equal(t, 1, f.reads.take())
		})
	}

	// A verified credential with no session row has nothing to revoke.
	t.Run("verified bearer with no session row", func(t *testing.T) {
		token, _ := f.newToken(t)
		code, _, reached, _ := serve(f.httpChain, "/memql/query", token, bearerCarriers[0].carry)
		require.Equal(t, http.StatusOK, code)
		require.True(t, reached)
		require.Equal(t, 1, f.reads.take())
	})
}

// narrowDelegation answers every subject with an active delegation of the least
// authority one can carry, which the stream verifier stamps on the context it
// hands on. The node installs a real resolver in the same place.
type narrowDelegation struct{}

func (narrowDelegation) ResolveActiveDelegation(_ context.Context, subject string) (*auth.DelegationContext, error) {
	return &auth.DelegationContext{
		DelegationId: "v1:identity:delegation:narrow",
		IdentityId:   subject,
		IdentityType: "human",
		AgentId:      "v1:agents:agent:narrow",
		RoleCeiling:  auth.RoleReader,
		Scopes:       []string{"nothing.that.reads.a.session"},
	}, nil
}

func TestSessionRevocationStreamVerifiesBeforeItReads(t *testing.T) {
	f := newRevocationFixture(t)
	opener := func(base grpc.StreamServerInterceptor) func(*testing.T, string) (error, bool, map[string]any) {
		interceptor := NewSessionRevocationStreamInterceptor(base, f.engine, f.logger)
		return func(t *testing.T, token string) (error, bool, map[string]any) {
			t.Helper()
			stream := newCaptureStream(t)
			stream.ctx = metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
			var reached bool
			var claims map[string]any
			err := interceptor(nil, stream, &grpc.StreamServerInfo{FullMethod: "/revocation.test/Stream"}, func(_ any, verified grpc.ServerStream) error {
				reached = true
				claims, _ = auth.ClaimsFromContext(verified.Context())
				return nil
			})
			return err, reached, claims
		}
	}
	open := opener(verifier.StreamInterceptor(f.verifier, f.logger))

	t.Run("a bearer the verifier refuses costs no read", func(t *testing.T) {
		err, reached, _ := open(t, "not-a-token")
		require.Equal(t, codes.Unauthenticated, status.Code(err))
		require.False(t, reached)
		require.Zero(t, f.reads.take())
	})
	t.Run("a revoked session is refused", func(t *testing.T) {
		err, reached, _ := open(t, f.revokedSession(t))
		require.Equal(t, codes.Unauthenticated, status.Code(err))
		require.Contains(t, fmt.Sprint(err), "session revoked")
		require.False(t, reached)
		require.Equal(t, 1, f.reads.take())
	})
	t.Run("a live session opens, carrying the verified claims", func(t *testing.T) {
		err, reached, claims := open(t, f.liveSession(t))
		require.NoError(t, err)
		require.True(t, reached)
		require.NotEmpty(t, claims)
		require.Equal(t, 1, f.reads.take())
	})
	t.Run("a verified bearer with no session row opens", func(t *testing.T) {
		token, _ := f.newToken(t)
		err, reached, _ := open(t, token)
		require.NoError(t, err)
		require.True(t, reached)
		require.Equal(t, 1, f.reads.take())
	})
	// A lookup that failed under a delegation's narrowed authority would fail
	// open, and a revoked session would get its stream.
	t.Run("a revoked session is refused under an active delegation", func(t *testing.T) {
		delegated := opener(verifier.StreamInterceptor(f.verifier, f.logger, narrowDelegation{}))
		err, reached, _ := delegated(t, f.revokedSession(t))
		require.Equal(t, codes.Unauthenticated, status.Code(err))
		require.Contains(t, fmt.Sprint(err), "session revoked")
		require.False(t, reached)
	})
}
