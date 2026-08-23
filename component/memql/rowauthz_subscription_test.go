package memql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// Subscriptions are an EGRESS OF ROWS, and until memql#4309 they were the
// one egress that never asked (design section 1).
//
// The engine has exactly one function for "may this caller see this row",
// rowAuthzAdmits. It admits or denies on a raw client query string, on
// graph expansion, on a top-level builtin result, and -- through the write
// guard -- on update and delete. `handleBusEvent` matched an event's topic
// against each subscription's patterns and sent the whole flattened
// payload. For the 31 concepts that declare a tier that is a real leak: a
// read denies the row and the subscription delivers it.
//
// This file is the seam's measurement. component/grpc holds the wiring
// test; the decision lives here, beside the function it defers to, so the
// gate cannot be moved away from the rule it implements.

// subscriptionFixtures registers one concept per tier and restores the
// registry afterwards.
//
// Fixtures rather than tree concepts for the tiers no concept declares --
// `public`, `granted` and the composite are declared by nothing today, and
// `granted` is the whole reason the id-only path exists. The two tiers the
// tree DOES declare are named from the tree so the common case is measured
// against a real declaration.
func subscriptionFixtures(t *testing.T) {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	before := memorynodes.All()
	memorynodes.MergeAll(map[string]*memorynodes.Concept{
		subPublicConcept: {
			Name: subPublicConcept, NodeType: "probe",
			RowAuthz: &langparser.RowAuthzDecl{Tier: langparser.RowAuthzPublic},
		},
		subGrantedConcept: {
			Name: subGrantedConcept, NodeType: "probe",
			RowAuthz: &langparser.RowAuthzDecl{Tier: langparser.RowAuthzGranted, Spec: "spaceMember"},
		},
		subCompositeConcept: {
			Name: subCompositeConcept, NodeType: "probe",
			RowAuthz: &langparser.RowAuthzDecl{
				Tier: langparser.RowAuthzOwned, Owner: "ownerUserId", ClusterOwnerBypass: true,
			},
		},
		subUndeclaredConcept: {Name: subUndeclaredConcept, NodeType: "probe"},
	})
	t.Cleanup(func() { memorynodes.ReplaceAll(before) })

	// POSITIVE CONTROL. An undeclared concept admits everything, so if the
	// fixtures did not land every assertion below would pass by admitting.
	for _, name := range []string{subPublicConcept, subGrantedConcept, subCompositeConcept} {
		if rowAuthzDeclFor(name) == nil {
			t.Fatalf("fixture %s is not registered; the tier assertions below would measure an "+
				"UNDECLARED concept, which admits everything and passes for the wrong reason", name)
		}
	}
	if rowAuthzDeclFor(declaredOwnedConcept) == nil {
		t.Fatalf("%s no longer declares a tier; this file's owned-tier case measures nothing",
			declaredOwnedConcept)
	}
}

const (
	subPublicConcept     = "v1:subfixture:publicrow"
	subGrantedConcept    = "v1:subfixture:grantedrow"
	subCompositeConcept  = "v1:subfixture:compositerow"
	subUndeclaredConcept = "v1:subfixture:undeclaredrow"
)

func subPayload(t *testing.T, m map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func writerAccess(userId string) *auth.AccessContext {
	return &auth.AccessContext{UserId: userId, Role: auth.RoleWriter}
}

func ownerAccess(userId string) *auth.AccessContext {
	return &auth.AccessContext{UserId: userId, Role: auth.RoleOwner}
}

// TestSubscriptionFanOutAppliesTheRowGate is the named evidence the
// @rowAuthz doc and per-row-authz-audit.md cite (rowauthz_doc_gate_test.go).
//
// One write per tier, two actors: the row reaches a subscribed stream only
// if the same function that admits it on a read admits it for that stream.
func TestSubscriptionFanOutAppliesTheRowGate(t *testing.T) {
	subscriptionFixtures(t)
	ownedDecl := declFor(t, declaredOwnedConcept)

	cases := []struct {
		name    string
		concept string
		id      string
		payload map[string]any
		access  *auth.AccessContext
		want    SubscriptionAdmission
	}{
		{"owned: the owner receives it", declaredOwnedConcept, declaredOwnedConcept + ":a",
			map[string]any{ownedDecl.Owner: "user-a"}, writerAccess("user-a"), SubscriptionAdmit},
		{"owned: a stranger does not", declaredOwnedConcept, declaredOwnedConcept + ":a",
			map[string]any{ownedDecl.Owner: "user-a"}, writerAccess("user-b"), SubscriptionDeny},
		{"owned: an unauthenticated stream does not", declaredOwnedConcept, declaredOwnedConcept + ":a",
			map[string]any{ownedDecl.Owner: "user-a"}, nil, SubscriptionDeny},

		{"clusterOwner: the cluster owner receives it", declaredClusterOwnerConcept,
			declaredClusterOwnerConcept + ":c", map[string]any{"number": "+15550000"},
			ownerAccess("root"), SubscriptionAdmit},
		{"clusterOwner: a reader does not", declaredClusterOwnerConcept,
			declaredClusterOwnerConcept + ":c", map[string]any{"number": "+15550000"},
			writerAccess("user-a"), SubscriptionDeny},

		{"public: everyone receives it", subPublicConcept, subPublicConcept + ":p",
			map[string]any{"x": 1}, writerAccess("user-b"), SubscriptionAdmit},
		{"undeclared: everyone receives it", subUndeclaredConcept, subUndeclaredConcept + ":u",
			map[string]any{"x": 1}, writerAccess("user-b"), SubscriptionAdmit},

		{"composite: the owner receives it", subCompositeConcept, subCompositeConcept + ":k",
			map[string]any{"ownerUserId": "user-a"}, writerAccess("user-a"), SubscriptionAdmit},
		{"composite: a cluster owner receives it", subCompositeConcept, subCompositeConcept + ":k",
			map[string]any{"ownerUserId": "user-a"}, ownerAccess("root"), SubscriptionAdmit},
		{"composite: a third user does not", subCompositeConcept, subCompositeConcept + ":k",
			map[string]any{"ownerUserId": "user-a"}, writerAccess("user-c"), SubscriptionDeny},

		{"granted: id-only, the client re-reads", subGrantedConcept, subGrantedConcept + ":g",
			map[string]any{"spaceId": "s1"}, writerAccess("user-a"), SubscriptionIdOnly},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AdmitSubscriptionRow(context.Background(), tc.access,
				tc.concept, tc.id, subPayload(t, tc.payload))
			if got != tc.want {
				t.Fatalf("AdmitSubscriptionRow(%s) = %v, want %v.\n"+
					"A subscription is an egress of rows and must reach the SAME verdict the read "+
					"path reaches for this actor -- one seam, not a second rulebook (design D1).",
					tc.concept, got, tc.want)
			}
		})
	}
}

