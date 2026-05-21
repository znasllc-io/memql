package identity

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeEngine routes queries to a per-fixture map of query-text-prefix
// -> nodes. The resolver only emits a handful of distinct query
// shapes (queryActiveDelegationsByIdentitySubject + queryUserById +
// queryIdentityById) so a prefix-match is enough to disambiguate.
type fakeEngine struct {
	mu    sync.Mutex
	calls []string
	// matchers is consulted in declaration order; first matching
	// prefix wins and its handler runs.
	matchers []fakeMatcher
}

type fakeMatcher struct {
	prefix  string
	handler func() (*memqlengine.ExecuteResult, error)
}

func (f *fakeEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, q)
	matchers := f.matchers
	f.mu.Unlock()
	for _, m := range matchers {
		if strings.HasPrefix(q, m.prefix) {
			return m.handler()
		}
	}
	return resultWith(nil), nil
}

func resultWith(nodes []*memqlv1.MemoryNode) *memqlengine.ExecuteResult {
	return &memqlengine.ExecuteResult{
		Bundle: &memqlv1.GraphBundle{Nodes: nodes},
	}
}

// delegationNode builds a MemoryNode shaped like delegationFull.
type delegationNodeOpts struct {
	ID              string
	IdentityId      string
	IdentitySubject string
	IdentityType    string
	AgentId         string
	RoleCeiling     string
	Scopes          []string
	ExpiresAt       time.Time
	Active          bool
	RevokedAt       time.Time
	CreatedAt       time.Time
}

