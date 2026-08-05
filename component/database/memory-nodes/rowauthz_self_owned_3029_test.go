package memoryNodes

import (
	"strings"
	"testing"

	parser "github.com/znasllc-io/memql/component/language/parser"
)

// rowauthz_self_owned_3029_test.go -- memql#3029.
//
// `@rowAuthz(owner="F")` could only name a payload field the concept declares,
// so a SELF-OWNED concept -- one whose owner is the row's own identity --
// could not declare a tier at all. That excluded `v1:identity:user`: it
// declares no `userId`, no `ownerUserId`, no `accountId`, because a user's
// owner IS the row. So the concept holding the cluster-wide `role` was
// structurally unable to come under memql#2982's owner-field provenance gate.
//
// Ruled by the operator in memql#2803: add an `owner="id"` form, chosen over a
// new tier keyword because it needs no new vocabulary and reads identically.
//
// STATIC ONLY. This changes what can be declared and checked, never what is
// enforced -- TestRowAuthzIsInert stays green and unmodified.

func TestValidateRowAuthz_AcceptsSelfOwnedId(t *testing.T) {
	// A concept with NO payload property named `id` -- which is every
	// concept, since `id` is a reserved row intrinsic.
	props := []parsedProperty{{name: "displayName"}, {name: "role"}}

	if err := validateRowAuthz("v1:identity:user",
		&parser.RowAuthzDecl{Tier: parser.RowAuthzOwned, Owner: "id"}, props); err != nil {
		t.Fatalf("@rowAuthz(owner=\"id\") was rejected. A self-owned concept's owner IS the row, "+
			"and `id` is a row intrinsic no concept can declare as a property -- so requiring a "+
			"declared property makes the tier unexpressible for v1:identity:user, the highest-value "+
			"concept in the tree (memql#3029): %v", err)
	}
}

// TestValidateRowAuthz_AdmitsIdOnly is the scoping decision, recorded as a
// test rather than only as prose.
//
// `id` only. `createdBy` looks similar and is not: it means "who wrote the
// row", not "whose row it is". A row created by an admin on a user's behalf
// has a createdBy that is not the owner, so admitting it would let a concept
// declare an owner tier that is false -- exactly the class of false
// declaration memql#2982 exists to catch.
func TestValidateRowAuthz_AdmitsIdOnly(t *testing.T) {
	props := []parsedProperty{{name: "displayName"}}

	for _, owner := range []string{"createdBy", "createdAt", "concept", "type", "partition"} {
		err := validateRowAuthz("v1:identity:user",
			&parser.RowAuthzDecl{Tier: parser.RowAuthzOwned, Owner: owner}, props)
		if err == nil {
			t.Errorf("@rowAuthz(owner=%q) was accepted. Only `id` names the row's own identity; "+
				"every other intrinsic means something else, and `createdBy` in particular means "+
				"\"who wrote the row\" rather than \"whose row it is\" -- declaring it as the owner "+
				"is a false declaration (memql#3029).", owner)
		}
	}
}

// TestValidateRowAuthz_UndeclaredFieldStillRejected pins that the normal case
// is untouched, including its message. The gate's value comes from rejecting a
// typo, and widening it for `id` must not blunt that.
func TestValidateRowAuthz_UndeclaredFieldStillRejected(t *testing.T) {
	props := []parsedProperty{{name: "displayName"}, {name: "role"}}

	err := validateRowAuthz("v1:some:thing",
		&parser.RowAuthzDecl{Tier: parser.RowAuthzOwned, Owner: "ownerUserIdd"}, props)
	if err == nil {
		t.Fatal("a misspelled owner field was accepted; the gate's whole value is catching this")
	}
	if !strings.Contains(err.Error(), "does not declare") {
		t.Errorf("the existing diagnostic must stay intact for the normal case, got: %v", err)
	}
	if !strings.Contains(err.Error(), "displayName") {
		t.Errorf("the diagnostic should still list the declared fields, got: %v", err)
	}

	// And a declared field still passes.
	if err := validateRowAuthz("v1:some:thing",
		&parser.RowAuthzDecl{Tier: parser.RowAuthzOwned, Owner: "role"}, props); err != nil {
		t.Errorf("a declared property must still be accepted: %v", err)
	}
}

// TestParseRowAuthz_OwnerIdParses is acceptance criterion 1.
//
// The parser accepts any non-empty owner string, so this may already hold --
// which is exactly why it needs an assertion. "Already true" and "true by
// accident" look identical until someone tightens the parser.
func TestParseRowAuthz_OwnerIdParses(t *testing.T) {
	decl, err := parser.ParseRowAuthz(&parser.Attribute{
		Name: parser.RowAuthzAnnotation,
		Args: map[string]any{"owner": "id"},
	})
	if err != nil {
		t.Fatalf("@rowAuthz(owner=\"id\") must parse: %v", err)
	}
	if decl.Tier != parser.RowAuthzOwned {
		t.Errorf("Tier = %q, want %q", decl.Tier, parser.RowAuthzOwned)
	}
	if decl.Owner != "id" {
		t.Errorf("Owner = %q, want \"id\"", decl.Owner)
	}
}
