package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestAnonymousPayloadAllowlist pins the surface an unauthenticated session
// may reach: query execution, graph subscriptions, and the stream-level
// control frames every caller needs. Nothing else.
//
// ExecuteQuery is admitted and CAN carry a mutation -- one message serves
// both. That is not a hole: executeWrite refuses every anonymous write
// before the concept is even resolved (see
// TestAnonymousWriteIsRefusedAtTheChokepoint in component/memql). The
// read/write split is enforced where writes happen rather than by parsing
// intent out of a query string here.
func TestAnonymousPayloadAllowlist(t *testing.T) {
	for _, p := range []any{
		&memqlv1.MemqlClientMessage_ClientHello{},
		&memqlv1.MemqlClientMessage_Ack{},
		&memqlv1.MemqlClientMessage_CancelRequest{},
		&memqlv1.MemqlClientMessage_ExecuteQuery{},
		&memqlv1.MemqlClientMessage_Subscribe{},
		&memqlv1.MemqlClientMessage_Unsubscribe{},
	} {
		assert.Truef(t, isAnonymousPayload(p), "%T must be admitted -- it is the read surface", p)
	}

	// Everything a stranger must not reach. The AI surface is the expensive
	// one (a leaked anonymous session driving model calls is a bill), the
	// identity family is the dangerous one, and the tool loop is both.
	for _, p := range []any{
		&memqlv1.MemqlClientMessage_AiChat{},
		&memqlv1.MemqlClientMessage_AiSpeech{},
		&memqlv1.MemqlClientMessage_AiSuggest{},
		&memqlv1.MemqlClientMessage_CallTool{},
		&memqlv1.MemqlClientMessage_ListTools{},
		&memqlv1.MemqlClientMessage_IdentityCreate{},
		&memqlv1.MemqlClientMessage_IdentityUpdate{},
		&memqlv1.MemqlClientMessage_IdentityList{},
		&memqlv1.MemqlClientMessage_DelegationCreate{},
		&memqlv1.MemqlClientMessage_CreateWorkerToken{},
		&memqlv1.MemqlClientMessage_SendGuestInvite{},
		&memqlv1.MemqlClientMessage_RotateAuth{},
		&memqlv1.MemqlClientMessage_RevokeAllSessions{},
		&memqlv1.MemqlClientMessage_NodeMaintenance{},
		&memqlv1.MemqlClientMessage_AgentGenerateTurn{},
		&memqlv1.MemqlClientMessage_ConceptsList{},
		&memqlv1.MemqlClientMessage_MyAccess{},
		&memqlv1.MemqlClientMessage_PolyphonRoomToken{},
		&memqlv1.MemqlClientMessage_VoiceAgentTurnRequest{},
	} {
		assert.Falsef(t, isAnonymousPayload(p), "%T must be refused on an anonymous session", p)
	}
}

// TestAnonymousInterceptorIsInertWhenDisabled is the property that makes
// this change safe to ship: with the flag off, every stream reaches `base`
// with the context it arrived on, so a credential-less dial is refused by
// exactly the code that refuses it today.
func TestAnonymousInterceptorIsInertWhenDisabled(t *testing.T) {
	var reached bool
	var sawAnonymous bool
	base := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		reached = true
		sawAnonymous = auth.IsAnonymousActor(ss.Context())
		return nil
	}

	in := NewAnonymousStreamInterceptor(base, false, nil)
	err := in(nil, &fakeAnonStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: "/m/Stream"}, nil)

	assert.NoError(t, err)
	assert.True(t, reached, "the base chain was not reached -- with public reads off this interceptor must be a pass-through")
	assert.False(t, sawAnonymous, "an anonymous actor was stamped with the flag OFF")
}

// TestAnonymousInterceptorIgnoresStreamsCarryingACredential. Anonymous is
// what a caller gets for presenting NOTHING, never a fallback for
// presenting something the chain did not like. Degrading an expired bearer
// to anonymous would turn "your session ended" into "you are now a
// stranger" -- the same page with less on it and no way to tell.
func TestAnonymousInterceptorIgnoresStreamsCarryingACredential(t *testing.T) {
	for name, md := range map[string]metadata.MD{
		"a bearer":         metadata.Pairs("authorization", "Bearer eyJhbGciOi.nonsense"),
		"a guest token":    metadata.Pairs("authorization", "Guest abc123"),
		"an operator key":  metadata.Pairs("authorization", "Operator k"),
		"a garbage scheme": metadata.Pairs("authorization", "Wat something"),
	} {
		var reachedBase, sawAnonymous bool
		base := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			reachedBase = true
			sawAnonymous = auth.IsAnonymousActor(ss.Context())
			return nil
		}
		ctx := metadata.NewIncomingContext(context.Background(), md)
		in := NewAnonymousStreamInterceptor(base, true, nil)
		if err := in(nil, &fakeAnonStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: "/m/Stream"}, nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		assert.Truef(t, reachedBase, "%s: the base chain was not reached", name)
		assert.Falsef(t, sawAnonymous, "%s: was degraded to the anonymous actor instead of being decided by the auth chain", name)
	}
}

// TestAnonymousInterceptorAdmitsACredentiallessStreamWhenEnabled -- the
// reachable positive, so the two tests above cannot be vacuous.
func TestAnonymousInterceptorAdmitsACredentiallessStreamWhenEnabled(t *testing.T) {
	var reachedBase bool
	base := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		reachedBase = true
		return nil
	}
	var handlerSawAnonymous bool
	handler := func(srv any, stream grpc.ServerStream) error {
		handlerSawAnonymous = auth.IsAnonymousActor(stream.Context())
		return nil
	}

	in := NewAnonymousStreamInterceptor(base, true, nil)
	err := in(nil, &fakeAnonStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: "/m/Stream"}, handler)

	assert.NoError(t, err)
	assert.False(t, reachedBase, "a credential-less stream was handed to the auth chain, which would refuse it")
	assert.True(t, handlerSawAnonymous, "the handler did not see the anonymous actor")
}

// An empty authorization header is not a credential. It arrives from
// clients that set the header unconditionally, and treating it as one
// would make the whole feature unreachable from them for no stated reason.
func TestAnEmptyAuthorizationHeaderIsNotACredential(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "   "))
	assert.False(t, streamCarriesCredential(ctx))
	assert.False(t, streamCarriesCredential(context.Background()))
	assert.True(t, streamCarriesCredential(
		metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer x"))))
}

type fakeAnonStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeAnonStream) Context() context.Context { return f.ctx }
