package memql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestOrganizationBoundaryOverridesCreatorAndAppliesToSubscription(t *testing.T) {
	const concept = "v1:campaigns:campaign"
	accountFixture(t, concept, "accountId")
	ctx := accountCtx(t, "member", accountScopeWith("acme"))
	ownButOtherOrg := rowPayload(t, map[string]any{"ownerUserId": "member", "accountId": "beta"})
	for _, write := range []bool{false, true} {
		if got := rowAuthzAdmitsMode(ctx, concept, "row", ownButOtherOrg, write); got != rowAuthzDeny {
			t.Fatalf("creator bypassed org boundary, write=%v", write)
		}
	}
	ac, _ := auth.AccessFromContext(ctx)
	if AdmitSubscriptionRow(ctx, ac, concept, "row", ownButOtherOrg) != SubscriptionDeny {
		t.Fatal("subscription leaked cross-org creator row")
	}
	sameOrg := rowPayload(t, map[string]any{"ownerUserId": "colleague", "accountId": "acme"})
	if rowAuthzAdmits(ctx, concept, "row", sameOrg) != rowAuthzAdmit {
		t.Fatal("member cannot collaborate in authorized org")
	}
	legacy := rowPayload(t, map[string]any{"ownerUserId": "member"})
	if rowAuthzAdmits(ctx, concept, "legacy", legacy) != rowAuthzAdmit {
		t.Fatal("legacy private record disappeared")
	}
}

func TestOrganizationDefaultNeverSelectsAnotherOrganization(t *testing.T) {
	for _, tc := range []struct {
		operator bool
		ids      []string
		want     string
		fail     bool
	}{
		{true, nil, "self", false}, {true, []string{"acme", "beta"}, "self", false},
		{false, []string{"acme"}, "acme", false},
		{false, []string{"v1:accounts:account:acme", "acme"}, "acme", false},
		{false, nil, "", true}, {false, []string{"acme", "beta"}, "", true},
	} {
		got, err := organizationDefaultAccount(tc.operator, accountScopeWith(tc.ids...))
		if got != tc.want || (err != nil) != tc.fail {
			t.Fatalf("default %v/%v = %q,%v", tc.operator, tc.ids, got, err)
		}
	}
}

func TestOrganizationPlanBoundaryCompilesAndMatches(t *testing.T) {
	ctx := accountCtx(t, "member", accountScopeWith("acme"))
	engine := &MemQLEngine{}
	for _, rowID := range []bool{false, true} {
		n := &AccountScopeExpression{Field: "accountId", Organization: true, RowID: rowID, AllowUntied: !rowID}
		lowered := engine.accountScopeComparison(ctx, n)
		compiled, ok := engine.tryCompileCombinedFilter(ctx, lowered, "")
		if !ok {
			t.Fatal("org boundary would post-filter after pagination")
		}
		if rowID && !strings.Contains(compiled.sql, "id = ANY") {
			t.Fatal(compiled.sql)
		}
		if !rowID && !strings.Contains(compiled.sql, "jsonb_exists_any") {
			t.Fatal(compiled.sql)
		}
		for _, id := range []string{"acme", "beta"} {
			node := memorynodes.MemoryNode{ID: "v1:accounts:account:" + id, Payload: rowPayload(t, map[string]any{"accountId": id})}
			got := accountScopeMatchesNode(node, lowered.(*accountScopeMatch), map[string]map[string]any{})
			if got != (id == "acme") {
				t.Fatalf("rowID=%v id=%s admitted=%v", rowID, id, got)
			}
		}
	}
}

func TestOrganizationRawWriteCannotForgeAttribution(t *testing.T) {
	engine := &MemQLEngine{}
	ctx := accountCtx(t, "member", accountScopeWith("acme"))
	for _, concept := range []string{conceptAccountsAccount, conceptIdentityGroup, conceptIdentityGroupMembership} {
		if err := engine.validateOrganizationOwnership(ctx, concept, map[string]any{"accountId": "acme"}, false); err == nil {
			t.Fatalf("raw membership/organization administration allowed on %s", concept)
		}
	}
	err := engine.validateOrganizationOwnership(ctx, "v1:campaigns:campaign", map[string]any{"accountId": "beta"}, false)
	if err == nil || !strings.Contains(err.Error(), "organization_forbidden") {
		t.Fatalf("forged account: %v", err)
	}
	err = engine.validateOrganizationOwnership(ctx, "v1:campaigns:campaign", map[string]any{}, true)
	if err == nil || !strings.Contains(err.Error(), "organization_required") {
		t.Fatalf("implicit legacy transfer: %v", err)
	}
	if err := engine.validateOrganizationOwnership(context.Background(), "v1:work:goal", map[string]any{"accountIds": []string{"beta"}}, false); err != nil {
		t.Fatal("multi-account labels became sole ownership")
	}
}

func TestOrganizationReferencesRefuseMixedOrMissingOwnership(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{{"acme", "v1:accounts:account:acme", true}, {"acme", "beta", false}, {"acme", "", false}, {"", "acme", false}} {
		if organizationAccountsAgree(tc.a, tc.b) != tc.want {
			t.Fatalf("%q versus %q", tc.a, tc.b)
		}
	}
}

func TestOrganizationTargetBindingDoesNotChangeUserAuthoredMutationContracts(t *testing.T) {
	fn := &Function{Name: "mySiteLifecycle", FunctionKind: "mutation", BoundConcept: "v1:platform:site", RequiresCapability: CapabilityRequirement{Verb: "execute", Resource: "app:deployables/publish"}}
	if concept, argument := organizationCapabilityTarget(fn); concept != "" || argument != "" {
		t.Fatal("custom DSL mutation acquired a product-specific argument contract")
	}
}

// A write to a row that exists in an organization the caller may not change is
// refused with an exported value, so that a caller can say it in words of its own
// (a template id another organization holds) without matching the text, and its
// words are what they always were for anyone who reads them.
func TestAWriteToARowInAnotherOrganizationIsTheExportedRefusal(t *testing.T) {
	engine := &MemQLEngine{}
	ctx := accountCtx(t, "member", accountScopeWith("acme"))
	prior := map[string]any{"accountId": "beta"}

	err := engine.validateOrganizationTransfer(ctx, "v1:campaigns:template", "t1", prior, map[string]any{"accountId": "acme"})
	if !errors.Is(err, ErrOrganizationRowForbidden) {
		t.Fatalf("a member of acme writing over beta's row: %v, want ErrOrganizationRowForbidden", err)
	}
	const words = "organization_forbidden: you cannot change data in the existing organization"
	if err.Error() != words {
		t.Errorf("the refusal says %q, want %q", err.Error(), words)
	}
	// A concept that is not organization-owned is not judged by it.
	if err := engine.validateOrganizationTransfer(ctx, "v1:common:agent", "a1", prior, map[string]any{"accountId": "acme"}); err != nil {
		t.Errorf("a concept no organization owns was refused: %v", err)
	}
}
