package memql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
)

// TestUpdateInboundRequestStatusRefusesAClient is the refusal the memql#5707
// follow-up bought, on a real engine over a real Postgres.
//
// v1:platform:inboundRequest declares no @rowAuthz, so before
// updateInboundRequestStatus carried @serverOnly any authenticated caller
// could stamp any staged delivery: mark a privacy request `processed` that no
// connector worked, or `failed` one it did, and the operator's queue would say
// so. A client is refused now -- a cluster owner included, because the
// property is the CHANNEL, not the rank -- and the row is untouched. The
// server-side stamp, under internal origin, still lands.
func TestUpdateInboundRequestStatusRefusesAClient(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	const conceptName = "v1:platform:inboundRequest"
	internal := auth.ContextWithInternalOrigin(ctx)

	reqId := "in5707-" + uniqueSuffix("status-server-only")
	canonicalId := runMutation(t, internal, eng, "stageInboundRequest", map[string]any{
		"requestId":         reqId,
		"source":            "acme",
		"medium":            "webhook",
		"body":              `{"a":1}`,
		"signatureVerified": true,
		"receivedAt":        "2026-09-27T12:00:00Z",
	})

	owner := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "owner-5707", Role: auth.RoleOwner})
	_, err := eng.Execute(auth.ContextWithClientOrigin(owner),
		`mutation updateInboundRequestStatus(requestId: "`+reqId+`", status: "processed")`)
	require.Error(t, err, "a client stamped a staged delivery's handling state")
	require.Contains(t, err.Error(), "server-only")
	require.Equal(t, "received", latestPayload(t, internal, db, conceptName, canonicalId)["status"],
		"the refused stamp must leave the row as the receiver wrote it")

	runMutation(t, internal, eng, "updateInboundRequestStatus", map[string]any{
		"requestId":   reqId,
		"status":      "failed",
		"lastError":   "downstream refused",
		"processedAt": "2026-09-27T12:01:00Z",
	})
	require.Equal(t, "failed", latestPayload(t, internal, db, conceptName, canonicalId)["status"],
		"the server-side stamp must still land")
}
