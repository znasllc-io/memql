package node

// query_forward_actor_test.go -- the forwarded-query hop must carry the
// originating caller's identity (memql#2814).
//
// handleQueryForward executed forwarded DSL on context.Background(), so the
// query ran with NO AccessContext. Under the deny-on-nil default (#2801) that
// silently returns zero rows for any actor-gated construct -- the cockpit
// telephony views would blank in a multi-node deployment. Before that default
// it failed the other way: isClusterOwner resolved true and every partition's
// call-detail records were served to whatever the forward carried.
//
// This was filed as unreachable (zero producers) precisely so it could not
// land silently once someone wires the producer side. These tests are the
// tripwire: they fail if the hop ever goes back to executing without an actor.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// capturingExecutor records the context each Execute call receives so the
// test can assert on the identity the forwarded query actually ran under.
type capturingExecutor struct {
	called  bool
	gotCtx  context.Context
	execErr error
}

func (c *capturingExecutor) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	c.called = true
	c.gotCtx = ctx
	return &memqlengine.ExecuteResult{}, c.execErr
}

// stubResolver stands in for *auth.IdentityResolver. It models the property
// that matters: authority comes from the STORE, keyed by subject -- never from
// the claims on the wire. A subject that is not in `users` does not resolve,
// and the role is whatever the store says regardless of any forwarded `role`.
type stubResolver struct {
	users  map[string]auth.Role
	called int
}

func (r *stubResolver) LoadFromClaims(ctx context.Context, claims map[string]any) (*auth.AccessContext, error) {
	r.called++
	sub, _ := claims["sub"].(string)
	role, ok := r.users[sub]
	if !ok {
		return nil, errors.New("user not provisioned")
	}
	return &auth.AccessContext{UserId: sub, Role: role}, nil
}

func newQueryForwardService(exec QueryExecutor, resolver ForwardedAccessResolver) *nodeService {
	return &nodeService{
		logger:         testLogger(),
		identity:       testIdentity(),
		peerManager:    NewPeerManager(testIdentity(), testLogger()),
		queryExecutor:  exec,
		accessResolver: resolver,
	}
}

// knownUser is the provisioned principal the happy-path tests forward as.
const knownUser = "v1:identity:user:user-2814"

func newStubResolver() *stubResolver {
	return &stubResolver{users: map[string]auth.Role{knownUser: auth.Role("admin")}}
}

func lastQueryResponse(t *testing.T, f *fakeStream) *nodev1.QueryResponse {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		t.Fatal("handler sent no message")
	}
	resp := f.sent[len(f.sent)-1].GetQueryResponse()
	if resp == nil {
		t.Fatalf("last message is not a QueryResponse: %+v", f.sent[len(f.sent)-1])
	}
	return resp
}

// TestHandleQueryForward_PropagatesCallerIdentity is the core assertion: the
// claims the forwarding node put on the envelope are rebuilt into the context
// the query executes under, so the receiving node sees the SAME actor.
func TestHandleQueryForward_PropagatesCallerIdentity(t *testing.T) {
	exec := &capturingExecutor{}
	svc := newQueryForwardService(exec, newStubResolver())
	stream := newFakeStream()

	claims := auth.ForwardedClaimsFromIdentity(auth.UserIdentity{
		Subject: knownUser,
		Email:   "forwarded@example.com",
		Role:    "admin",
	})

	svc.handleQueryForward("peer-a", &nodev1.QueryForward{
		RequestId: "req-1",
		Query:     "query allNumbers",
		Auth:      claims,
	}, stream)

	if !exec.called {
		t.Fatal("expected the forwarded query to execute")
	}
	if exec.gotCtx == nil {
		t.Fatal("executor received a nil context")
	}

	// The whole point: identity survived the hop.
	// Assert the AccessContext -- the key the ENGINE reads. Asserting
	// UserIdentityFromContext (the TokenInfo key) instead would be a proxy
	// for the property, not the property: ContextWithForwardedClaims sets
	// TokenInfo + Claims but NOT AccessContext, so a context that satisfies
	// the TokenInfo assertion can still hand the DSL a deny-all envelope.
	// That gap is the whole of memql#2814.
	ac, ok := auth.AccessFromContext(exec.gotCtx)
	if !ok || ac == nil {
		t.Fatal("forwarded query executed with NO AccessContext -- every engine actor surface (resolveActorPath, spec evaluator, mutation templates) reads auth.AccessFromContext, so actor.userId resolves to \"\" and an actor-gated construct silently returns zero rows (memql#2814)")
	}
	if ac.UserId != knownUser {
		t.Fatalf("actor userId = %q, want %q -- the receiving node is executing as the wrong principal", ac.UserId, knownUser)
	}
	if string(ac.Role) != "admin" {
		t.Fatalf("actor role = %q, want %q", ac.Role, "admin")
	}

	// Assert through the envelope the DSL actually binds, so this pins the
	// user-visible behaviour rather than a struct field.
	envelope := auth.ActorEnvelopeMap(ac)
	if envelope["userId"] != knownUser {
		t.Fatalf("actor.userId in the DSL envelope = %v, want %q", envelope["userId"], knownUser)
	}

	if resp := lastQueryResponse(t, stream); !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}
}

