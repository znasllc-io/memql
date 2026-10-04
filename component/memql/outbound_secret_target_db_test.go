package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// outbound_secret_target_db_test.go -- memql#5480, on a real engine over a
// real Postgres.
//
// A webhook row may name the v1:platform:globalSecret holding its URL instead
// of carrying the URL, because a Discord webhook's token is in its path. The
// outbound worker's own tests drive it against a fake engine, which makes every
// engine half of that contract true by construction: that the stamp's
// "secret:" + args.targetSecret writes the descriptor, that outboundRequestFull
// carries targetSecret to the worker, that a client is refused both
// server-only constructs, and -- the half that matters most -- that no write
// without internal origin can name a secret or change what a secret row sends.
// @serverOnly bars a NAMED call; a raw insert() never consults it, so the last
// is the write guard's (outbound_secret_target_write_guard.go). component/outbound
// is not in the db-tests lane, so all of it is pinned here, on the shared engine
// this package's DSL-over-real-rows tests already boot.

const outboundRequestConcept = "v1:platform:outboundRequest"

// outboundClient is a signed-in person on the wire: client origin, an actor.
func outboundClient(ctx context.Context, userID string) context.Context {
	return auth.ContextWithClientOrigin(auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: userID, Role: auth.RoleWriter}))
}

// stageSecretRow stages a secret-target row the way the notify stage will:
// through the server-only mutation, under internal origin.
func stageSecretRow(t *testing.T, eng *MemQLEngine, ctx context.Context, reqId string) string {
	t.Helper()
	return runMutation(t, auth.ContextWithInternalOrigin(ctx), eng, "stageOutboundRequestToSecret", map[string]any{
		"requestId":    reqId,
		"targetSecret": "DISCORD_RELEASES",
		"body":         `{"content":"run 42 passed"}`,
		"dedupeKey":    "pn-" + reqId,
		"requestedBy":  "pipelines:notify:run-42",
	})
}

// outboundRowCount counts every stored version of a row, by its bare or its
// canonical id, so "refused" can be told from "written somewhere".
func outboundRowCount(t *testing.T, db *bun.DB, reqId string) int {
	t.Helper()
	n, err := db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).
		Where("concept = ?", outboundRequestConcept).
		Where("id IN (?)", bun.In([]string{reqId, outboundRequestConcept + ":" + reqId})).
		Count(context.Background())
	require.NoError(t, err)
	return n
}

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

	canonicalId := stageSecretRow(t, eng, ctx, reqId)
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

// TestStageOutboundRequestToSecretRefusesANameOutsideThePattern: the secret's
// name is interpolated into the resolver's lookup, so the mutation holds it to
// ^[A-Z][A-Z0-9_]{0,63}$ before a row can exist. The worker re-checks the same
// rule before it resolves anything (component/outbound).
func TestStageOutboundRequestToSecretRefusesANameOutsideThePattern(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	internal := auth.ContextWithInternalOrigin(ctx)
	for _, name := range []string{"discord_releases", `X" || payload.name != "`, "9DISCORD"} {
		reqId := "out5480-" + uniqueSuffix("secret-pattern")
		_, err := eng.Execute(internal, `mutation stageOutboundRequestToSecret(requestId: "`+reqId+
			`", targetSecret: `+langparser.QuoteString(name)+`, body: "{}")`)
		require.Error(t, err, "a secret name outside the pattern was staged: %q", name)
		require.Contains(t, err.Error(), "does not match pattern", "refused for the wrong reason: %q", name)
		require.Zero(t, outboundRowCount(t, db, reqId), "a refused stage wrote a row for %q", name)
	}
}

// TestAClientRawInsertNamingASecretIsRefused: insert() is reachable by any
// signed-in caller and never consults stageOutboundRequestToSecret's
// @serverOnly. Without the write guard this row lands, the drain scan projects
// its targetSecret, and the worker POSTs the caller's body to the URL a
// cluster owner stored.
func TestAClientRawInsertNamingASecretIsRefused(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	client := outboundClient(ctx, "writer-5480")
	reqId := "out5480-" + uniqueSuffix("raw-insert")

	_, err := eng.Execute(client, `insert("v1:platform:outboundRequest", id="`+reqId+`", payload={`+
		`"medium":"webhook","target":"secret:DISCORD_RELEASES","targetSecret":"DISCORD_RELEASES",`+
		`"body":"not from this cluster","status":"pending","attempts":0})`)
	require.Error(t, err, "a client raw insert named a secret")
	require.Contains(t, err.Error(), "targetSecret")
	require.Zero(t, outboundRowCount(t, db, reqId), "the refused insert wrote a row")

	// The refusal is about the secret, not about raw writes: the same insert
	// with an ordinary target lands, as it always has.
	plainId := reqId + "-plain"
	_, err = eng.Execute(client, `insert("v1:platform:outboundRequest", id="`+plainId+`", payload={`+
		`"medium":"webhook","target":"https://hooks.example/plain","body":"plain","status":"pending","attempts":0})`)
	require.NoError(t, err, "a client raw insert of a plain row must still land")
	require.Equal(t, 1, outboundRowCount(t, db, plainId))
}

