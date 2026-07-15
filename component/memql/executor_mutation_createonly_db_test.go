package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// executor_mutation_createonly_db_test.go is the real-engine reproduction +
// fix guard for fylo#63: stageOutboundRequest re-fires must not reset the
// delivery lifecycle of a row the outbound worker already owns.
//
// The bug: stageOutboundRequest derives requestId deterministically per
// (kind, refId) so a re-fire re-targets the SAME v1:platform:outboundRequest
// row (staging idempotency). But the mutation writes status="pending" +
// attempts=0 in its insert block, and the engine read-merge (memql#1709)
// let those overwrite the stored values on a re-stage -- so a re-fire while
// the triggering row is still at the same stage would reset an already-sent
// (or in-flight) row back to pending and the worker would redeliver.
//
// The fix: @createOnly("status", "attempts") drops those fields from the
// delta on the read-merge path, so a re-stage preserves the worker-owned
// lifecycle while still refreshing the deliverable content.
//
// This boots a REAL engine against a REAL Postgres (same New + Init path
// app.Run runs), so it exercises the actual DSL-loaded mutation + the
// executeWrite read-merge, not a hand-built payload. Postgres-gated: skips
// when no DB is reachable, exactly like executor_mutation_readmerge_db_test.go.

func TestCreateOnly_ReStageDoesNotResetWorkerOwnedStatus(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)

	const conceptName = "v1:platform:outboundRequest"
	reqId := "co63-" + uniqueSuffix("restage")

	// 1. Product stages the outbound request (birth): status seeds to
	//    pending, attempts to 0.
	canonicalId := runMutation(t, ctx, eng, "stageOutboundRequest", map[string]any{
		"requestId":   reqId,
		"medium":      "webhook",
		"target":      "https://hook.example/co63",
		"body":        "body-v1",
		"dedupeKey":   "co63:label:" + reqId,
		"requestedBy": "test:createonly",
	})

	staged := latestPayload(t, ctx, db, conceptName, canonicalId)
	require.Equal(t, "pending", staged["status"], "fresh stage seeds status=pending")
	require.Equal(t, float64(0), staged["attempts"], "fresh stage seeds attempts=0")

	// 2. The outbound worker claims and delivers it: status -> sent,
	//    attempts -> 1, sentAt stamped. (Its own update{} mutation, which
	//    legitimately writes status -- @createOnly is mutation-scoped to
	//    stageOutboundRequest, so this path is unaffected.)
	runMutation(t, ctx, eng, "updateOutboundRequestStatus", map[string]any{
		"requestId": reqId,
		"status":    "sent",
		"attempts":  1,
		"sentAt":    "2026-01-02T03:04:05Z",
		"lastError": "",
	})

	delivered := latestPayload(t, ctx, db, conceptName, canonicalId)
	require.Equal(t, "sent", delivered["status"])
	require.Equal(t, float64(1), delivered["attempts"])

	// 3. The product automation re-fires (returnCase.updated fires on ANY
	//    field change) and re-stages the SAME requestId with refreshed
	//    content. WITHOUT @createOnly this resets status->pending and the
	//    worker redelivers. WITH @createOnly the worker-owned lifecycle is
	//    preserved and only the content is refreshed.
	runMutation(t, ctx, eng, "stageOutboundRequest", map[string]any{
		"requestId":   reqId,
		"medium":      "webhook",
		"target":      "https://hook.example/co63",
		"body":        "body-v2",
		"dedupeKey":   "co63:label:" + reqId,
		"requestedBy": "test:createonly",
	})

	afterRestage := latestPayload(t, ctx, db, conceptName, canonicalId)
	require.Equal(t, "sent", afterRestage["status"],
		"fylo#63: a re-stage must NOT reset an already-sent row to pending (@createOnly status)")
	require.Equal(t, float64(1), afterRestage["attempts"],
		"fylo#63: a re-stage must NOT reset attempts (@createOnly attempts)")
	require.Equal(t, "2026-01-02T03:04:05Z", afterRestage["sentAt"],
		"sentAt (worker-owned, never written by stageOutboundRequest) survives the read-merge")
	require.Equal(t, "body-v2", afterRestage["body"],
		"non-createOnly content still refreshes on re-stage (not a blind create-if-absent)")
}