// TestHandleQueryForward_RefusesWithoutIdentity pins the fail-closed half.
// A forward with no claims is a producer bug; executing it anonymously is
// how #2814 caused either a silent zero-row result or a cross-partition
// data leak, depending on which side of #2801 you were on. Refuse loudly
// instead, and above all do NOT execute.
func TestHandleQueryForward_RefusesWithoutIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth map[string]string
	}{
		{"nil auth map", nil},
		{"empty auth map", map[string]string{}},
		// A non-empty map that resolves to no principal. A len(auth)>0 gate
		// would admit these; the gate must test the REBUILT principal.
		{"claims with no subject", map[string]string{"email": "nobody@example.com"}},
		{"unrecognised claims only", map[string]string{"totally": "bogus"}},
		// The escalation case: FallbackFromClaims lifts `role` without
		// requiring `sub`, and IsClusterOwner() is just Role==RoleOwner --
		// so admitting this would hand CLUSTER-OWNER authority to a
		// subject-less claim map.
		{"role owner with no subject", map[string]string{"role": "owner"}},
		// Subject that does not exist in the store. Claim-derived authority
		// would happily execute as it; the resolver refuses.
		{"unprovisioned subject", map[string]string{"sub": "v1:identity:user:ghost", "role": "owner"}},
		{"non-canonical subject", map[string]string{"sub": "not-a-real-user", "role": "owner"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &capturingExecutor{}
			svc := newQueryForwardService(exec, newStubResolver())
			stream := newFakeStream()

			svc.handleQueryForward("peer-a", &nodev1.QueryForward{
				RequestId: "req-2",
				Query:     "query allNumbers",
				Auth:      tc.auth,
			}, stream)

			if exec.called {
				t.Fatal("a forward with no caller identity MUST NOT execute -- running it anonymously is the #2814 defect")
			}

			resp := lastQueryResponse(t, stream)
			if resp.Success {
				t.Fatal("expected the refusal to be reported as a failure")
			}
			if !strings.Contains(resp.Error, "no resolvable caller identity") {
				t.Fatalf("the error must say identity is missing so the producer bug is obvious, got: %q", resp.Error)
			}
			if resp.RequestId != "req-2" {
				t.Fatalf("refusal must correlate to the request id, got %q", resp.RequestId)
			}
		})
	}
}

// TestHandleQueryForward_UsesStreamContext pins cancellation propagation: the
// forwarded execution must die with the peer stream that asked for it, rather
// than running on a detached background context.
func TestHandleQueryForward_UsesStreamContext(t *testing.T) {
	exec := &capturingExecutor{}
	svc := newQueryForwardService(exec, newStubResolver())

	streamCtx, cancel := context.WithCancel(context.Background())
	stream := newFakeStream()
	stream.ctx = streamCtx

	svc.handleQueryForward("peer-a", &nodev1.QueryForward{
		RequestId: "req-3",
		Query:     "query allNumbers",
		Auth:      auth.ForwardedClaimsFromIdentity(auth.UserIdentity{Subject: knownUser}),
	}, stream)

	if !exec.called {
		t.Fatal("expected the forwarded query to execute")
	}
	if err := exec.gotCtx.Err(); err != nil {
		t.Fatalf("context should be live before cancel, got %v", err)
	}

	cancel()
	if err := exec.gotCtx.Err(); err == nil {
		t.Fatal("cancelling the peer stream must cancel the forwarded execution -- context.Background() would survive it and keep working for a caller that is already gone")
	}
}