// v1:identity:user is UNDECLARED and carries @pii, so the read path
// narrows it on an unbound read even though no tier gates it
// (rowauthz_pii_unbound.go). The subscription seam has to narrow it the
// same way, or subscribing to the concept hands every user's PII fields to
// any signed-in stream -- which is memql#3350's generic-browse hole
// arriving through a different door.
func TestSubscriptionNarrowsUserRowsAsTheReadPathDoes(t *testing.T) {
	subscriptionFixtures(t)
	const userConcept = "v1:identity:user"
	if rowAuthzDeclFor(userConcept) != nil {
		t.Skipf("%s now declares a tier; this test measures the UNDECLARED PII narrowing", userConcept)
	}
	if !rowAuthzConceptCarriesPII(userConcept) {
		t.Fatalf("%s declares no @pii field, so this test measures nothing", userConcept)
	}

	row := subPayload(t, map[string]any{"displayName": "Alice"})
	const aliceId = "v1:identity:user:alice"

	if got := AdmitSubscriptionRow(context.Background(), writerAccess("alice"), userConcept, aliceId, row); got != SubscriptionAdmit {
		t.Errorf("the subject was denied their own user row: %v", got)
	}
	if got := AdmitSubscriptionRow(context.Background(), ownerAccess("root"), userConcept, aliceId, row); got != SubscriptionAdmit {
		t.Errorf("a cluster owner was denied a user row: %v", got)
	}
	if got := AdmitSubscriptionRow(context.Background(), writerAccess("mallory"), userConcept, aliceId, row); got != SubscriptionDeny {
		t.Fatalf("AdmitSubscriptionRow handed another user's @pii row to a plain writer (%v). "+
			"The read path withholds it on an unbound read; a subscription that does not is the "+
			"same hole through a different door (memql#3350).", got)
	}
}

// The seam must mark its read UNBOUND. That is what engages the PII
// narrowing above -- rowAuthzPIIUnboundDenies' first clause returns false
// for a read that is not unbound, so an unstamped context would admit
// every user row and the test above would be the only thing to notice.
// Asserted directly so the REASON is pinned, not just the outcome.
func TestSubscriptionReadIsStampedUnbound(t *testing.T) {
	subscriptionFixtures(t)
	ctx := subscriptionReadContext(context.Background(), writerAccess("mallory"))
	if !rowAuthzReadIsUnbound(ctx) {
		t.Fatal("the subscription read context is not stamped UNBOUND. A subscription is the " +
			"generic-browse shape -- a client reading a concept's rows with no named construct " +
			"behind it -- and the PII narrowing only engages on an unbound read.")
	}
	if auth.OriginFromContext(ctx).IsInternal() {
		t.Fatal("the subscription read context claims INTERNAL origin. A subscription is a client " +
			"egress; internal origin would skip the PII narrowing entirely.")
	}
}

// An update() emits `created` and `updated`. Both carry the same row, so
// both must reach the same verdict -- a row that is denied on one and
// delivered on the other leaks through whichever the client happens to be
// subscribed to (design section 6 item 2).
func TestSubscriptionVerdictIsStableAcrossActionsOfOneRow(t *testing.T) {
	subscriptionFixtures(t)
	decl := declFor(t, declaredOwnedConcept)
	payload := subPayload(t, map[string]any{decl.Owner: "user-a"})
	id := declaredOwnedConcept + ":same"

	for _, access := range []*auth.AccessContext{writerAccess("user-a"), writerAccess("user-b"), nil} {
		created := AdmitSubscriptionRow(context.Background(), access, declaredOwnedConcept, id, payload)
		updated := AdmitSubscriptionRow(context.Background(), access, declaredOwnedConcept, id, payload)
		if created != updated {
			t.Fatalf("the same row reached different verdicts on created (%v) and updated (%v)",
				created, updated)
		}
	}
}
