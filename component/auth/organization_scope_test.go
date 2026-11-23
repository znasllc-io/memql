package auth

import (
	"context"
	"testing"
	"time"
)

func TestOrganizationRoleCannotGainClusterAuthorityThroughRankOrGrants(t *testing.T) {
	cat := assignmentCatalog()
	cat.ranks["acct-lead"] = 350
	for _, resource := range []string{ResourcePrincipal, ResourceRole, ResourceAdmission, ResourceDeployment, ResourceConstruct, "app:settings/cluster", "app:cluster", ResourceGroup, ResourceData, "app:email"} {
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
	for _, resource := range []string{ResourceGroup, ResourceData, "app:email"} {
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

// AN ORGANIZATION ROLE ADDS ITS HOLDER'S OWN API KEY (owner decision
// 2026-11-22). When organization roles were barred from Settings' parts,
// every one of them was cluster configuration. The API keys part is not: it
// is a person's own key, paid with their own money, and the owner's rule is
// that model access is bring-your-own for everybody. So that one part is not
// barred. A role written with it holds it, and a person granted it by name
// holds it. Every other part of Settings stays barred to an organization
// role, whatever its rows or a grant say, the cluster's vendor account among
// them.
func TestAnOrganizationRoleMayHoldTheApiKeysPartAndNoOtherNewPartOfSettings(t *testing.T) {
	const apiKeys = "app:settings/apikeys"
	stillBarred := []string{"app:settings/providers", "app:settings/clusteraccount", "app:settings/keys", "app:settings/cluster"}

	cat := assignmentCatalog()
	cat.ranks["acct-member"] = 90
	cat.scopes["acct-member"] = "acct-1"
	cat.grants["acct-member"] = map[VerbResource]bool{}
	// The lead's ROLE was written with opening Settings and adding a key, and
	// with parts an organization role may not use.
	cat.grants["acct-lead"][VerbResource{Verb: VerbRead, Resource: "app:settings"}] = true
	cat.grants["acct-lead"][VerbResource{Verb: VerbExecute, Resource: apiKeys}] = true
	for _, part := range stillBarred {
		cat.grants["acct-lead"][VerbResource{Verb: VerbExecute, Resource: part}] = true
	}
	installFake(t, cat)
	// The member's role holds nothing in Settings: grants name the person.
	byName := []Grant{allow(VerbExecute, apiKeys)}
	for _, part := range stillBarred {
		byName = append(byName, allow(VerbExecute, part))
	}
	installGrantFake(t, &countingGrantSource{user: map[string][]Grant{
		"u-member": byName,
		"u-denied": {deny(VerbExecute, apiKeys)},
	}})
	ctx := context.Background()

	holders := map[string]Subject{
		"a person whose organization role holds the part": {Role: "acct-lead", UserId: "u-lead"},
		"a person with an organization role, granted it":  {Role: "acct-member", UserId: "u-member"},
	}
	for who, subject := range holders {
		if d := DecideFor(ctx, subject, VerbExecute, apiKeys); !d.Held || d.Barred {
			t.Errorf("%s may not add a key of their own: held %v, barred %v", who, d.Held, d.Barred)
		}
		for _, part := range stillBarred {
			if d := DecideFor(ctx, subject, VerbExecute, part); d.Held || !d.Barred {
				t.Errorf("%s reached %s: held %v, barred %v", who, part, d.Held, d.Barred)
			}
		}
		// What the OS reads to decide what to draw agrees with the gate.
		for _, d := range EffectiveCapabilities(ctx, subject) {
			if d.Verb == VerbExecute && d.Resource == apiKeys && (!d.Held || d.Barred) {
				t.Errorf("%s: the listing says held %v, barred %v for the API keys part", who, d.Held, d.Barred)
			}
		}
	}

	// Opening Settings itself was never barred; the part is of no use without it.
	if !CapableFor(ctx, holders["a person whose organization role holds the part"], VerbRead, "app:settings") {
		t.Error("an organization role written with Settings cannot open it")
	}
	// An organization role that was given nothing holds nothing: lifting the
	// bar hands out no permission.
	if CapableFor(ctx, Subject{Role: "acct-member", UserId: "u-other"}, VerbExecute, apiKeys) {
		t.Error("an organization role nobody gave the part to holds it")
	}
	// And an operator's deny still withholds it from one person.
	if CapableFor(ctx, Subject{Role: "acct-lead", UserId: "u-denied"}, VerbExecute, apiKeys) {
		t.Error("a person denied the part by name holds it through their organization role")
	}
}
