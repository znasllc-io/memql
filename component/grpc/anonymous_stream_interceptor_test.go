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
	err := in(nil, &fakeAnonStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: memqlv1.MemqlService_Stream_FullMethodName}, nil)

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
		if err := in(nil, &fakeAnonStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: memqlv1.MemqlService_Stream_FullMethodName}, nil); err != nil {
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
	err := in(nil, &fakeAnonStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: memqlv1.MemqlService_Stream_FullMethodName}, handler)

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

// TestAnonymousAdmissionIsScopedToTheMemqlStream is the gate for the hole
// this interceptor's SHAPE invites.
//
// This gRPC server carries WorkerService (agent nodes) and
// DeployControlService (the identity node) alongside MemqlService, so every
// interceptor in the chain sees their streams. The surface pin cannot help
// there: it inspects a *MemqlClientMessage and passes anything else through
// untouched, which is correct for the voice-agent interceptor it copies --
// admission there needs a valid signed JWT -- and wrong here, where the
// premise is admitting a caller who presented nothing.
//
// Downstream handlers have their own owner/role gates and RoleAnonymous
// would fail them. That is not the standard: an interceptor that opens a
// door to the internet must not rely on somebody else's gate to close it.
func TestAnonymousAdmissionIsScopedToTheMemqlStream(t *testing.T) {
	for _, method := range []string{
		"/znasllc.memql.v1.WorkerService/Stream",
		"/znasllc.memql.v1.DeployControlService/RequestDeploy",
		"/znasllc.memql.v1.NodeService/Stream",
		"/some.other.Service/Method",
	} {
		var reachedBase, sawAnonymous bool
		base := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			reachedBase = true
			sawAnonymous = auth.IsAnonymousActor(ss.Context())
			return nil
		}
		in := NewAnonymousStreamInterceptor(base, true, nil)
		if err := in(nil, &fakeAnonStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: method}, nil); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		assert.Truef(t, reachedBase, "%s: was not handed to the auth chain -- an anonymous caller reached a service this interceptor was not designed for", method)
		assert.Falsef(t, sawAnonymous, "%s: an anonymous actor was stamped on a non-MemqlService stream", method)
	}

	// The reachable positive: the one method it IS for still admits, so the
	// assertions above cannot pass by the feature being switched off.
	var handlerSawAnonymous bool
	handler := func(srv any, stream grpc.ServerStream) error {
		handlerSawAnonymous = auth.IsAnonymousActor(stream.Context())
		return nil
	}
	in := NewAnonymousStreamInterceptor(
		func(any, grpc.ServerStream, *grpc.StreamServerInfo, grpc.StreamHandler) error { return nil },
		true, nil)
	if err := in(nil, &fakeAnonStream{ctx: context.Background()},
		&grpc.StreamServerInfo{FullMethod: memqlv1.MemqlService_Stream_FullMethodName}, handler); err != nil {
		t.Fatal(err)
	}
	assert.True(t, handlerSawAnonymous, "the MemqlService stream stopped admitting anonymous callers")
}

// TestEnsureAccessPreservesTheAnonymousActor is the regression test for a
// bypass that every other test in this epic was blind to.
//
// handleExecuteQuery stamps `auth.ContextWithAccess(ctx, s.ensureAccess(ctx))`
// onto the request context before calling the engine. ensureAccess resolves an
// actor from CLAIMS, and an anonymous stream has none -- so it fell through to
// FallbackFromClaims and returned an EMPTY actor whose IsAnonymous is false.
// That empty actor REPLACED the interceptor's anonymous one, and row admission
// then took the ordinary path, where an UNDECLARED concept admits every
// caller.
//
// So the load-bearing rule of the public tier -- "an anonymous caller reaches
// public-tier concepts and nothing else, undeclared included" -- was bypassed
// on ExecuteQuery, which is the ONLY read path an anonymous caller has. It
// failed in the admitting direction, silently, with every gate reporting
// exactly what it was asked.
//
// The row-gate tests could not catch it: they build a context directly and
// never cross this seam. That is why this test lives here, beside the
// function, rather than beside the rule it protects.
func TestEnsureAccessPreservesTheAnonymousActor(t *testing.T) {
	s := &streamSession{service: &service{}}

	ctx := auth.ContextWithAnonymousActor(context.Background())
	got := s.ensureAccess(ctx)

	if got == nil {
		t.Fatal("ensureAccess returned nil for an anonymous stream")
	}
	if !got.IsAnonymousActor() {
		t.Fatalf("ensureAccess replaced the anonymous actor with %+v.\n"+
			"Everything downstream reads the actor it returns, so row admission would take the ordinary "+
			"path -- and an UNDECLARED concept admits every caller there. That is the one rule the public "+
			"tier exists to enforce, bypassed on the only read path an anonymous caller has.", got)
	}
	if got.UserId != auth.AnonymousUserId {
		t.Errorf("UserId = %q, want %q -- the result-cache key folds this in, so a changed value also un-shares every public read", got.UserId, auth.AnonymousUserId)
	}

	// The reachable positive: an ordinary stream still resolves from claims
	// and does NOT come back anonymous, so the branch above cannot have been
	// written so broadly that it swallows everyone.
	plain := &streamSession{service: &service{}}
	if ac := plain.ensureAccess(context.Background()); ac != nil && ac.IsAnonymousActor() {
		t.Error("a stream with no anonymous actor resolved AS anonymous -- the branch is matching more than it should")
	}
}

// TestSubscribeResolvesTheActorForGraphKindsToo is the second half of the
// same class as TestEnsureAccessPreservesTheAnonymousActor: the actor was
// correct, and the live path could not see it.
//
// handleBusEvent admits each delivered graph event with currentAccess(),
// which returns the CACHED actor and deliberately does not resolve -- a
// resolve on the event pump would put a lock and a database round trip on
// the delivery path of every event. handleSubscribe resolved the actor only
// inside its non-graph branch, so a stream whose first message was a
// GRAPH_EVENTS subscribe reached fan-out with a NIL actor, which
// rowAuthzAdmits treats as an empty one -- and an undeclared concept admits
// an empty actor.
//
// For an authenticated caller that is the standing undeclared behaviour and
// errs toward fewer rows on the declared tiers. For an anonymous caller it
// errs the other way: the live feed would have been looser than the read it
// mirrors, which is the exact property memql#4309's design D2 exists to
// prevent.
func TestSubscribeResolvesTheActorForGraphKindsToo(t *testing.T) {
	s := &streamSession{service: &service{}}

	// Nothing has resolved yet -- the state a stream is in when its FIRST
	// message is a subscribe.
	if s.currentAccess() != nil {
		t.Fatal("currentAccess() was already populated; this fixture needs an unresolved session")
	}

	// What handleSubscribe now does for every kind.
	s.ensureAccess(auth.ContextWithAnonymousActor(context.Background()))

	got := s.currentAccess()
	if got == nil {
		t.Fatal("currentAccess() is still nil after subscribe resolved the actor -- every delivered event would be admitted against an empty actor, and an undeclared concept admits one")
	}
	if !got.IsAnonymousActor() {
		t.Fatalf("currentAccess() = %+v, want the anonymous actor -- the fan-out gate would not know the subscriber is anonymous", got)
	}
}
