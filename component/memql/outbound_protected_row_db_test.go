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

// outbound_protected_row_db_test.go -- memql#5480, on a real engine over a
// real Postgres.
//
// A webhook row may name the v1:platform:globalSecret holding its URL instead
// of carrying the URL, because a Discord webhook's token is in its path; and a
// row whose outcome server code reports -- the pipelines notify stage's email
// -- is staged by server code and marked so (serverStaged). The outbound
// worker's own tests drive it against a fake engine, which makes every engine
// half of that contract true by construction: that the stamp's
// "secret:" + args.targetSecret writes the descriptor, that outboundRequestFull
// carries targetSecret to the worker, that a client is refused the server-only
// constructs, and -- the half that matters most -- that no write without
// internal origin reaches a protected row at all: not its target, not what it
// sends, not its delivery state. @serverOnly bars a NAMED call; a raw insert()
// never consults it, so the last is the write guard's
// (outbound_protected_row_write_guard.go). component/outbound is not in the
// db-tests lane, so all of it is pinned here, on the shared engine this
// package's DSL-over-real-rows tests already boot.

const outboundRequestConcept = "v1:platform:outboundRequest"

// outboundClient is an operator on the wire. The operator-only row tier
// admits this caller so these cases exercise the deeper protected-field guard:
// even an operator cannot forge a server-owned receipt. Ordinary-user refusal
// is covered by delivery_privacy_db_test.go.
func outboundClient(ctx context.Context, userID string) context.Context {
	return auth.ContextWithClientOrigin(auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: userID, Role: auth.RoleOwner}))
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
	internal := auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(ctx, "outbound-test"))
	reqId := "out5480-" + uniqueSuffix("secret-descriptor")

	canonicalId := stageSecretRow(t, eng, ctx, reqId)
	stored := latestPayload(t, internal, db, outboundRequestConcept, canonicalId)
	require.Equal(t, "webhook", stored["medium"])
	require.Equal(t, "secret:DISCORD_RELEASES", stored["target"],
		"target must hold the descriptor, so the row and every error name the secret and never the URL")
	require.Equal(t, "DISCORD_RELEASES", stored["targetSecret"])
	require.Equal(t, true, stored["serverStaged"], "a secret row is staged by server code, and marked so")
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
	client := outboundClient(ctx, "operator-5480")
	reqId := "out5480-" + uniqueSuffix("raw-insert")

	_, err := eng.Execute(client, `insert("v1:platform:outboundRequest", id="`+reqId+`", payload={`+
		`"medium":"webhook","target":"secret:DISCORD_RELEASES","targetSecret":"DISCORD_RELEASES",`+
		`"body":"not from this cluster","status":"pending","attempts":0})`)
	require.Error(t, err, "a client raw insert named a secret")
	require.Contains(t, err.Error(), "`targetSecret`")
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
	client := outboundClient(ctx, "operator-5480")
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

// outboundWorkerActor is the outbound worker's identity as
// component/outbound.SystemActorContext builds it: a system token, no person.
// The worker adds internal origin on each status stamp, inline.
func outboundWorkerActor() context.Context {
	return auth.ContextWithSystemActor(context.Background(), "system:outbound")
}

