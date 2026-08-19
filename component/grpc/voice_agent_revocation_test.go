package memql

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/identity"
)

// memql#4111: a compromised class="voice_agent" JWT had no kill switch.
// The verify path is JWKS-only, this class creates no
// v1:identity:authSession row, and verifier.EpochCheck keys on a user row
// a machine identity does not have -- so soft-deleting the credential's
// identity row changed nothing for its full 90-day TTL.
//
// These tests pin the gate that closes it, and the two failure modes that
// matter more than the happy path: it must fail CLOSED on a lookup error,
// and it must NOT lock out a credential whose row simply does not exist.

const vaRevokedIdentityId = "v1:identity:identity:va-prod-1"

// stubRevocationResolver answers from a fixed map and counts calls, so the
// cache can be observed rather than assumed.
type stubRevocationResolver struct {
	revoked map[string]bool
	err     error
	calls   atomic.Int32
}

func (s *stubRevocationResolver) IsVoiceAgentTokenRevoked(_ context.Context, identityId string) (bool, error) {
	s.calls.Add(1)
	if s.err != nil {
		return false, s.err
	}
	return s.revoked[identityId], nil
}

// vaRevocationHarness mints a real voice-agent JWT against a real JWKS
// endpoint, so these exercise the actual verify path rather than a stub of
// it.
func vaRevocationHarness(t *testing.T, check *VoiceAgentRevocationCheck) (grpc.StreamServerInterceptor, *stubBase, metadata.MD) {
	t.Helper()
	srv := httptest.NewServer(http.NewServeMux())
	t.Cleanup(srv.Close)
	mux := http.NewServeMux()
	srv.Config.Handler = mux

	km, iss := vaNewIssuer(t, srv.URL)
	mux.Handle("/.well-known/jwks.json", vaJWKSHandler(km))

	tok, _, err := iss.IssueVoiceAgentAccessToken(identity.VoiceAgentIssueInput{
		IdentityId: vaRevokedIdentityId,
		InstanceId: "voice-agent-prod-us-east-1",
	}, time.Now().UTC())
	require.NoError(t, err)

	v := vaNewVerifier(t, srv.URL)
	base := &stubBase{}
	return NewVoiceAgentStreamInterceptor(base.Interceptor(), v, check, slog.Default()),
		base,
		metadata.Pairs("authorization", "Bearer "+tok)
}

func vaRun(interceptor grpc.StreamServerInterceptor, md metadata.MD) (bool, error) {
	admitted := false
	err := interceptor(nil, vaServerStream(md), &grpc.StreamServerInfo{FullMethod: "/x"},
		func(_ interface{}, _ grpc.ServerStream) error {
			admitted = true
			return nil
		})
	return admitted, err
}