// TestHandleQueryForward_RoleComesFromTheStoreNotTheWire is the security
// assertion this whole path turns on.
//
// The claims arrive as an UNSIGNED map on a peer message. If authority were
// derived from them (auth.FallbackFromClaims does exactly that -- it lifts
// `role` straight out of the map), then any peer able to send a QueryForward
// could execute as cluster owner: IsClusterOwner() is just Role==RoleOwner,
// and the forwarded surface dispatches mutations and logic, not only reads.
//
// The receiving node must therefore resolve the principal against the store
// and take the role from THERE, ignoring whatever the wire asserted.
func TestHandleQueryForward_RoleComesFromTheStoreNotTheWire(t *testing.T) {
	exec := &capturingExecutor{}
	resolver := newStubResolver() // knownUser is "admin" in the store
	svc := newQueryForwardService(exec, resolver)
	stream := newFakeStream()

	svc.handleQueryForward("peer-a", &nodev1.QueryForward{
		RequestId: "req-esc",
		Query:     "query allNumbers",
		// A peer claiming owner for a real user it has no right to elevate.
		Auth: map[string]string{"sub": knownUser, "role": "owner"},
	}, stream)

	if !exec.called {
		t.Fatal("a provisioned subject should still execute")
	}
	ac, ok := auth.AccessFromContext(exec.gotCtx)
	if !ok || ac == nil {
		t.Fatal("expected an AccessContext")
	}
	if ac.IsClusterOwner() {
		t.Fatal("PRIVILEGE ESCALATION: the forwarded `role: owner` claim was honoured. Authority must come from the store, not the wire -- any peer could otherwise assert cluster-owner for an arbitrary subject (memql#2814)")
	}
	if string(ac.Role) != "admin" {
		t.Fatalf("role = %q, want the STORED role %q", ac.Role, "admin")
	}
	if resolver.called == 0 {
		t.Fatal("the resolver must be consulted -- claim-derived authority is the defect")
	}
}

// TestHandleQueryForward_RefusesWithoutResolver pins the fail-closed default.
// A node that wires SetQueryExecutor but forgets SetIdentityResolver must
// refuse forwards, not silently fall back to trusting the claims.
func TestHandleQueryForward_RefusesWithoutResolver(t *testing.T) {
	exec := &capturingExecutor{}
	svc := newQueryForwardService(exec, nil) // executor wired, resolver forgotten
	stream := newFakeStream()

	svc.handleQueryForward("peer-a", &nodev1.QueryForward{
		RequestId: "req-nores",
		Query:     "query allNumbers",
		Auth:      auth.ForwardedClaimsFromIdentity(auth.UserIdentity{Subject: knownUser, Role: "owner"}),
	}, stream)

	if exec.called {
		t.Fatal("with no resolver the handler MUST refuse -- falling back to the forwarded claims is how a peer asserts its own authority")
	}
	if resp := lastQueryResponse(t, stream); resp.Success {
		t.Fatal("expected a failure response")
	}
}

// TestHandleQueryForward_DoesNotInheritStreamIdentity closes the round-2
// finding that the gate read claims back out of the CONTEXT. Because
// ContextWithForwardedClaims is a no-op on an empty map, a claims-carrying
// stream context would have supplied the identity instead -- so a forward
// with NO auth would execute as the sending NODE. Claims must come from
// req.GetAuth() and nowhere else.
func TestHandleQueryForward_DoesNotInheritStreamIdentity(t *testing.T) {
	exec := &capturingExecutor{}
	resolver := &stubResolver{users: map[string]auth.Role{
		knownUser: auth.Role("admin"),
		// The peer node itself is a resolvable principal -- so if the handler
		// ever reads identity off the stream context, it resolves cleanly and
		// executes. Only sourcing claims from the envelope prevents that.
		"v1:cluster:node:agent-0": auth.Role("owner"),
	}}
	svc := newQueryForwardService(exec, resolver)

	stream := newFakeStream()
	stream.ctx = auth.ContextWithClaims(context.Background(), map[string]any{
		"sub": "v1:cluster:node:agent-0", "role": "owner",
	})

	svc.handleQueryForward("peer-a", &nodev1.QueryForward{
		RequestId: "req-inherit",
		Query:     "query allNumbers",
		Auth:      nil, // empty envelope
	}, stream)

	if exec.called {
		ac, _ := auth.AccessFromContext(exec.gotCtx)
		t.Fatalf("a forward with an EMPTY auth map executed by inheriting the peer stream's identity (userId=%v) -- claims must be read from req.GetAuth(), not round-tripped through the context (memql#2814)", ac.UserId)
	}
}
