package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// rowauthz_write_test.go -- memql#3079, Phase 5.

// conceptWithTier builds a concept carrying a row-authz declaration.
func conceptWithTier(name string, decl *langparser.RowAuthzDecl) *memorynodes.Concept {
	return &memorynodes.Concept{Name: name, RowAuthz: decl}
}

func ownedConcept() *memorynodes.Concept {
	return conceptWithTier("v1:notes:note", &langparser.RowAuthzDecl{
		Tier: langparser.RowAuthzOwned, Owner: "ownerUserId",
	})
}

func rowAuthzActorCtx(userId string, role auth.Role) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userId, Role: role})
}

// THE HEADLINE: a caller updating another user's row is refused; the owner is
// unaffected. This is the behaviour change the ruling authorises.
func TestWriteGuardRefusesForeignOwnerAndAllowsOwner(t *testing.T) {
	e := &MemQLEngine{}
	prior := map[string]any{"ownerUserId": "u-alice", "body": "alice's note"}

	t.Run("foreign caller is refused", func(t *testing.T) {
		_, err := e.assertRowAuthzWrite(rowAuthzActorCtx("u-mallory", auth.RoleWriter),
			ownedConcept(), "v1:notes:note:n1", prior, true, true)
		if err == nil {
			t.Fatal("a caller updating another user's row must be refused (memql#3079)")
		}
		// The refusal must NOT echo the row's owner back -- that would turn it
		// into an ownership oracle over rows the caller cannot read.
		if strings.Contains(err.Error(), "u-alice") {
			t.Errorf("the refusal leaks the row's owner: %v", err)
		}
	})

	t.Run("the owner is unaffected", func(t *testing.T) {
		decision, err := e.assertRowAuthzWrite(rowAuthzActorCtx("u-alice", auth.RoleWriter),
			ownedConcept(), "v1:notes:note:n1", prior, true, true)
		if err != nil {
			t.Fatalf("the legitimate owner must be unaffected: %v", err)
		}
		if decision != writeAuthzOwner {
			t.Errorf("decision = %q, want %q", decision, writeAuthzOwner)
		}
	})
}

// A CREATE must not be refused. This is the bug that would have turned the
// guard into an outage: a genuine create reaches the shared write chokepoint
// with existed == false, and refusing it would break every create on a
// declared concept.
func TestWriteGuardDoesNotRefuseACreate(t *testing.T) {
	e := &MemQLEngine{}
	decision, err := e.assertRowAuthzWrite(rowAuthzActorCtx("u-alice", auth.RoleWriter),
		ownedConcept(), "v1:notes:note:new", nil,
		false, // existed: no prior row
		false, // requirePrior: insert() path
	)
	if err != nil {
		t.Fatalf("a create has no target row to own and must not be refused: %v\n"+
			"Guarding a create's ownership STAMP is memql#3059, a different problem.", err)
	}
	if decision != writeAuthzNoDeclaration {
		t.Errorf("decision = %q, want the create passthrough", decision)
	}
}

// A missing target row on the UPDATE path must not read as authorized. This is
// the fail-open direction, and #2982's analyzer had to be fixed once for
// exactly it.
func TestWriteGuardRefusesAMissingTargetOnUpdate(t *testing.T) {
	e := &MemQLEngine{}
	_, err := e.assertRowAuthzWrite(rowAuthzActorCtx("u-alice", auth.RoleWriter),
		ownedConcept(), "v1:notes:note:gone", nil,
		false, // existed: no such row
		true,  // requirePrior: update() path
	)
	if err == nil {
		t.Fatal("a missing target row must not authorize the write (memql#3079)")
	}
}

