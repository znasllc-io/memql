package node_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"google.golang.org/protobuf/proto"
)

func TestRoleTestAuthoritySurvivesWireHop(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	ac := &auth.AccessContext{
		UserId: "v1:identity:user:role-test-viewer", Role: auth.RoleReader,
		RoleTestSessionID: "role-test-session",
	}
	authority, err := auth.ForwardedAuthorityForUser(ac, auth.RoleTestClass, "", now.Add(30*time.Minute), now)
	require.NoError(t, err)
	wire, err := proto.Marshal(node.ForwardedAuthorityToProto(authority, "replica-one", "bff"))
	require.NoError(t, err)
	received := new(nodev1.ForwardedAuthority)
	require.NoError(t, proto.Unmarshal(wire, received))
	forwarded := node.ForwardedAuthorityFromProto(received)
	require.Equal(t, authority.RoleTestSessionID, forwarded.RoleTestSessionID)
	require.Equal(t, authority.CredentialClass, forwarded.CredentialClass)
	require.Equal(t, authority.ExpiresAt, forwarded.ExpiresAt)
	resolved, err := auth.VerifyForwardedAuthority(forwarded, now)
	require.NoError(t, err)
	require.Equal(t, ac.UserId, resolved.UserId)
	require.Equal(t, ac.Role, resolved.Role)
	ctx := auth.BindForwardedContext(context.Background(), forwarded.Principal().Claims, resolved, forwarded)
	grant, err := auth.CaptureExecutionAuthority(ctx)
	require.NoError(t, err)
	require.Equal(t, ac.RoleTestSessionID, grant["roleTestSessionId"])

	received.RoleTestSessionId = ""
	_, err = auth.VerifyForwardedAuthority(node.ForwardedAuthorityFromProto(received), now)
	require.Error(t, err, "a hop that loses the session boundary must fail closed")
}
