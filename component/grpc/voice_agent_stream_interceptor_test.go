package memql

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/verifier"
)

// stubBase is a thin base-chain stand-in that just records whether
// it ran. Lets tests confirm "fall through to base" vs "admit here"
// without spinning up the full identity-JWT chain.
type stubBase struct {
	ran bool
}

func (s *stubBase) Interceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		s.ran = true
		return handler(srv, ss)
	}
}

func vaServerStream(md metadata.MD) grpc.ServerStream {
	ctx := metadata.NewIncomingContext(context.Background(), md)
	return &vaFakeStream{ctx: ctx}
}

type vaFakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *vaFakeStream) Context() context.Context { return s.ctx }

func vaJWKSHandler(km *identity.KeyManager) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(identity.BuildJWKS(km, time.Now().UTC()))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

func vaNewIssuer(t *testing.T, baseURL string) (*identity.KeyManager, *identity.JWTIssuer) {
	t.Helper()
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	require.NoError(t, err)
	require.NoError(t, km.Load())
	iss, err := identity.NewJWTIssuer(km, identity.Config{
		Enabled:     true,
		BaseURL:     baseURL,
		JWTAudience: "memql",
		KeyDir:      dir,
	})
	require.NoError(t, err)
	return km, iss
}

func vaNewVerifier(t *testing.T, baseURL string) *verifier.Verifier {
	t.Helper()
	cfg := verifier.Config{
		BaseURL:             baseURL,
		ExpectedAudience:    "memql",
		JWKSRefreshInterval: time.Hour,
		JWKSFetchTimeout:    5 * time.Second,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cache, err := verifier.NewJWKSCache(cfg, logger)
	require.NoError(t, err)
	v, err := verifier.New(cfg, cache, nil, logger)
	require.NoError(t, err)
	return v
}

// TestVoiceAgentInterceptor_JWTAdmits is the load-bearing #109
// case: a class="voice_agent" JWT minted by the identity service
// is admitted through the voice-agent interceptor without
// consulting the base chain.
func TestVoiceAgentInterceptor_JWTAdmits(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := vaNewIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", vaJWKSHandler(km))

	tok, _, err := iss.IssueVoiceAgentAccessToken(identity.VoiceAgentIssueInput{
		IdentityId: "v1:identity:identity:va-prod-1",
		InstanceId: "voice-agent-prod-us-east-1",
	}, time.Now().UTC())
	require.NoError(t, err)

	v := vaNewVerifier(t, srv.URL)
	base := &stubBase{}
	interceptor := NewVoiceAgentStreamInterceptor(base.Interceptor(), v, slog.Default())

	md := metadata.Pairs("authorization", "Bearer "+tok)
	admitted := false
	err = interceptor(nil, vaServerStream(md), &grpc.StreamServerInfo{FullMethod: "/x"}, func(_ interface{}, _ grpc.ServerStream) error {
		admitted = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, admitted, "JWT-class voice-agent token rejected")
	assert.False(t, base.ran, "voice-agent JWT path must skip base chain")
}

// TestVoiceAgentInterceptor_UserClassJWTFallsThrough confirms a
// plain user-class JWT does NOT get admitted via the voice-agent
// path -- it falls through to the base chain where the regular
// auth middleware decides.
func TestVoiceAgentInterceptor_UserClassJWTFallsThrough(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := vaNewIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", vaJWKSHandler(km))

	tok, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId: "v1:identity:user:alice",
		Email:  "alice@example.com",
	}, time.Now().UTC())
	require.NoError(t, err)

	v := vaNewVerifier(t, srv.URL)
	base := &stubBase{}
	interceptor := NewVoiceAgentStreamInterceptor(base.Interceptor(), v, slog.Default())

	md := metadata.Pairs("authorization", "Bearer "+tok)
	err = interceptor(nil, vaServerStream(md), &grpc.StreamServerInfo{FullMethod: "/x"}, func(_ interface{}, _ grpc.ServerStream) error {
		return nil
	})
	require.NoError(t, err)
	assert.True(t, base.ran, "user-class JWT must fall through to base chain")
}

// TestVoiceAgentInterceptor_PATFallsThrough confirms PATs aren't
// considered as voice-agent candidates -- they go straight to base.
func TestVoiceAgentInterceptor_PATFallsThrough(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Config.Handler = mux
	km, _ := vaNewIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", vaJWKSHandler(km))

	v := vaNewVerifier(t, srv.URL)
	base := &stubBase{}
	interceptor := NewVoiceAgentStreamInterceptor(base.Interceptor(), v, slog.Default())
	md := metadata.Pairs("authorization", "Bearer mql_pat_anything")
	err := interceptor(nil, vaServerStream(md), &grpc.StreamServerInfo{FullMethod: "/x"}, func(_ interface{}, _ grpc.ServerStream) error {
		return nil
	})
	require.NoError(t, err)
	assert.True(t, base.ran, "PAT bearers must fall through to base chain")
}

// TestVoiceAgentInterceptor_NilVerifierFallsThrough: a binary
// without the voice-agent verifier configured passes every call to
// base. Used by tests and by binaries that don't run the voice
// surface.
func TestVoiceAgentInterceptor_NilVerifierFallsThrough(t *testing.T) {
	base := &stubBase{}
	interceptor := NewVoiceAgentStreamInterceptor(base.Interceptor(), nil, slog.Default())
	md := metadata.Pairs("authorization", "Bearer some-token")
	err := interceptor(nil, vaServerStream(md), &grpc.StreamServerInfo{FullMethod: "/x"}, func(_ interface{}, _ grpc.ServerStream) error {
		return nil
	})
	require.NoError(t, err)
	assert.True(t, base.ran)
}

// TestIsVoiceAgentPayload_Allowlist pins the client->server payload
// surface the voice-agent token may use. The realtime executor's MCP
// tool bridge (ListTools at bring-up + CallTool per model-driven call)
// MUST be admitted -- omitting them tore down the stream with
// PermissionDenied before any audio could flow. Data-plane writes
// (Query/Mutation/Subscribe) must still be rejected.
func TestIsVoiceAgentPayload_Allowlist(t *testing.T) {
	allowed := []any{
		&memqlv1.MemqlClientMessage_ClientHello{},
		&memqlv1.MemqlClientMessage_Ack{},
		&memqlv1.MemqlClientMessage_VoiceAgentSessionStart{},
		&memqlv1.MemqlClientMessage_VoiceAgentSessionEnd{},
		&memqlv1.MemqlClientMessage_VoiceAgentPartialTranscript{},
		&memqlv1.MemqlClientMessage_VoiceAgentFinalTranscript{},
		&memqlv1.MemqlClientMessage_VoiceAgentTurnRequest{},
		&memqlv1.MemqlClientMessage_VoiceAgentRealtimeOutput{},
		&memqlv1.MemqlClientMessage_ListTools{},
		&memqlv1.MemqlClientMessage_CallTool{},
	}
	for _, p := range allowed {
		assert.Truef(t, isVoiceAgentPayload(p), "%T must be admitted", p)
	}
	rejected := []any{
		&memqlv1.MemqlClientMessage_ExecuteQuery{},
		&memqlv1.MemqlClientMessage_Subscribe{},
		&memqlv1.MemqlClientMessage_ConceptsSubscribe{},
	}
	for _, p := range rejected {
		assert.Falsef(t, isVoiceAgentPayload(p), "%T must be rejected", p)
	}
}