func delegationNode(opts delegationNodeOpts) *memqlv1.MemoryNode {
	fields := map[string]*structpb.Value{
		"id":              structpb.NewStringValue(opts.ID),
		"identityId":      structpb.NewStringValue(opts.IdentityId),
		"identitySubject": structpb.NewStringValue(opts.IdentitySubject),
		"identityType":    structpb.NewStringValue(opts.IdentityType),
		"agentId":         structpb.NewStringValue(opts.AgentId),
		"roleCeiling":     structpb.NewStringValue(opts.RoleCeiling),
		"active":          structpb.NewBoolValue(opts.Active),
	}
	if len(opts.Scopes) > 0 {
		listValues := make([]*structpb.Value, 0, len(opts.Scopes))
		for _, s := range opts.Scopes {
			listValues = append(listValues, structpb.NewStringValue(s))
		}
		fields["scopes"] = structpb.NewListValue(&structpb.ListValue{Values: listValues})
	}
	if !opts.ExpiresAt.IsZero() {
		fields["expiresAt"] = structpb.NewStringValue(opts.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if !opts.RevokedAt.IsZero() {
		fields["revokedAt"] = structpb.NewStringValue(opts.RevokedAt.UTC().Format(time.RFC3339))
	}
	if !opts.CreatedAt.IsZero() {
		fields["createdAt"] = structpb.NewStringValue(opts.CreatedAt.UTC().Format(time.RFC3339))
	}
	return &memqlv1.MemoryNode{
		Id:      opts.ID,
		Payload: &structpb.Struct{Fields: fields},
	}
}

// identityNode mimics the v1:identity:identity row the resolver
// dereferences to find the userId behind a delegation. Resolver only
// reads the userId field.
func identityNode(identityId, userId string) *memqlv1.MemoryNode {
	return &memqlv1.MemoryNode{
		Id: identityId,
		Payload: &structpb.Struct{
			Fields: map[string]*structpb.Value{
				"id":     structpb.NewStringValue(identityId),
				"userId": structpb.NewStringValue(userId),
			},
		},
	}
}

// userNode mirrors a userFull projection -- just the fields Store.LookupUserById reads.
func userNode(userId, email, role string) *memqlv1.MemoryNode {
	return &memqlv1.MemoryNode{
		Id: userId,
		Payload: &structpb.Struct{
			Fields: map[string]*structpb.Value{
				"id":           structpb.NewStringValue(userId),
				"primaryEmail": structpb.NewStringValue(email),
				"role":         structpb.NewStringValue(role),
				"active":       structpb.NewBoolValue(true),
			},
		},
	}
}

// captureAuditor is a fake AuditLogger.
type captureAuditor struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (c *captureAuditor) Log(_ context.Context, ev AuditEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *captureAuditor) Last() *AuditEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) == 0 {
		return nil
	}
	cp := c.events[len(c.events)-1]
	return &cp
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fixtureEngine wires the standard set of matchers used across most
// tests: one delegation row, the identity behind it, and the user
// behind the identity.
func fixtureEngine(delegationNodes []*memqlv1.MemoryNode, identity *memqlv1.MemoryNode, user *memqlv1.MemoryNode) *fakeEngine {
	return &fakeEngine{
		matchers: []fakeMatcher{
			{
				prefix: "queryActiveDelegationsByIdentitySubject",
				handler: func() (*memqlengine.ExecuteResult, error) {
					return resultWith(delegationNodes), nil
				},
			},
			{
				prefix: "queryIdentityById",
				handler: func() (*memqlengine.ExecuteResult, error) {
					if identity == nil {
						return resultWith(nil), nil
					}
					return resultWith([]*memqlv1.MemoryNode{identity}), nil
				},
			},
			{
				prefix: "queryUserById",
				handler: func() (*memqlengine.ExecuteResult, error) {
					if user == nil {
						return resultWith(nil), nil
					}
					return resultWith([]*memqlv1.MemoryNode{user}), nil
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// cases
// ---------------------------------------------------------------------------

func TestResolve_NoDelegationReturnsNil(t *testing.T) {
	eng := fixtureEngine(nil, nil, nil)
	r := NewEngineDelegationResolver(eng, nil, quietLogger())
	dc, err := r.ResolveActiveDelegation(context.Background(), "v1:identity:user:alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dc != nil {
		t.Errorf("expected nil DelegationContext on no-row miss; got %+v", dc)
	}
}

func TestResolve_HappyPath_AdmitsWithCappedRole(t *testing.T) {
	created := time.Now().UTC().Add(-time.Hour)
	delegation := delegationNode(delegationNodeOpts{
		ID:              "v1:identity:delegation:abc",
		IdentityId:      "v1:identity:identity:bob",
		IdentitySubject: "subj-bob",
		IdentityType:    "human",
		AgentId:         "v1:agents:agent:helper",
		RoleCeiling:     "writer",
		Active:          true,
		CreatedAt:       created,
	})
	eng := fixtureEngine(
		[]*memqlv1.MemoryNode{delegation},
		identityNode("v1:identity:identity:bob", "v1:identity:user:bob"),
		userNode("v1:identity:user:bob", "bob@example.com", "admin"),
	)
	aud := &captureAuditor{}
	r := NewEngineDelegationResolver(eng, aud, quietLogger())

	dc, err := r.ResolveActiveDelegation(context.Background(), "subj-bob")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dc == nil {
		t.Fatal("expected DelegationContext; got nil")
	}
	if dc.AgentId != "v1:agents:agent:helper" {
		t.Errorf("AgentId mismatch: %q", dc.AgentId)
	}
	if dc.RoleCeiling != auth.RoleWriter {
		t.Errorf("ceiling = %q, want writer", dc.RoleCeiling)
	}
	if dc.DelegatingIdentity.Role != "admin" {
		t.Errorf("delegating role = %q, want admin", dc.DelegatingIdentity.Role)
	}
	if dc.DelegatingIdentity.Email != "bob@example.com" {
		t.Errorf("delegating email = %q, want bob@example.com", dc.DelegatingIdentity.Email)
	}
	// Audit emitted with the success outcome.
	last := aud.Last()
	if last == nil || last.Action != "delegation_used" || last.Outcome != AuditOutcomeSuccess {
		t.Errorf("expected success audit; got %+v", last)
	}
}

func TestResolve_RejectsCeilingExceedingDelegatorRole(t *testing.T) {
	// Delegator role = writer; delegation ceiling = admin. Admin
	// outranks writer (RoleLevel(admin)=1 < RoleLevel(writer)=2).
	// Reject.
	delegation := delegationNode(delegationNodeOpts{
		ID:              "v1:identity:delegation:over",
		IdentityId:      "v1:identity:identity:carol",
		IdentitySubject: "subj-carol",
		AgentId:         "v1:agents:agent:x",
		RoleCeiling:     "admin",
		Active:          true,
		CreatedAt:       time.Now().UTC().Add(-time.Hour),
	})
	eng := fixtureEngine(
		[]*memqlv1.MemoryNode{delegation},
		identityNode("v1:identity:identity:carol", "v1:identity:user:carol"),
		userNode("v1:identity:user:carol", "carol@example.com", "writer"),
	)
	aud := &captureAuditor{}
	r := NewEngineDelegationResolver(eng, aud, quietLogger())

	dc, err := r.ResolveActiveDelegation(context.Background(), "subj-carol")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dc != nil {
		t.Errorf("expected nil DelegationContext for ceiling-exceeds-delegator; got %+v", dc)
	}
	last := aud.Last()
	if last == nil || last.Action != "delegation_rejected_ceiling" || last.Outcome != AuditOutcomeBlocked {
		t.Errorf("expected blocked-ceiling audit; got %+v", last)
	}
	if !strings.Contains(last.FailureReason, "exceeds delegator role") {
		t.Errorf("reason should explain the ceiling rejection; got %q", last.FailureReason)
	}
}

func TestResolve_RejectsExpiredDelegation(t *testing.T) {
	// Delegation expired one hour ago.
	delegation := delegationNode(delegationNodeOpts{
		ID:              "v1:identity:delegation:stale",
		IdentityId:      "v1:identity:identity:dan",
		IdentitySubject: "subj-dan",
		AgentId:         "v1:agents:agent:x",
		RoleCeiling:     "reader",
		Active:          true,
		ExpiresAt:       time.Now().UTC().Add(-time.Hour),
		CreatedAt:       time.Now().UTC().Add(-2 * time.Hour),
	})
	eng := fixtureEngine(
		[]*memqlv1.MemoryNode{delegation},
		identityNode("v1:identity:identity:dan", "v1:identity:user:dan"),
		userNode("v1:identity:user:dan", "dan@example.com", "admin"),
	)
	aud := &captureAuditor{}
	r := NewEngineDelegationResolver(eng, aud, quietLogger())

	dc, err := r.ResolveActiveDelegation(context.Background(), "subj-dan")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dc != nil {
		t.Errorf("expected nil DelegationContext post-expiry; got %+v", dc)
	}
	last := aud.Last()
	if last == nil || last.Action != "delegation_rejected_expired" || last.Outcome != AuditOutcomeBlocked {
		t.Errorf("expected blocked-expired audit; got %+v", last)
	}
}

func TestResolve_NarrowsWildcardScopeForNonPrivilegedDelegator(t *testing.T) {
	// Reader delegator with a "*" scope -- the wildcard would let
	// the delegation paper over the role gate. Resolver drops it,
	// keeps the explicit scopes.
	delegation := delegationNode(delegationNodeOpts{
		ID:              "v1:identity:delegation:wild",
		IdentityId:      "v1:identity:identity:ed",
		IdentitySubject: "subj-ed",
		AgentId:         "v1:agents:agent:x",
		RoleCeiling:     "reader",
		Scopes:          []string{"*", "query:cognition.spaces", ""},
		Active:          true,
		CreatedAt:       time.Now().UTC().Add(-time.Hour),
	})
	eng := fixtureEngine(
		[]*memqlv1.MemoryNode{delegation},
		identityNode("v1:identity:identity:ed", "v1:identity:user:ed"),
		userNode("v1:identity:user:ed", "ed@example.com", "reader"),
	)
	r := NewEngineDelegationResolver(eng, nil, quietLogger())

	dc, err := r.ResolveActiveDelegation(context.Background(), "subj-ed")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dc == nil {
		t.Fatal("expected admit; got nil")
	}
	if len(dc.Scopes) != 1 || dc.Scopes[0] != "query:cognition.spaces" {
		t.Errorf("expected only the explicit scope; got %v", dc.Scopes)
	}
}

func TestResolve_KeepsWildcardForPrivilegedDelegator(t *testing.T) {
	// Owner delegator with a "*" scope -- allowed.
	delegation := delegationNode(delegationNodeOpts{
		ID:              "v1:identity:delegation:wild2",
		IdentityId:      "v1:identity:identity:fay",
		IdentitySubject: "subj-fay",
		AgentId:         "v1:agents:agent:x",
		RoleCeiling:     "owner",
		Scopes:          []string{"*"},
		Active:          true,
		CreatedAt:       time.Now().UTC().Add(-time.Hour),
	})
	eng := fixtureEngine(
		[]*memqlv1.MemoryNode{delegation},
		identityNode("v1:identity:identity:fay", "v1:identity:user:fay"),
		userNode("v1:identity:user:fay", "fay@example.com", "owner"),
	)
	r := NewEngineDelegationResolver(eng, nil, quietLogger())

	dc, err := r.ResolveActiveDelegation(context.Background(), "subj-fay")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dc == nil {
		t.Fatal("expected admit; got nil")
	}
	if len(dc.Scopes) != 1 || dc.Scopes[0] != "*" {
		t.Errorf("owner delegator should keep wildcard; got %v", dc.Scopes)
	}
}

func TestResolve_RejectsRevokedDelegation(t *testing.T) {
	// active=true but revokedAt set -- belt-and-suspenders skip
	// even though the query's traitIsActiveRecord should have
	// excluded it.
	delegation := delegationNode(delegationNodeOpts{
		ID:              "v1:identity:delegation:revoked",
		IdentityId:      "v1:identity:identity:gail",
		IdentitySubject: "subj-gail",
		AgentId:         "v1:agents:agent:x",
		RoleCeiling:     "reader",
		Active:          true,
		RevokedAt:       time.Now().UTC().Add(-time.Hour),
		CreatedAt:       time.Now().UTC().Add(-2 * time.Hour),
	})
	eng := fixtureEngine(
		[]*memqlv1.MemoryNode{delegation},
		identityNode("v1:identity:identity:gail", "v1:identity:user:gail"),
		userNode("v1:identity:user:gail", "gail@example.com", "admin"),
	)
	r := NewEngineDelegationResolver(eng, nil, quietLogger())

	dc, err := r.ResolveActiveDelegation(context.Background(), "subj-gail")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dc != nil {
		t.Errorf("revoked delegation must not admit; got %+v", dc)
	}
}

func TestResolve_RejectsOrphanedDelegation(t *testing.T) {
	// Delegation row points at an identityId that resolves to no
	// user -- treat as a dropped row and emit a blocked audit.
	delegation := delegationNode(delegationNodeOpts{
		ID:              "v1:identity:delegation:orphan",
		IdentityId:      "v1:identity:identity:ghost",
		IdentitySubject: "subj-ghost",
		AgentId:         "v1:agents:agent:x",
		RoleCeiling:     "reader",
		Active:          true,
		CreatedAt:       time.Now().UTC().Add(-time.Hour),
	})
	eng := fixtureEngine(
		[]*memqlv1.MemoryNode{delegation},
		identityNode("v1:identity:identity:ghost", "v1:identity:user:gone"),
		nil, // user lookup returns nothing
	)
	aud := &captureAuditor{}
	r := NewEngineDelegationResolver(eng, aud, quietLogger())

	dc, err := r.ResolveActiveDelegation(context.Background(), "subj-ghost")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dc != nil {
		t.Errorf("orphan delegation must not admit; got %+v", dc)
	}
	last := aud.Last()
	if last == nil || last.Action != "delegation_rejected_orphan" {
		t.Errorf("expected orphan audit; got %+v", last)
	}
}

func TestResolve_PicksNewestActiveOfMany(t *testing.T) {
	older := delegationNode(delegationNodeOpts{
		ID:              "v1:identity:delegation:older",
		IdentityId:      "v1:identity:identity:hal",
		IdentitySubject: "subj-hal",
		AgentId:         "v1:agents:agent:older",
		RoleCeiling:     "reader",
		Active:          true,
		CreatedAt:       time.Now().UTC().Add(-3 * time.Hour),
	})
	newer := delegationNode(delegationNodeOpts{
		ID:              "v1:identity:delegation:newer",
		IdentityId:      "v1:identity:identity:hal",
		IdentitySubject: "subj-hal",
		AgentId:         "v1:agents:agent:newer",
		RoleCeiling:     "writer",
		Active:          true,
		CreatedAt:       time.Now().UTC().Add(-time.Hour),
	})
	eng := fixtureEngine(
		[]*memqlv1.MemoryNode{older, newer},
		identityNode("v1:identity:identity:hal", "v1:identity:user:hal"),
		userNode("v1:identity:user:hal", "hal@example.com", "admin"),
	)
	r := NewEngineDelegationResolver(eng, nil, quietLogger())

	dc, err := r.ResolveActiveDelegation(context.Background(), "subj-hal")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dc == nil || dc.AgentId != "v1:agents:agent:newer" {
		t.Errorf("expected newer delegation to win; got %+v", dc)
	}
}

func TestResolve_PropagatesEngineError(t *testing.T) {
	eng := &fakeEngine{
		matchers: []fakeMatcher{
			{
				prefix: "queryActiveDelegationsByIdentitySubject",
				handler: func() (*memqlengine.ExecuteResult, error) {
					return nil, errors.New("db down")
				},
			},
		},
	}
	r := NewEngineDelegationResolver(eng, nil, quietLogger())

	dc, err := r.ResolveActiveDelegation(context.Background(), "subj-x")
	if err == nil {
		t.Fatal("expected error to propagate; got nil")
	}
	if dc != nil {
		t.Errorf("expected nil DelegationContext on engine error; got %+v", dc)
	}
}

func TestResolve_NoSubjectIsNoOp(t *testing.T) {
	eng := fixtureEngine(nil, nil, nil)
	r := NewEngineDelegationResolver(eng, nil, quietLogger())
	dc, err := r.ResolveActiveDelegation(context.Background(), "   ")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dc != nil {
		t.Errorf("empty subject must return nil; got %+v", dc)
	}
	// Engine should not be consulted at all.
	if len(eng.calls) != 0 {
		t.Errorf("expected zero engine calls for empty subject; got %d: %v", len(eng.calls), eng.calls)
	}
}

func TestResolve_AuditCarriesDelegationDetail(t *testing.T) {
	delegation := delegationNode(delegationNodeOpts{
		ID:              "v1:identity:delegation:zzz",
		IdentityId:      "v1:identity:identity:ivy",
		IdentitySubject: "subj-ivy",
		AgentId:         "v1:agents:agent:helper",
		RoleCeiling:     "reader",
		Active:          true,
		CreatedAt:       time.Now().UTC().Add(-time.Hour),
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
	})
	eng := fixtureEngine(
		[]*memqlv1.MemoryNode{delegation},
		identityNode("v1:identity:identity:ivy", "v1:identity:user:ivy"),
		userNode("v1:identity:user:ivy", "ivy@example.com", "admin"),
	)
	aud := &captureAuditor{}
	r := NewEngineDelegationResolver(eng, aud, quietLogger())

	if _, err := r.ResolveActiveDelegation(context.Background(), "subj-ivy"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	last := aud.Last()
	if last == nil {
		t.Fatal("audit emitted nothing")
	}
	if last.Category != AuditCategoryAuthorization {
		t.Errorf("category = %q, want authorization", last.Category)
	}
	if last.TargetType != "delegation" || last.TargetId != "v1:identity:delegation:zzz" {
		t.Errorf("target mismatch: type=%q id=%q", last.TargetType, last.TargetId)
	}
	if last.ActorIdentity != "v1:identity:identity:ivy" {
		t.Errorf("actor_identity = %q, want v1:identity:identity:ivy", last.ActorIdentity)
	}
	if last.Detail["expires_at"] == "" {
		t.Error("audit detail should carry expires_at")
	}
}
