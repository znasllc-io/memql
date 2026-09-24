package auth

import (
	"context"
	"testing"
	"time"
)

func TestOrganizationRoleCannotGainClusterAuthorityThroughRankOrGrants(t *testing.T) {
	cat := assignmentCatalog()
	cat.ranks["acct-lead"] = 350
	for _, resource := range []string{ResourcePrincipal, ResourceRole, ResourceAdmission, ResourceDeployment, ResourceConstruct, "app:settings/cluster", "app:cluster", ResourceGroup, ResourceData, "app:campaigns"} {
		cat.grants["acct-lead"][VerbResource{Verb: VerbUpdate, Resource: resource}] = true
	}
	installFake(t, cat)
	src := &countingGrantSource{user: map[string][]Grant{"u-lead": {allow(VerbUpdate, ResourceRole)}}}
	installGrantFake(t, src)
	ac := &AccessContext{UserId: "u-lead", Role: "acct-lead"}
	wire, err := ForwardedAuthorityForUser(ac, ForwardedClassUser, "", time.Time{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// The receiving node has only the verified wire identity. Scope is read
	// from its catalog, not from a local-origin request context or client id.
	remote, err := VerifyForwardedAuthority(wire, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	subject, ok := SubjectFromContext(ContextWithAccess(context.Background(), remote))
	if !ok {
		t.Fatal("forwarded subject missing")
	}
	if IsClusterOperator(remote.Role) {
		t.Fatal("scoped high-rank role became cluster operator")
	}
	for _, resource := range []string{ResourcePrincipal, ResourceRole, ResourceAdmission, ResourceDeployment, ResourceConstruct, "app:settings/cluster", "app:cluster"} {
		if CapableFor(context.Background(), subject, VerbUpdate, resource) {
			t.Errorf("scoped role gained %s", resource)
		}
	}
	for _, resource := range []string{ResourceGroup, ResourceData, "app:campaigns"} {
		if !CapableFor(context.Background(), subject, VerbUpdate, resource) {
			t.Errorf("lost explicitly granted org capability %s", resource)
		}
	}
	for _, d := range EffectiveCapabilities(context.Background(), subject) {
		if !ScopedRoleMayUse(subject.Role, d.Resource) && d.Held {
			t.Errorf("discovery disagrees for %s", d.Resource)
		}
	}
}
