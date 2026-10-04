package memql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
)

// outbound_secret_target_db_test.go -- memql#5480, on a real engine over a
// real Postgres.
//
// A webhook row may name the v1:platform:globalSecret holding its URL instead
// of carrying the URL, because a Discord webhook's token is in its path. The
// outbound worker's own tests drive it against a fake engine, which makes every
// DSL half of that contract true by construction: that the stamp's
// "secret:" + args.targetSecret writes the descriptor, that outboundRequestFull
// carries targetSecret to the worker, and that a client is refused both
// server-only constructs. component/outbound is not in the db-tests lane
// (memql#3030), so those halves are pinned here, on the shared engine this
// package's DSL-over-real-rows tests already boot.

const outboundRequestConcept = "v1:platform:outboundRequest"

// TestStageOutboundRequestToSecretStagesTheDescriptor: the server-side stage
// writes medium webhook, the descriptor secret:<NAME> as target and the name
// as targetSecret, and the by-id read projects targetSecret through
// outboundRequestFull, the shape the worker's drain scan reads rows through.
// A client is refused both constructs, a cluster owner included, because the
// property is the channel and not the rank.
func TestStageOutboundRequestToSecretStagesTheDescriptor(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	internal := auth.ContextWithInternalOrigin(ctx)
	reqId := "out5480-" + uniqueSuffix("secret-descriptor")

	canonicalId := runMutation(t, internal, eng, "stageOutboundRequestToSecret", map[string]any{
		"requestId":    reqId,
		"targetSecret": "DISCORD_RELEASES",
		"body":         `{"content":"run 42 passed"}`,
		"dedupeKey":    "pn-" + reqId,
		"requestedBy":  "pipelines:notify:run-42",
	})
	stored := latestPayload(t, internal, db, outboundRequestConcept, canonicalId)
	require.Equal(t, "webhook", stored["medium"])
	require.Equal(t, "secret:DISCORD_RELEASES", stored["target"],
		"target must hold the descriptor, so the row and every error name the secret and never the URL")
	require.Equal(t, "DISCORD_RELEASES", stored["targetSecret"])
	require.Equal(t, "pending", stored["status"])
	require.EqualValues(t, 0, stored["attempts"])

	res, err := eng.Execute(ContextWithFreshRead(internal), `query outboundRequestById(requestId: "`+reqId+`")`)
	require.NoError(t, err)
	rows := MaterializeRows(res)
	require.Len(t, rows, 1, "the by-id read must find the row it names")
	require.Equal(t, "DISCORD_RELEASES", rows[0]["targetSecret"],
		"outboundRequestFull must project targetSecret: the worker's drain scan reads rows through it, "+
			"and a scan without the field would deliver to the descriptor as if it were the URL")

	owner := auth.ContextWithClientOrigin(auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "owner-5480", Role: auth.RoleOwner}))
	_, err = eng.Execute(owner, `mutation stageOutboundRequestToSecret(requestId: "`+reqId+`-client", targetSecret: "DISCORD_RELEASES", body: "{}")`)
	require.Error(t, err, "a client staged a row naming a secret")
	require.Contains(t, err.Error(), "server-only")
	_, err = eng.Execute(owner, `query outboundRequestById(requestId: "`+reqId+`")`)
	require.Error(t, err, "a client read a delivery by id")
	require.Contains(t, err.Error(), "server-only")
}