// TestAClientCannotReVersionASecretRow: the same id is a rewrite. A raw insert
// or a re-stage through the client-reachable stageOutboundRequest would keep
// the row's targetSecret through the read-merge under the caller's body, or
// point it at another secret. Each is refused, and the row is as staged.
func TestAClientCannotReVersionASecretRow(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	internal := auth.ContextWithInternalOrigin(ctx)
	client := outboundClient(ctx, "writer-5480")
	reqId := "out5480-" + uniqueSuffix("secret-reversion")
	canonicalId := stageSecretRow(t, eng, ctx, reqId)

	for _, tc := range []struct {
		name, call, field string
	}{
		{"raw insert under a foreign body",
			`insert("v1:platform:outboundRequest", id="` + reqId + `", payload={"body":"not from this pipeline"})`, "body"},
		{"raw insert naming another secret",
			`insert("v1:platform:outboundRequest", id="` + reqId + `", payload={"targetSecret":"SOME_OTHER_SECRET"})`, "targetSecret"},
		{"re-stage through stageOutboundRequest",
			`mutation stageOutboundRequest(requestId: "` + reqId + `", medium: "webhook", target: "secret:DISCORD_RELEASES", body: "not from this pipeline")`, "targetSecret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := eng.Execute(client, tc.call)
			require.Error(t, err, "a client re-versioned a secret row")
			require.Contains(t, err.Error(), "`"+tc.field+"`")
			stored := latestPayload(t, internal, db, outboundRequestConcept, canonicalId)
			require.Equal(t, `{"content":"run 42 passed"}`, stored["body"], "the refused write changed the body")
			require.Equal(t, "DISCORD_RELEASES", stored["targetSecret"], "the refused write changed the secret")
		})
	}
}

// TestTheWorkersStatusStampsOnASecretRowLand: the outbound worker stamps
// delivery state under its own system actor, with no internal origin, so the
// guard must leave the lifecycle fields open on a secret row. This is the walk
// a delivery that is retried once and then sent takes.
func TestTheWorkersStatusStampsOnASecretRowLand(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	reqId := "out5480-" + uniqueSuffix("secret-stamps")
	canonicalId := stageSecretRow(t, eng, ctx, reqId)
	worker := auth.ContextWithClientOrigin(auth.ContextWithToken(context.Background(), &auth.TokenInfo{
		Subject: "system:outbound",
		Claims:  map[string]any{"sub": "system:outbound", "role": "system"},
	}))

	for _, call := range []string{
		`mutation updateOutboundRequestStatus(requestId: "` + reqId + `", status: "sending")`,
		`mutation updateOutboundRequestStatus(requestId: "` + reqId + `", status: "retrying", attempts: 1, lastError: "webhook: status 503", nextAttemptAt: "2026-10-04T12:00:30Z")`,
		`mutation updateOutboundRequestStatus(requestId: "` + reqId + `", status: "sending")`,
		`mutation updateOutboundRequestStatus(requestId: "` + reqId + `", status: "sent", lastError: "", sentAt: "2026-10-04T12:00:31Z")`,
	} {
		_, err := eng.Execute(worker, call)
		require.NoError(t, err, "the worker's status stamp was refused: %s", call)
	}
	stored := latestPayload(t, auth.ContextWithInternalOrigin(ctx), db, outboundRequestConcept, canonicalId)
	require.Equal(t, "sent", stored["status"])
	require.EqualValues(t, 1, stored["attempts"])
	require.Equal(t, "2026-10-04T12:00:31Z", stored["sentAt"])
	require.Equal(t, "DISCORD_RELEASES", stored["targetSecret"])
}

// TestAnInternalReStageThroughThePlainMutationLeavesAPlainRow: what the guard
// does not judge, the stamp does. An internal-origin caller of the plain
// stageOutboundRequest -- a product automation's step context carries internal
// origin -- passes the guard, so the mutation stamps targetSecret empty: a
// re-stage onto a secret row's id leaves an ordinary row whose target, the
// descriptor, no allowlist admits, rather than the secret under a new body.
func TestAnInternalReStageThroughThePlainMutationLeavesAPlainRow(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	internal := auth.ContextWithInternalOrigin(ctx)
	reqId := "out5480-" + uniqueSuffix("secret-internal-restage")
	canonicalId := stageSecretRow(t, eng, ctx, reqId)

	const productBody = `{"content":"a product's own delivery"}`
	runMutation(t, internal, eng, "stageOutboundRequest", map[string]any{
		"requestId": reqId,
		"medium":    "webhook",
		"target":    "secret:DISCORD_RELEASES",
		"body":      productBody,
	})
	stored := latestPayload(t, internal, db, outboundRequestConcept, canonicalId)
	require.Equal(t, productBody, stored["body"], "precondition: the internal re-stage landed on the same row")
	require.Empty(t, stored["targetSecret"],
		"a re-stage through the plain mutation kept the secret target, so the worker would POST that body to the URL the secret holds")
}
