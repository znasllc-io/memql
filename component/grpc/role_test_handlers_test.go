package memql

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

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