// clusterOwner bypasses; admin DOES NOT. Stated rather than inferred, per the
// ruling's constraint 2 -- and asserted, because "we decided admin does not
// bypass" is worth exactly as much as the test that proves it.
func TestClusterOwnerBypassesAndAdminDoesNot(t *testing.T) {
	e := &MemQLEngine{}
	prior := map[string]any{"ownerUserId": "u-alice"}

	decision, err := e.assertRowAuthzWrite(rowAuthzActorCtx("u-root", auth.RoleOwner),
		ownedConcept(), "v1:notes:note:n1", prior, true, true)
	if err != nil {
		t.Fatalf("the cluster owner must bypass: %v", err)
	}
	if decision != writeAuthzClusterOwner {
		t.Errorf("decision = %q, want %q", decision, writeAuthzClusterOwner)
	}

	if _, err := e.assertRowAuthzWrite(rowAuthzActorCtx("u-admin", auth.RoleAdmin),
		ownedConcept(), "v1:notes:note:n1", prior, true, true); err == nil {
		t.Error("admin must NOT bypass the row-owner guard.\n" +
			"The declaration vocabulary has a clusterOwner tier and no admin tier, so an admin " +
			"bypass grants a power the DSL cannot express and therefore cannot declare, review " +
			"or revoke per concept. memql#2991 is the worked example of a role-based write " +
			"bypass going wrong (memql#3079)")
	}
}

// The internal-origin escape is scoped to ONE write, not to a request.
// memql#2989 built and REFUTED the per-request shape; this proves the escape
// this guard honours does not leak to the next construct.
func TestInternalOriginEscapeIsPerWriteNotPerRequest(t *testing.T) {
	e := &MemQLEngine{}
	prior := map[string]any{"ownerUserId": "u-alice"}

	// A request-derived context for a caller who owns nothing here.
	requestCtx := rowAuthzActorCtx("u-mallory", auth.RoleWriter)

	// The trusted caller stamps internal origin on a context IT constructs,
	// for one operation.
	oneWrite := auth.ContextWithInternalOrigin(requestCtx)
	if _, err := e.assertRowAuthzWrite(oneWrite, ownedConcept(), "v1:notes:note:n1", prior, true, true); err != nil {
		t.Fatalf("an internal-origin write must be allowed: %v", err)
	}

	// The NEXT construct in the same request uses the request context, which
	// was never stamped. It must still be refused.
	if _, err := e.assertRowAuthzWrite(requestCtx, ownedConcept(), "v1:notes:note:n1", prior, true, true); err == nil {
		t.Fatal("the internal-origin escape leaked to the next construct in the same request.\n" +
			"memql#2989 refuted exactly this: stamping internal origin on a REQUEST-derived " +
			"context opens every guarded construct for the rest of that request. The escape must " +
			"be scoped to the single write it authorizes (memql#3079)")
	}
}

// No authenticated actor and not internal: fail CLOSED.
func TestWriteGuardFailsClosedWithNoActor(t *testing.T) {
	e := &MemQLEngine{}
	prior := map[string]any{"ownerUserId": "u-alice"}
	if _, err := e.assertRowAuthzWrite(context.Background(),
		ownedConcept(), "v1:notes:note:n1", prior, true, true); err == nil {
		t.Fatal("an unauthenticated write to a concept declaring an owner must be refused")
	}
}

// A concept that declares nothing is untouched -- 88 of 101 concepts today.
func TestUndeclaredConceptIsNotGuarded(t *testing.T) {
	e := &MemQLEngine{}
	decision, err := e.assertRowAuthzWrite(rowAuthzActorCtx("u-mallory", auth.RoleWriter),
		&memorynodes.Concept{Name: "v1:nothing:declared"}, "v1:nothing:declared:x",
		map[string]any{"ownerUserId": "u-alice"}, true, true)
	if err != nil {
		t.Fatalf("an undeclared concept must not be guarded: %v", err)
	}
	if decision != writeAuthzNoDeclaration {
		t.Errorf("decision = %q, want %q", decision, writeAuthzNoDeclaration)
	}
}

