package memql

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity/roletest"
)

// TestRoleTestListCarriesTheRolesItsOperatorMayTest: the list answer carries
// the roles the service decided, in its order and with the labels a client
// draws -- the role's name, and an organization role's organization.
func TestRoleTestListCarriesTheRolesItsOperatorMayTest(t *testing.T) {
	got := roleTestRolesPayload([]roletest.Role{
		{Slug: "reader", Name: "Viewer"},
		{Slug: "site-moderator", Name: "Site moderator", AccountID: "org-fixture", Organization: "Acme"},
	})
	require.Len(t, got, 2)
	require.Equal(t, []string{"reader", "Viewer", "", ""}, []string{got[0].GetRole(), got[0].GetName(), got[0].GetAccountId(), got[0].GetOrganization()})
	require.Equal(t, []string{"site-moderator", "Site moderator", "org-fixture", "Acme"},
		[]string{got[1].GetRole(), got[1].GetName(), got[1].GetAccountId(), got[1].GetOrganization()})
	require.Empty(t, roleTestRolesPayload(nil))
}

func TestRoleTestKeepsEndAndPersonalWorkButCannotMintCredentials(t *testing.T) {
	session, _ := newBadgeGateSession(t, badgeStreamClaims(auth.RoleTestClass, time.Now().Add(time.Minute)))
	end := &memqlv1.MemqlClientMessage{Payload: &memqlv1.MemqlClientMessage_IdentityAdmin{IdentityAdmin: &memqlv1.IdentityAdminMsg{Request: &memqlv1.IdentityAdminMsg_RoleTest{RoleTest: &memqlv1.RoleTestRequest{Operation: "end"}}}}}
	require.Equal(t, badgeGateAllow, session.badgeGate(end))
	query := &memqlv1.MemqlClientMessage{Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{ExecuteQuery: &memqlv1.ExecuteQueryMsg{}}}
	require.Equal(t, badgeGateAllow, session.badgeGate(query))
	mint := &memqlv1.MemqlClientMessage{Payload: &memqlv1.MemqlClientMessage_CreateAccountToken{CreateAccountToken: &memqlv1.CreateAccountTokenMsg{}}}
	require.Equal(t, badgeGateRestricted, session.badgeGate(mint))
	start := &memqlv1.MemqlClientMessage{Payload: &memqlv1.MemqlClientMessage_IdentityAdmin{IdentityAdmin: &memqlv1.IdentityAdminMsg{Request: &memqlv1.IdentityAdminMsg_RoleTest{RoleTest: &memqlv1.RoleTestRequest{Operation: "start", Role: "admin"}}}}}
	require.Equal(t, badgeGateRestricted, session.badgeGate(start))
	expired, _ := newBadgeGateSession(t, badgeStreamClaims(auth.RoleTestClass, time.Now().Add(-time.Second)))
	require.Equal(t, badgeGateExpired, expired.badgeGate(query))
}
