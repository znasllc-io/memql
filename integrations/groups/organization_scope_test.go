package groups

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

type organizationMembershipSource struct{ groups []string }

func (s *organizationMembershipSource) ActiveGroupIdsForUser(context.Context, string) []string {
	return s.groups
}

type organizationGrantSource struct{}

func (organizationGrantSource) ActiveGrantsForUser(context.Context, string) ([]auth.Grant, error) {
	return []auth.Grant{{Verb: auth.VerbUpdate, Resource: auth.ResourceGroup, Effect: auth.GrantAllow}}, nil
}
func (organizationGrantSource) ActiveGrantsForGroups(context.Context, []string) ([]auth.Grant, error) {
	return nil, nil
}

func TestOrganizationGroupAdministrationIsBoundedAndReResolved(t *testing.T) {
	i, stub := newFixture(t)
	stub.groups["beta"] = map[string]any{"id": "beta", "accountId": "beta", "status": StatusActive, "kind": KindAccount}
	stub.users["u-reader"] = map[string]any{"id": "u-reader", "role": "reader"}
	stub.members["acct-acme"] = []any{map[string]any{"id": "existing-reader", "groupId": "acct-acme", "userId": "u-reader", "status": StatusActive}}
	ms := &organizationMembershipSource{groups: []string{"acct-acme"}}
	previousMembership := auth.InstalledMembershipSource()
	previousGrants := auth.InstalledGrantSource()
	auth.SetMembershipSource(ms)
	auth.SetGrantSource(organizationGrantSource{})
	t.Cleanup(func() { auth.SetMembershipSource(previousMembership); auth.SetGrantSource(previousGrants) })
	ctx := memberCtx()
	// The operator grants group management; membership still bounds it.
	if _, err := i.handleGroupMemberAdd(ctx, map[string]any{"groupId": "acct-acme", "userId": "u-reader"}, 0); err != nil {
		t.Fatal(err)
	}
	before := len(stub.writes)
	if _, err := i.handleGroupMemberAdd(ctx, map[string]any{"groupId": "beta", "userId": "u-reader"}, 0); err == nil {
		t.Fatal("group grant authorized a different organization")
	}
	if len(stub.writes) != before {
		t.Fatal("refused action wrote records")
	}
	// A new receiver/next request re-resolves active groups; no process-local
	// session flag keeps an ex-member's management permission alive.
	ms.groups = nil
	receiver := New(stub, func(string, ...any) {})
	if _, err := receiver.handleGroupUpdate(callerCtx("u-writer", auth.RoleWriter), map[string]any{"groupId": "acct-acme", "name": "Hijacked"}, 0); err == nil {
		t.Fatal("removed member retained authority on another receiver")
	}
	if len(stub.writes) != before {
		t.Fatal("removed member changed group")
	}
}

type organizationOnlyGroupGrantSource struct{}

func (organizationOnlyGroupGrantSource) ActiveGrantsForUser(context.Context, string) ([]auth.Grant, error) {
	return nil, nil
}
func (organizationOnlyGroupGrantSource) ActiveGrantsForGroups(_ context.Context, groups []string) ([]auth.Grant, error) {
	for _, id := range groups {
		if id == "acct-acme" {
			return []auth.Grant{{Verb: auth.VerbUpdate, Resource: auth.ResourceGroup, Effect: auth.GrantAllow}}, nil
		}
	}
	return nil, nil
}
func TestOrganizationManagementGrantDoesNotFollowMultipleMemberships(t *testing.T) {
	i, stub := newFixture(t)
	stub.groups["beta"] = map[string]any{"id": "beta", "accountId": "beta", "status": StatusActive, "kind": KindAccount}
	oldMembership, oldGrants := auth.InstalledMembershipSource(), auth.InstalledGrantSource()
	auth.SetMembershipSource(&organizationMembershipSource{groups: []string{"acct-acme", "beta"}})
	auth.SetGrantSource(organizationOnlyGroupGrantSource{})
	t.Cleanup(func() { auth.SetMembershipSource(oldMembership); auth.SetGrantSource(oldGrants) })
	if _, err := i.handleGroupUpdate(memberCtx(), map[string]any{"groupId": "acct-acme", "name": "Acme Team"}, 0); err != nil {
		t.Fatal(err)
	}
	before := len(stub.writes)
	if _, err := i.handleGroupUpdate(memberCtx(), map[string]any{"groupId": "beta", "name": "Beta Hijacked"}, 0); err == nil {
		t.Fatal("Acme grant authorized management of Beta")
	}
	if len(stub.writes) != before {
		t.Fatal("foreign org management wrote data")
	}
}

// A denial belongs to the group organization's decision, not every roster.
type organizationSplitGroupGrantSource struct{}

func (organizationSplitGroupGrantSource) ActiveGrantsForUser(context.Context, string) ([]auth.Grant, error) {
	return nil, nil
}
func (organizationSplitGroupGrantSource) ActiveGrantsForGroups(_ context.Context, groups []string) ([]auth.Grant, error) {
	var out []auth.Grant
	for _, id := range groups {
		effect := auth.GrantAllow
		if id == "beta" {
			effect = auth.GrantDeny
		}
		out = append(out, auth.Grant{Verb: auth.VerbUpdate, Resource: auth.ResourceGroup, Effect: effect})
	}
	return out, nil
}
func TestOrganizationRosterGrantDoesNotFlattenAnotherOrganizationsDenial(t *testing.T) {
	i, stub := newFixture(t)
	stub.groups["beta"] = map[string]any{"id": "beta", "accountId": "beta", "status": StatusActive, "kind": KindAccount}
	oldMembership, oldGrants := auth.InstalledMembershipSource(), auth.InstalledGrantSource()
	auth.SetMembershipSource(&organizationMembershipSource{groups: []string{"acct-acme", "beta"}})
	auth.SetGrantSource(organizationSplitGroupGrantSource{})
	t.Cleanup(func() { auth.SetMembershipSource(oldMembership); auth.SetGrantSource(oldGrants) })
	if _, err := i.handleGroupPeople(memberCtx(), map[string]any{"groupId": "acct-acme"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := i.handleGroupPeople(memberCtx(), map[string]any{"groupId": "beta"}, 0); err == nil {
		t.Fatal("Beta roster denial ignored")
	}
}