// Tier behaviours that are not the owned path.
func TestOtherTiersOnTheWritePath(t *testing.T) {
	e := &MemQLEngine{}
	ctx := rowAuthzActorCtx("u-alice", auth.RoleWriter)

	t.Run("public names no owner, so nothing to enforce", func(t *testing.T) {
		decision, err := e.assertRowAuthzWrite(ctx,
			conceptWithTier("v1:x:pub", &langparser.RowAuthzDecl{Tier: langparser.RowAuthzPublic}),
			"v1:x:pub:1", map[string]any{}, true, true)
		if err != nil {
			t.Fatalf("public must not be refused: %v", err)
		}
		if decision != writeAuthzPublicTier {
			t.Errorf("decision = %q, want %q", decision, writeAuthzPublicTier)
		}
	})

	t.Run("clusterOwner tier refuses a non-owner", func(t *testing.T) {
		if _, err := e.assertRowAuthzWrite(ctx,
			conceptWithTier("v1:x:co", &langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}),
			"v1:x:co:1", map[string]any{}, true, true); err == nil {
			t.Error("a clusterOwner-tier concept must refuse a non-cluster-owner write")
		}
	})

	t.Run("granted fails closed until Phase 4", func(t *testing.T) {
		_, err := e.assertRowAuthzWrite(ctx,
			conceptWithTier("v1:x:g", &langparser.RowAuthzDecl{
				Tier: langparser.RowAuthzGranted, Spec: "isSpaceMember",
			}), "v1:x:g:1", map[string]any{}, true, true)
		if err == nil {
			t.Error("granted must fail CLOSED -- an unenforceable declaration must not read as " +
				"permission. No concept declares it today, so the blast radius is zero")
		}
	})

	t.Run("unrecognised tier fails closed", func(t *testing.T) {
		if _, err := e.assertRowAuthzWrite(ctx,
			conceptWithTier("v1:x:u", &langparser.RowAuthzDecl{Tier: langparser.RowAuthzTier("wat")}),
			"v1:x:u:1", map[string]any{}, true, true); err == nil {
			t.Error("an unrecognised tier must refuse the write")
		}
	})
}

// Ownership is read from the PERSISTED row, never from the caller's delta.
// Reading the delta would let a caller assert ownership in the very request
// that changes it -- which is why the guard sits before the read-merge.
func TestOwnershipIsReadFromTheStoredRow(t *testing.T) {
	e := &MemQLEngine{}
	// The stored row belongs to alice. A delta claiming mallory owns it is
	// irrelevant -- the guard never sees the delta.
	stored := map[string]any{"ownerUserId": "u-alice"}
	if _, err := e.assertRowAuthzWrite(rowAuthzActorCtx("u-mallory", auth.RoleWriter),
		ownedConcept(), "v1:notes:note:n1", stored, true, true); err == nil {
		t.Fatal("ownership must come from the stored row, not from anything the caller supplied")
	}
}

// A stored row missing the declared owner field fails CLOSED. "No owner
// recorded" must not mean "anyone may write".
func TestStoredRowWithoutTheOwnerFieldFailsClosed(t *testing.T) {
	e := &MemQLEngine{}
	for name, prior := range map[string]map[string]any{
		"field absent":     {"body": "x"},
		"field empty":      {"ownerUserId": ""},
		"field wrong type": {"ownerUserId": 42},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := e.assertRowAuthzWrite(rowAuthzActorCtx("u-alice", auth.RoleWriter),
				ownedConcept(), "v1:notes:note:n1", prior, true, true); err == nil {
				t.Error("a row with no usable owner value must fail closed")
			}
		})
	}
}

// SELF-OWNED (memql#3029): the row IS the owner, so identity comes from the
// row id rather than a payload field. The stored id is canonical
// (`{concept}:{shortId}`) while the actor's id is bare, so the comparison has
// to reduce one to the other.
func TestSelfOwnedComparesTheRowIdToTheActor(t *testing.T) {
	e := &MemQLEngine{}
	selfOwned := conceptWithTier("v1:identity:user", &langparser.RowAuthzDecl{
		Tier: langparser.RowAuthzOwned, Owner: langparser.RowAuthzSelfOwnedField,
	})

	if _, err := e.assertRowAuthzWrite(rowAuthzActorCtx("u_123", auth.RoleWriter),
		selfOwned, "v1:identity:user:u_123", map[string]any{}, true, true); err != nil {
		t.Errorf("a user updating their own row must be allowed: %v", err)
	}
	if _, err := e.assertRowAuthzWrite(rowAuthzActorCtx("u_456", auth.RoleWriter),
		selfOwned, "v1:identity:user:u_123", map[string]any{}, true, true); err == nil {
		t.Error("a user updating ANOTHER user's row must be refused")
	}
}
