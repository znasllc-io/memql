package node

// query_forward_badge_test.go -- does the badge role ceiling (memql#2513)
// survive the QueryForward hop?
//
// The sibling tests in query_forward_actor_test.go stub the resolver, which
// models "authority comes from the store, keyed by subject". That is the right
// property for the SUBJECT but it cannot see the ceiling at all: a badge
// grant's effective role is min(stored role, terminal ceiling), and the
// ceiling lives in the token's `class` / `role_ceiling` claims, not in the
// user row. Stubbing the resolver therefore asserts the unclamped store role
// is correct -- which is exactly the thing in question.
//
// This test uses the REAL *auth.IdentityResolver so applyBadgeRoleCeiling
// actually runs.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

const badgeOperator = "v1:identity:user:op-2814"

// badgeRowRunner returns a user row whose STORED role is owner. The operator
// is genuinely an owner; the badge grant is what must clamp them down.
type badgeRowRunner struct{}

func (badgeRowRunner) ExecuteShaped(_ context.Context, _ string) (any, error) {
	return []any{map[string]any{
		"id":           badgeOperator,
		"primaryEmail": "op@example.test",
		"role":         "owner",
	}}, nil
}

// TestHandleQueryForward_BadgeCeilingSurvivesTheHop pins the property that the
// forwarded hop must not escalate a badge session.
//
// Direct request: an owner badging into a reader kiosk is clamped to reader,
// so isClusterOwner is false. The forwarded hop must reach the same verdict.
// If it resolves the unclamped stored role instead, a peer-forwarded query
// executes as cluster owner on a surface that dispatches mutations and logic.
func TestHandleQueryForward_BadgeCeilingSurvivesTheHop(t *testing.T) {
	resolver := auth.NewIdentityResolver(badgeRowRunner{}, nil)

	// DIRECT: what component/grpc's ensureAccess computes for this session.
	direct, err := resolver.LoadFromClaims(context.Background(), map[string]any{
		"sub":          badgeOperator,
		"class":        "badge",
		"role_ceiling": "reader",
	})
	if err != nil {
		t.Fatalf("direct LoadFromClaims: %v", err)
	}
	if direct.Role != auth.RoleReader {
		t.Fatalf("precondition failed: direct role = %q, want reader", direct.Role)
	}
	if direct.IsClusterOwner() {
		t.Fatal("precondition failed: direct path must not be cluster owner")
	}

	// FORWARDED: the producer recipe this change prescribes, in the proto
	// comment on QueryForward.auth and in the handler's own refusal string.
	claims := auth.ForwardedClaimsFromIdentity(auth.UserIdentity{
		Subject: badgeOperator,
		Email:   "op@example.test",
		Role:    "owner",
	})

	exec := &capturingExecutor{}
	svc := newQueryForwardService(exec, resolver)
	stream := newFakeStream()

	svc.handleQueryForward("peer-a", &nodev1.QueryForward{
		RequestId: "req-badge",
		Query:     "query allNumbers",
		Auth:      claims,
	}, stream)

	if !exec.called {
		// Refusing is a perfectly good outcome: it fails closed.
		t.Skip("forward was refused rather than executed -- fails closed, nothing to escalate")
	}

	forwarded, ok := auth.AccessFromContext(exec.gotCtx)
	if !ok || forwarded == nil {
		t.Fatal("forwarded query executed with no AccessContext")
	}

	t.Logf("DIRECT    role=%q isClusterOwner=%v", direct.Role, direct.IsClusterOwner())
	t.Logf("FORWARDED role=%q isClusterOwner=%v", forwarded.Role, forwarded.IsClusterOwner())

	if forwarded.Role != direct.Role {
		t.Errorf("badge ceiling did not survive the hop: forwarded role = %q, direct role = %q -- "+
			"applyBadgeRoleCeiling keys on the `class` claim, and ForwardedClaimsFromIdentity "+
			"never emits it, so the receiver resolved the UNCLAMPED stored role (memql#2814, #2513)",
			forwarded.Role, direct.Role)
	}
	if forwarded.IsClusterOwner() && !direct.IsClusterOwner() {
		t.Errorf("PRIVILEGE ESCALATION across the mesh hop: forwarded isClusterOwner=true while "+
			"the direct path resolves false for the same session (forwarded role %q)", forwarded.Role)
	}
}
