package groups

import (
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

func TestOrganizationPeopleNeverEnumeratesAnotherClient(t *testing.T) {
	i, stub := newFixture(t)
	stub.groups["beta"] = map[string]any{"id": "beta", "accountId": "beta", "status": StatusActive, "kind": KindAccount}
	stub.users["our-person"] = map[string]any{"id": "our-person", "role": "reader", "active": true, "displayName": "Ours", "primaryEmail": "ours@example.test", "preferences": map[string]any{"private": "never-project"}}
	stub.users["their-person"] = map[string]any{"id": "their-person", "role": "reader", "active": true, "displayName": "Theirs"}
	stub.members["acct-acme"] = []any{map[string]any{"id": "m-1", "groupId": "acct-acme", "userId": "our-person", "status": StatusActive}}
	stub.members["beta"] = []any{map[string]any{"id": "m-2", "groupId": "beta", "userId": "their-person", "status": StatusActive}}
	ms := &organizationMembershipSource{groups: []string{"acct-acme"}}
	previousMembership, previousGrants := auth.InstalledMembershipSource(), auth.InstalledGrantSource()
	auth.SetMembershipSource(ms)
	auth.SetGrantSource(organizationGrantSource{})
	t.Cleanup(func() { auth.SetMembershipSource(previousMembership); auth.SetGrantSource(previousGrants) })
	res, err := i.handleGroupPeople(memberCtx(), map[string]any{"groupId": "acct-acme"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		t.Fatalf("wrong roster: %#v", rows)
	}
	person, _ := rows[0]["payload"].(map[string]any)
	if rowString(person, "displayName") != "Ours" {
		t.Fatalf("wrong organization roster: %#v", rows)
	}
	if _, ok := person["preferences"]; ok {
		t.Fatal("private user fields leaked into the organization roster")
	}
	if _, err := i.handleGroupPeople(memberCtx(), map[string]any{"groupId": "beta"}, 0); err == nil {
		t.Fatal("cross-account roster admitted")
	}
	ms.groups = nil
	if _, err := New(stub, nil).handleGroupPeople(memberCtx(), map[string]any{"groupId": "acct-acme"}, 0); err == nil {
		t.Fatal("another node reused revoked membership")
	}
}