// The credential is valid, correctly signed, and correctly classed -- and
// must still be refused, because the operator flipped its row.
func TestVoiceAgentRevocation_RevokedRowRejects(t *testing.T) {
	resolver := &stubRevocationResolver{revoked: map[string]bool{vaRevokedIdentityId: true}}
	interceptor, _, md := vaRevocationHarness(t, &VoiceAgentRevocationCheck{Resolver: resolver})

	admitted, err := vaRun(interceptor, md)
	require.Error(t, err)
	assert.False(t, admitted, "a revoked voice-agent credential was admitted")
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestVoiceAgentRevocation_LiveRowAdmits(t *testing.T) {
	resolver := &stubRevocationResolver{revoked: map[string]bool{vaRevokedIdentityId: false}}
	interceptor, base, md := vaRevocationHarness(t, &VoiceAgentRevocationCheck{Resolver: resolver})

	admitted, err := vaRun(interceptor, md)
	require.NoError(t, err)
	assert.True(t, admitted, "a live voice-agent credential was rejected")
	assert.False(t, base.ran, "voice-agent JWT path must skip the base chain")
}

// A lookup failure must not admit. The whole point of the gate is that a
// leaked credential is refused; a resolver that errors on a DB blip and
// falls open would make the gate worthless exactly when the store is
// unhealthy.
func TestVoiceAgentRevocation_LookupErrorFailsClosed(t *testing.T) {
	resolver := &stubRevocationResolver{err: errors.New("db unreachable")}
	interceptor, _, md := vaRevocationHarness(t, &VoiceAgentRevocationCheck{Resolver: resolver})

	admitted, err := vaRun(interceptor, md)
	require.Error(t, err)
	assert.False(t, admitted, "the gate admitted traffic on a failed revocation lookup")
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

// An UNKNOWN row is not a revoked row. The operator-CLI mint path does not
// persist an identity row, so treating "no row" as revoked would break
// minting rather than close a leak. This is the same convention the
// node-class gate uses.
func TestVoiceAgentRevocation_UnknownRowAdmits(t *testing.T) {
	resolver := &stubRevocationResolver{revoked: map[string]bool{}}
	interceptor, _, md := vaRevocationHarness(t, &VoiceAgentRevocationCheck{Resolver: resolver})

	admitted, err := vaRun(interceptor, md)
	require.NoError(t, err)
	assert.True(t, admitted, "a credential with no persisted row must not be locked out")
}

// A nil check is the pre-#4111 behaviour and must stay reachable: dev
// builds and tests run with no store.
func TestVoiceAgentRevocation_NilCheckSkipsLookup(t *testing.T) {
	interceptor, _, md := vaRevocationHarness(t, nil)
	admitted, err := vaRun(interceptor, md)
	require.NoError(t, err)
	assert.True(t, admitted)

	resolver := &stubRevocationResolver{}
	interceptor2, _, md2 := vaRevocationHarness(t, &VoiceAgentRevocationCheck{Resolver: nil})
	admitted2, err2 := vaRun(interceptor2, md2)
	require.NoError(t, err2)
	assert.True(t, admitted2)
	assert.Equal(t, int32(0), resolver.calls.Load())
}

// The cache must memoise BOTH answers -- caching only "live" would leave a
// revoked credential re-reading the store on every open, and caching
// nothing would put a DB read on the stream-open path for every reconnect.
func TestVoiceAgentRevocationCache_MemoisesBothAnswers(t *testing.T) {
	for _, revoked := range []bool{true, false} {
		resolver := &stubRevocationResolver{revoked: map[string]bool{"id-1": revoked}}
		cache := newVoiceAgentRevocationCache(time.Hour)

		for i := 0; i < 3; i++ {
			got, err := cache.lookup(context.Background(), "id-1", resolver)
			require.NoError(t, err)
			assert.Equal(t, revoked, got)
		}
		assert.Equal(t, int32(1), resolver.calls.Load(),
			"revoked=%v: the cache re-read the store instead of memoising", revoked)
	}
}

// An error must never be memoised: a transient failure would otherwise
// pin a wrong answer for the whole TTL.
func TestVoiceAgentRevocationCache_DoesNotMemoiseErrors(t *testing.T) {
	resolver := &stubRevocationResolver{err: errors.New("boom")}
	cache := newVoiceAgentRevocationCache(time.Hour)

	for i := 0; i < 3; i++ {
		_, err := cache.lookup(context.Background(), "id-1", resolver)
		require.Error(t, err)
	}
	assert.Equal(t, int32(3), resolver.calls.Load(), "an error answer was memoised")
}

// An expired entry must be re-read, or the "operator revoked it" signal
// would never arrive for an agent holding a long-lived stream open.
func TestVoiceAgentRevocationCache_ExpiredEntryRefetches(t *testing.T) {
	resolver := &stubRevocationResolver{revoked: map[string]bool{"id-1": false}}
	cache := newVoiceAgentRevocationCache(time.Nanosecond)

	_, err := cache.lookup(context.Background(), "id-1", resolver)
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)

	resolver.revoked["id-1"] = true
	got, err := cache.lookup(context.Background(), "id-1", resolver)
	require.NoError(t, err)
	assert.True(t, got, "the cache served a stale not-revoked answer past its TTL")
	assert.Equal(t, int32(2), resolver.calls.Load())
}
