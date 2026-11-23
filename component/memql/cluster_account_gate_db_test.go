package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// cluster_account_gate_db_test.go -- who the cluster's own vendor account pays
// for, decided by the rows (owner decision 2026-11-22; the rule is in
// vendor_account_cluster.go).
//
// The cluster owner may use it from the start. Anybody else may only when the
// owner allowed them, as a person or through a group. And the person judged is
// the one the call is FOR: work continued on a person's behalf carries a
// stand-in role, which must decide nothing.
func TestTheClustersAccountIsOpenToItsOwnerAndWhoTheyAllow(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	suffix := uniqueSuffix("cluster-account")
	who := func(name string) string { return "cluster-account-" + name + "-" + suffix }
	owner, developer, allowed, member, denied, gone := who("owner"), who("developer"), who("allowed"), who("member"), who("denied"), who("gone")
	seedPrincipal(t, eng, owner, auth.RoleOwner)
	seedPrincipal(t, eng, developer, auth.RoleDeveloper)
	seedPrincipal(t, eng, allowed, auth.RoleWriter)
	seedPrincipal(t, eng, member, auth.RoleReader)
	seedPrincipal(t, eng, denied, auth.RoleOwner)

	// One person allowed by name, one through a group, and one owner the part
	// was taken away from.
	writeGrant(t, eng, auth.SubjectKindUser, allowed, auth.VerbExecute, ClusterAccountPart, auth.GrantAllow)
	group := "g-cluster-account-" + suffix
	seedGroup(t, eng, group, "Vendor account users", "custom", "")
	seedMembership(t, eng, group, member, "active")
	writeGrant(t, eng, auth.SubjectKindGroup, group, auth.VerbExecute, ClusterAccountPart, auth.GrantAllow)
	writeGrant(t, eng, auth.SubjectKindUser, denied, auth.VerbExecute, ClusterAccountPart, auth.GrantDeny)

	// A person who was deactivated: the principal table resolves no role.
	payload, err := json.Marshal(map[string]any{"displayName": gone, "primaryEmail": gone + "@example.test", "role": "owner", "active": false})
	require.NoError(t, err)
	_, err = eng.Execute(groupSeedCtx(), fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString(conceptIdentityUser), langparser.QuoteString(gone), payload))
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		ctx    context.Context
		userId string
		open   bool
	}{
		"the cluster owner, in their own session":         {rankActorCtx(owner, auth.RoleOwner), owner, true},
		"the cluster owner's work, continued for them":    {auth.ContextWithUserActor(context.Background(), owner), owner, true},
		"the cluster owner, named by the cluster's actor": {auth.ContextWithSystemActor(context.Background(), "sweep"), owner, true},
		"a developer":                            {rankActorCtx(developer, auth.RoleDeveloper), developer, false},
		"a developer's work, continued for them": {auth.ContextWithUserActor(context.Background(), developer), developer, false},
		"a person the owner allowed":             {rankActorCtx(allowed, auth.RoleWriter), allowed, true},
		"that person's work, continued for them": {auth.ContextWithUserActor(context.Background(), allowed), allowed, true},
		"a member of a group the owner allowed":  {rankActorCtx(member, auth.RoleReader), member, true},
		"that member's work, continued for them": {auth.ContextWithUserActor(context.Background(), member), member, true},
		"an owner the part was taken away from":  {rankActorCtx(denied, auth.RoleOwner), denied, false},
		"a deactivated person's leftover work":   {auth.ContextWithUserActor(context.Background(), gone), gone, false},
		"somebody the cluster does not know":     {auth.ContextWithUserActor(context.Background(), who("nobody")), who("nobody"), false},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.open, eng.clusterAccountOpenTo(tc.ctx, tc.userId))
			// And through the registry, which is what every caller asks.
			closed := eng.providers.ClusterAccountClosedTo(tc.ctx, tc.userId)
			require.Equal(t, tc.open, closed == "", "the registry answered %q", closed)
		})
	}

	// The canonical spelling of an id is the same person.
	require.True(t, eng.clusterAccountOpenTo(auth.ContextWithUserActor(context.Background(), owner), conceptIdentityUser+":"+owner))
}
