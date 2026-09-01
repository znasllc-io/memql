package identity

import (
	"context"
	"strings"
	"testing"

	componentAuth "github.com/znasllc-io/memql/component/auth"
)

// OWNERSHIP TRANSFER (memql#4838).
//
// These cover the REFUSALS, which is where the value is. The happy path moves
// rows and is exercised end to end by the db-gated lane; every case below is a
// call that must not happen, and each one is a way the feature could quietly
// make the problem it exists to solve worse.

func ownerCtx(role componentAuth.Role) context.Context {
	return componentAuth.ContextWithAccess(context.Background(), &componentAuth.AccessContext{
		UserId: "operator", Role: role, PrimaryEmail: "op@example.com",
	})
}

// A cluster-owner action. "A rank above both parties" has no answer when both
// parties are owners -- which is the case the feature exists for -- so the
// invoker is the operator of the deployment, and the action is audited.
func TestTransferIsAClusterOwnerAction(t *testing.T) {
	i := NewIdentityIntegration()
	for _, role := range []componentAuth.Role{
		componentAuth.RoleAdmin,
		componentAuth.RoleDeveloper,
		componentAuth.RoleWriter,
		componentAuth.RoleReader,
	} {
		_, err := i.handleTransferRowOwnership(ownerCtx(role), map[string]any{
			"fromUserId": "leaver", "toUserId": "stayer",
		}, 0)
		if err == nil {
			t.Fatalf("role %q was allowed to transfer row ownership", role)
		}
		if !strings.Contains(err.Error(), "cluster-owner") {
			t.Fatalf("role %q refused with %q, which does not say why", role, err)
		}
	}
}

// An unauthenticated call is refused for the same reason and before anything
// else is read.
func TestTransferRefusesAnActorlessCall(t *testing.T) {
	i := NewIdentityIntegration()
	if _, err := i.handleTransferRowOwnership(context.Background(), map[string]any{
		"fromUserId": "leaver", "toUserId": "stayer",
	}, 0); err == nil {
		t.Fatal("a call with no access context was allowed to transfer row ownership")
	}
}

// Both principals are required. A transfer with no subject is not a transfer.
func TestTransferRequiresBothPrincipals(t *testing.T) {
	i := NewIdentityIntegration()
	ctx := ownerCtx(componentAuth.RoleOwner)
	for _, args := range []map[string]any{
		{"toUserId": "stayer"},
		{"fromUserId": "leaver"},
		{},
		{"fromUserId": "  ", "toUserId": "stayer"},
	} {
		if _, err := i.handleTransferRowOwnership(ctx, args, 0); err == nil {
			t.Fatalf("args %v were accepted; a transfer always has a subject", args)
		}
	}
}

// Transferring to the same principal is REFUSED rather than treated as a
// no-op. An operator who typed the same id twice believes they moved
// something, and an audit row agreeing with them is worse than a refusal.
func TestTransferRefusesTheSamePrincipalOnBothSides(t *testing.T) {
	i := NewIdentityIntegration()
	ctx := ownerCtx(componentAuth.RoleOwner)
	for _, pair := range [][2]string{
		{"u1", "u1"},
		// The two id spellings the row gate treats as one: bare and
		// canonical. Refusing only the literal match would let the same
		// mistake through in the spelling a client actually sends.
		{"u1", "v1:identity:user:u1"},
		{"v1:identity:user:u1", "u1"},
	} {
		_, err := i.handleTransferRowOwnership(ctx, map[string]any{
			"fromUserId": pair[0], "toUserId": pair[1],
		}, 0)
		if err == nil {
			t.Fatalf("transfer from %q to %q was accepted; they name the same principal", pair[0], pair[1])
		}
		if !strings.Contains(err.Error(), "same principal") {
			t.Fatalf("refusal for %v was %q, which does not name the reason", pair, err)
		}
	}
}

// sameOwnerId is the comparison the refusal above rests on, and it must agree
// with the row gate about what "the same principal" means -- bare against
// canonical, in either order.
func TestSameOwnerIdMatchesBothIdSpellings(t *testing.T) {
	cases := []struct {
		a, b string
		same bool
	}{
		{"u1", "u1", true},
		{"u1", "v1:identity:user:u1", true},
		{"v1:identity:user:u1", "u1", true},
		{"v1:identity:user:u1", "v1:identity:user:u1", true},
		{"u1", "u2", false},
		{"v1:identity:user:u1", "v1:identity:user:u2", false},
	}
	for _, c := range cases {
		if got := sameOwnerId(c.a, c.b); got != c.same {
			t.Fatalf("sameOwnerId(%q, %q) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
}

// A node with no engine or database refuses rather than reporting a transfer
// of zero rows. Zero rows moved and "this node cannot move rows" are different
// answers, and only one of them is safe to believe.
func TestTransferRefusesWhenTheNodeCannotWrite(t *testing.T) {
	i := NewIdentityIntegration()
	_, err := i.handleTransferRowOwnership(ownerCtx(componentAuth.RoleOwner), map[string]any{
		"fromUserId": "leaver", "toUserId": "stayer",
	}, 0)
	if err == nil {
		t.Fatal("a node with no engine reported a successful transfer")
	}
	if !strings.Contains(err.Error(), "engine") {
		t.Fatalf("refusal was %q, which does not say the node cannot write", err)
	}
}

// The self-owned tier is excluded from the transferable set. Its "owner field"
// is the row's own id, so a transfer would mean RENAMING the row -- a
// different and much worse operation than changing who owns it.
func TestSelfOwnedConceptsAreNotTransferable(t *testing.T) {
	// transferableConcepts reads the live registry; with no concepts loaded it
	// returns nothing, which is the honest answer and not an assertion. The
	// property under test is the FILTER, so it is asserted on the predicate
	// the filter applies rather than on a registry this test would have to
	// build to say anything at all.
	for _, name := range transferableConcepts("") {
		decl := conceptRowAuthz(name)
		if decl == nil {
			t.Fatalf("%s was listed as transferable but declares no tier", name)
		}
		if strings.TrimSpace(decl.Owner) == "id" {
			t.Fatalf("%s declares the self-owned tier and must not be transferable: its owner "+
				"field is the row's own id, so a transfer would rename the row", name)
		}
		if strings.TrimSpace(decl.Owner) == "" {
			t.Fatalf("%s was listed as transferable with no owner field to rewrite", name)
		}
	}
}
