package memql

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/common"
)

func TestInboundRetryWritesAndDispatchesOnlyChangedContent(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	bus := events.NewBus()
	eng.SetEventBus(bus)
	t.Cleanup(bus.Close)
	arrivals := make(chan events.Event, 16)
	unsubscribe := bus.Subscribe("graph.node.created.v1:platform:inboundRequest", func(ev events.Event) { arrivals <- ev })
	t.Cleanup(unsubscribe)
	writes := 0
	ctx = common.ContextWithWriteObserver(auth.ContextWithInternalOrigin(ctx), func(concept, id string) {
		if concept == inboundRequestConcept {
			writes++
		}
	})
	args := map[string]any{"requestId": uniqueSuffix("inbound-no-change"), "source": "acme", "medium": "webhook", "body": "original body", "receivedAt": "2026-10-13T00:00:00Z", "headersJson": "{}", "verifiedBy": "env", "signatureVerified": true}
	await := func() {
		t.Helper()
		select {
		case <-arrivals:
		case <-time.After(5 * time.Second):
			t.Fatal("changed delivery published no event")
		}
	}
	id := runMutation(t, ctx, eng, "stageInboundRequest", args)
	await()
	runMutation(t, ctx, eng, "updateInboundRequestStatus", map[string]any{"requestId": args["requestId"], "status": "processed", "processedAt": "2026-10-13T00:01:00Z"})
	await()
	writes = 0
	for i := 0; i < 4; i++ {
		// Receiver-local delivery timestamps and headers are create-only; a retry
		// must compare the merged row, not the incoming partial payload.
		args["receivedAt"] = "2026-10-13T01:00:00Z"
		args["headersJson"] = `{"retry":true}`
		require.Equal(t, id, runMutation(t, ctx, eng, "stageInboundRequest", args))
	}
	var versions int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id=?`, id).Scan(&versions))
	require.Equal(t, 2, versions)
	require.Zero(t, writes, "a duplicate must not open the held work journal")
	select {
	case <-arrivals:
		t.Fatal("a duplicate delivery was redispatched")
	case <-time.After(100 * time.Millisecond):
	}
	require.Equal(t, "processed", latestPayload(t, ctx, db, inboundRequestConcept, id)["status"])
	args["body"] = "changed body"
	runMutation(t, ctx, eng, "stageInboundRequest", args)
	await()
	require.Equal(t, 1, writes)
	require.Equal(t, "changed body", latestPayload(t, ctx, db, inboundRequestConcept, id)["body"])
	require.Equal(t, "processed", latestPayload(t, ctx, db, inboundRequestConcept, id)["status"])
}

func TestUnchangedInboundComparisonPreservesLargeNumbers(t *testing.T) {
	require.True(t, sameJSON([]byte(`{"a":1,"b":2}`), []byte(`{"b":2,"a":1}`)))
	require.False(t, sameJSON([]byte(`{"a":9007199254740992}`), []byte(`{"a":9007199254740993}`)))
}
