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

// account_authority_test.go -- the call's account across a mesh hop (design
// record docs/superpowers/specs/2026-11-12-account-row-ownership-design.md,
// section C: "the account travels across nodes").
//
// The account a call acts in is stated on the node where the call starts, by
// the row that triggered an automation or the site a request arrived on. The
// node that handles a forward has neither, so what it records has to arrive on
// the assertion. These run the whole chain a real forward runs -- the producer
// reading its context, the ONE mapping onto the wire message, the bytes, the
// mapping back, the verifier, the bind -- and ask the far context what the
// engine there will ask it.

func hopOverTheWire(t *testing.T, authority auth.ForwardedAuthority, now time.Time) (auth.ForwardedAuthority, context.Context) {
	t.Helper()
	wire, err := proto.Marshal(node.ForwardedAuthorityToProto(authority, "replica-one", "bff"))
	require.NoError(t, err)
	received := new(nodev1.ForwardedAuthority)
	require.NoError(t, proto.Unmarshal(wire, received))
	forwarded := node.ForwardedAuthorityFromProto(received)
	resolved, err := auth.VerifyForwardedAuthority(forwarded, now)
	require.NoError(t, err)
	// context.Background(), deliberately: the receiving node holds none of the
	// originating call's state, and anything it knows came over the wire.
	return forwarded, auth.BindForwardedContext(context.Background(), forwarded.Principal().Claims, resolved, forwarded)
}

func TestCallAccountSurvivesWireHop(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	ac := &auth.AccessContext{UserId: "v1:identity:user:site-owner", Role: auth.RoleWriter}
	bound, err := auth.ForwardedAuthorityForUser(ac, auth.ForwardedClassUser, "", time.Time{}, now)
	require.NoError(t, err)

	// THE ORIGINATING NODE. The actor is bound first and the account is stated
	// afterwards, further down the same call, which is the order an automation
	// run and a shopper form both produce. The producer must carry the account
	// as it stands when the forward is made.
	origin := auth.BindForwardedContext(context.Background(), bound.Principal().Claims, ac, bound)
	origin = auth.ContextWithCallAccount(origin, "v1:accounts:account:acme")
	authority, ok := auth.ForwardedAuthorityFromContext(origin)
	require.True(t, ok)
	require.Equal(t, "acme", authority.AccountId, "the producer did not read the account the call is acting in")

	forwarded, remote := hopOverTheWire(t, authority, now)
	require.Equal(t, authority.AccountId, forwarded.AccountId,
		"the account did not cross the wire: it is missing from node.proto or from one of the two adapters")

	got, stated := auth.CallAccountFromContext(remote)
	require.True(t, stated, "the receiving node bound no account, so it would work one out again from the actor")
	require.Equal(t, "acme", got)

	// The actor arrived as it always did; the account changed nothing about it.
	remoteAccess, ok := auth.AccessFromContext(remote)
	require.True(t, ok)
	require.Equal(t, ac.UserId, remoteAccess.UserId)
	require.Equal(t, ac.Role, remoteAccess.Role)

	// A SECOND HOP out of the receiving node carries the same account, without
	// that node having stated anything itself.
	again, ok := auth.ForwardedAuthorityFromContext(remote)
	require.True(t, ok)
	_, twice := hopOverTheWire(t, again, now)
	got, stated = auth.CallAccountFromContext(twice)
	require.True(t, stated)
	require.Equal(t, "acme", got, "the second hop lost the account the first one carried")
}

// An assertion that names no account binds none. The receiving node then
// resolves the account from the actor, and it must be free to: a bound empty
// value that read as "this call acts in no account" would leave every row of a
// forwarded person's call unassigned.
func TestAnAuthorityWithNoAccountBindsNone(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	ac := &auth.AccessContext{UserId: "v1:identity:user:alice", Role: auth.RoleWriter}
	authority, err := auth.ForwardedAuthorityForUser(ac, auth.ForwardedClassUser, "", time.Time{}, now)
	require.NoError(t, err)
	require.Empty(t, authority.AccountId, "the constructor invented an account")

	onWire := node.ForwardedAuthorityToProto(authority, "replica-one", "bff")
	require.Empty(t, onWire.GetAccountId())

	forwarded, remote := hopOverTheWire(t, authority, now)
	require.Empty(t, forwarded.AccountId)
	got, stated := auth.CallAccountFromContext(remote)
	require.False(t, stated, "an assertion with no account bound %q", got)
}

// A system principal carries an account too. An automation that a row triggered
// runs as the cluster and still acts in that row's account, and when one of its
// steps forwards, the far node must record it.
func TestASystemAuthorityCarriesItsCallAccount(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	system, err := auth.ForwardedAuthorityForSystem("pipelines-executor:replica-one", now)
	require.NoError(t, err)
	access, err := auth.VerifyForwardedAuthority(system, now)
	require.NoError(t, err)

	origin := auth.ContextWithCallAccount(context.Background(), "beta")
	origin = auth.BindForwardedContext(origin, system.Principal().Claims, access, system)
	authority, ok := auth.ForwardedAuthorityFromContext(origin)
	require.True(t, ok)

	_, remote := hopOverTheWire(t, authority, now)
	got, stated := auth.CallAccountFromContext(remote)
	require.True(t, stated)
	require.Equal(t, "beta", got)
}