// TestTheWorkersStatusStampsOnASecretRowLand: the outbound worker stamps
// delivery state under its system actor with internal origin on each stamp,
// so the guard admits it. This is the walk a delivery that is retried once
// and then sent takes.
func TestTheWorkersStatusStampsOnASecretRowLand(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	reqId := "out5480-" + uniqueSuffix("secret-stamps")
	canonicalId := stageSecretRow(t, eng, ctx, reqId)
	worker := auth.ContextWithInternalOrigin(outboundWorkerActor())

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

// TestAClientCannotStampASecretRowsStatus: the notify stage reports a delivery
// when the row it staged reads `sent`, so a secret row's delivery state is as
// much server-side Go's as its target. A client marking one sent would fake a
// delivery, and requeueing one would post the stored body again. Both are
// refused, as is the worker's own actor arriving without internal origin, and
// the row keeps the state the worker gave it.
func TestAClientCannotStampASecretRowsStatus(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	internal := auth.ContextWithInternalOrigin(ctx)
	reqId := "out5480-" + uniqueSuffix("secret-client-stamp")
	canonicalId := stageSecretRow(t, eng, ctx, reqId)
	_, err := eng.Execute(auth.ContextWithInternalOrigin(outboundWorkerActor()),
		`mutation updateOutboundRequestStatus(requestId: "`+reqId+`", status: "failed", attempts: 5, lastError: "webhook: status 404")`)
	require.NoError(t, err, "precondition: the worker failed the row")

	for _, tc := range []struct {
		name string
		ctx  context.Context
		call string
	}{
		{"a client marking it sent", outboundClient(ctx, "operator-5480"),
			`mutation updateOutboundRequestStatus(requestId: "` + reqId + `", status: "sent", lastError: "", sentAt: "2026-10-04T12:00:31Z")`},
		{"a client requeueing it", outboundClient(ctx, "operator-5480"),
			`mutation updateOutboundRequestStatus(requestId: "` + reqId + `", status: "pending")`},
		{"the worker's actor without internal origin", auth.ContextWithClientOrigin(outboundWorkerActor()),
			`mutation updateOutboundRequestStatus(requestId: "` + reqId + `", status: "pending")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := eng.Execute(tc.ctx, tc.call)
			require.Error(t, err, "a status stamp without internal origin landed on a secret row")
			require.Contains(t, err.Error(), "`status`")
			stored := latestPayload(t, internal, db, outboundRequestConcept, canonicalId)
			require.Equal(t, "failed", stored["status"], "the refused stamp changed the row's state")
			require.EqualValues(t, 5, stored["attempts"])
		})
	}
}

// TestAnOperatorStatusStampOnAPlainRowLands: the rule is about protected rows --
// naming a secret, or staged by server code. Operators may still manage a
// plain row that carries neither marker; ordinary users cannot access the
// concept (memql#5804).
func TestAnOperatorStatusStampOnAPlainRowLands(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	client := outboundClient(ctx, "operator-5480")
	reqId := "out5480-" + uniqueSuffix("plain-client-stamp")
	canonicalId := runMutation(t, client, eng, "stageOutboundRequest", map[string]any{
		"requestId": reqId,
		"medium":    "webhook",
		"target":    "https://hooks.example/plain",
		"body":      "plain",
	})

	for _, call := range []string{
		`mutation updateOutboundRequestStatus(requestId: "` + reqId + `", status: "sent", lastError: "", sentAt: "2026-10-04T12:00:31Z")`,
		`mutation updateOutboundRequestStatus(requestId: "` + reqId + `", status: "pending")`,
	} {
		_, err := eng.Execute(client, call)
		require.NoError(t, err, "a client status stamp on a plain row was refused: %s", call)
	}
	require.Equal(t, "pending", latestPayload(t, auth.ContextWithInternalOrigin(ctx), db, outboundRequestConcept, canonicalId)["status"])
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
	require.Equal(t, true, stored["serverStaged"],
		"the row stays the server's: the re-stage was internal, and the mark is not the plain mutation's to clear")
}

// stageServerRow stages an email row the way the notify stage does: through
// stageServerOutboundRequest, under internal origin.
func stageServerRow(t *testing.T, eng *MemQLEngine, ctx context.Context, reqId string) string {
	t.Helper()
	return runMutation(t, auth.ContextWithInternalOrigin(ctx), eng, "stageServerOutboundRequest", map[string]any{
		"requestId":   reqId,
		"medium":      "email",
		"target":      "ops@example.test",
		"subject":     "shop - Push to main passed",
		"body":        "All 2 stages passed.",
		"dedupeKey":   "pn-" + reqId,
		"requestedBy": "pipelines:notify:run-42",
	})
}

// TestStageServerOutboundRequestMarksTheRow: the server-side stage of a plain
// delivery writes the row stageOutboundRequest would, marked serverStaged and
// naming no secret. A client is refused the construct, a cluster owner
// included, and writes no row.
func TestStageServerOutboundRequestMarksTheRow(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	internal := auth.ContextWithInternalOrigin(ctx)
	reqId := "out5480-" + uniqueSuffix("server-staged")

	canonicalId := stageServerRow(t, eng, ctx, reqId)
	stored := latestPayload(t, internal, db, outboundRequestConcept, canonicalId)
	require.Equal(t, "email", stored["medium"])
	require.Equal(t, "ops@example.test", stored["target"])
	require.Equal(t, true, stored["serverStaged"])
	require.Empty(t, stored["targetSecret"], "a plain delivery names no secret")
	require.Equal(t, "pending", stored["status"])
	require.EqualValues(t, 0, stored["attempts"])

	owner := auth.ContextWithClientOrigin(auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "owner-5480", Role: auth.RoleOwner}))
	clientId := reqId + "-client"
	_, err := eng.Execute(owner, `mutation stageServerOutboundRequest(requestId: "`+clientId+`", medium: "email", target: "ops@example.test", body: "x")`)
	require.Error(t, err, "a client staged a row the server alone may write")
	require.Contains(t, err.Error(), "server-only")
	require.Zero(t, outboundRowCount(t, db, clientId), "the refused stage wrote a row")
}

// TestAClientRawInsertMarkingARowServerStagedIsRefused: insert() never
// consults stageServerOutboundRequest's @serverOnly, so the mark itself is
// the guard's: a client could otherwise mint a row that reads `sent`, staged
// in the server's name.
func TestAClientRawInsertMarkingARowServerStagedIsRefused(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	client := outboundClient(ctx, "operator-5480")
	reqId := "out5480-" + uniqueSuffix("raw-staged")

	_, err := eng.Execute(client, `insert("v1:platform:outboundRequest", id="`+reqId+`", payload={`+
		`"medium":"email","target":"ops@example.test","body":"not from this cluster","status":"sent","attempts":1,"serverStaged":true})`)
	require.Error(t, err, "a client raw insert marked a row server-staged")
	require.Contains(t, err.Error(), "`serverStaged`")
	require.Zero(t, outboundRowCount(t, db, reqId), "the refused insert wrote a row")
}

// TestAClientCannotReVersionAServerStagedRow: a re-stage at the same id would
// change where the delivery goes or what it says, and a raw insert could drop
// the mark that keeps the row the server's. Each is refused, and the row is as
// staged.
func TestAClientCannotReVersionAServerStagedRow(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	internal := auth.ContextWithInternalOrigin(ctx)
	client := outboundClient(ctx, "operator-5480")
	reqId := "out5480-" + uniqueSuffix("staged-reversion")
	canonicalId := stageServerRow(t, eng, ctx, reqId)

	for _, tc := range []struct {
		name, call, field string
	}{
		{"re-stage to another address through stageOutboundRequest",
			`mutation stageOutboundRequest(requestId: "` + reqId + `", medium: "email", target: "eve@example.test", body: "All 2 stages passed.")`, "target"},
		{"raw insert under a foreign body",
			`insert("v1:platform:outboundRequest", id="` + reqId + `", payload={"body":"not from this pipeline"})`, "body"},
		{"raw insert dropping the mark",
			`insert("v1:platform:outboundRequest", id="` + reqId + `", payload={"serverStaged":false})`, "serverStaged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := eng.Execute(client, tc.call)
			require.Error(t, err, "a client re-versioned a server-staged row")
			require.Contains(t, err.Error(), "`"+tc.field+"`")
			stored := latestPayload(t, internal, db, outboundRequestConcept, canonicalId)
			require.Equal(t, "ops@example.test", stored["target"], "the refused write changed the target")
			require.Equal(t, "All 2 stages passed.", stored["body"], "the refused write changed the body")
			require.Equal(t, true, stored["serverStaged"], "the refused write dropped the mark")
		})
	}
}

// TestTheWorkersStatusStampsOnAServerStagedRowLand: the worker stamps under
// internal origin, so the guard admits its walk of a delivery.
func TestTheWorkersStatusStampsOnAServerStagedRowLand(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	reqId := "out5480-" + uniqueSuffix("staged-stamps")
	canonicalId := stageServerRow(t, eng, ctx, reqId)
	worker := auth.ContextWithInternalOrigin(outboundWorkerActor())

	for _, call := range []string{
		`mutation updateOutboundRequestStatus(requestId: "` + reqId + `", status: "sending")`,
		`mutation updateOutboundRequestStatus(requestId: "` + reqId + `", status: "sent", lastError: "", sentAt: "2026-10-04T12:00:31Z")`,
	} {
		_, err := eng.Execute(worker, call)
		require.NoError(t, err, "the worker's status stamp was refused: %s", call)
	}
	stored := latestPayload(t, auth.ContextWithInternalOrigin(ctx), db, outboundRequestConcept, canonicalId)
	require.Equal(t, "sent", stored["status"])
	require.Equal(t, true, stored["serverStaged"])
}

// TestAClientCannotStampAServerStagedRowsStatus: the notify stage reports its
// email delivered when the row reads `sent`, so a client marking one sent
// would fake the delivery, and requeueing one would send it again. Both are
// refused, as is the worker's own actor arriving without internal origin, and
// the row keeps the state the worker gave it.
func TestAClientCannotStampAServerStagedRowsStatus(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	internal := auth.ContextWithInternalOrigin(ctx)
	reqId := "out5480-" + uniqueSuffix("staged-client-stamp")
	canonicalId := stageServerRow(t, eng, ctx, reqId)
	_, err := eng.Execute(auth.ContextWithInternalOrigin(outboundWorkerActor()),
		`mutation updateOutboundRequestStatus(requestId: "`+reqId+`", status: "failed", attempts: 5, lastError: "target not in email allowlist")`)
	require.NoError(t, err, "precondition: the worker failed the row")

	for _, tc := range []struct {
		name string
		ctx  context.Context
		call string
	}{
		{"a client marking it sent", outboundClient(ctx, "operator-5480"),
			`mutation updateOutboundRequestStatus(requestId: "` + reqId + `", status: "sent", lastError: "", sentAt: "2026-10-04T12:00:31Z")`},
		{"a client requeueing it", outboundClient(ctx, "operator-5480"),
			`mutation updateOutboundRequestStatus(requestId: "` + reqId + `", status: "pending")`},
		{"the worker's actor without internal origin", auth.ContextWithClientOrigin(outboundWorkerActor()),
			`mutation updateOutboundRequestStatus(requestId: "` + reqId + `", status: "pending")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := eng.Execute(tc.ctx, tc.call)
			require.Error(t, err, "a status stamp without internal origin landed on a server-staged row")
			require.Contains(t, err.Error(), "`status`")
			stored := latestPayload(t, internal, db, outboundRequestConcept, canonicalId)
			require.Equal(t, "failed", stored["status"], "the refused stamp changed the row's state")
			require.EqualValues(t, 5, stored["attempts"])
		})
	}
}
