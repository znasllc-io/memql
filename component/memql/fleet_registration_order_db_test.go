package memql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// Heartbeats append new row versions. Ordering by that version's createdAt
// rotated firstFit between machines on every heartbeat, losing model caches.
// Both replicas must read fresh liveness while retaining registration order.
func TestFleetRegistrationOrderSurvivesHeartbeatVersionsAcrossReplicas(t *testing.T) {
	writer, db, _ := sharedReadMergeEngine(t)
	reader, err := New(db)
	require.NoError(t, err)
	require.NoError(t, reader.Init(memorynodes.DefaultRegistry()))
	f := shareDirFixture{t: t, eng: writer, db: db, prefix: uniqueSuffix("fleet-order")}
	owner := f.person("owner", true)
	other := f.person("other", true)
	f.machine("other-owner", other)
	registered := time.Now().UTC().Add(-time.Hour)
	ids := []string{f.id("v1:worker:registration", "first"), f.id("v1:worker:registration", "second"), f.id("v1:worker:registration", "third")}
	beat := func(index int, active int) {
		t.Helper()
		f.insert("v1:worker:registration", ids[index], map[string]any{
			"ownerUserId": owner, "name": fmt.Sprintf("machine-%d", index),
			"identityId":      f.id("v1:identity:identity", fmt.Sprint(index)),
			"registeredAt":    registered.Add(time.Duration(index) * time.Minute).Format(time.RFC3339Nano),
			"lastSeenAt":      time.Now().UTC().Format(time.RFC3339Nano),
			"connectedNodeId": fmt.Sprintf("agent-%d", index%2), "activeCount": active,
		})
	}
	for i := range ids {
		beat(i, 0)
	}
	check := func(active int) {
		t.Helper()
		for replica, eng := range []*MemQLEngine{writer, reader} {
			for _, query := range []string{`query myWorkersWithStatus()`, `query allWorkersWithStatus(ownerUserId: ` + langparser.QuoteString(owner) + `)`} {
				ctx := shareDirActor(context.Background(), owner, auth.RoleOwner)
				result, err := eng.Execute(ctx, query)
				require.NoError(t, err, "replica %d: %s", replica, query)
				require.Len(t, result.Bundle.Nodes, len(ids))
				for i, node := range result.Bundle.Nodes {
					require.Equal(t, BareShortId(ids[i]), BareShortId(node.Id), "replica %d: %s must preserve firstFit ordering", replica, query)
				}
				payload := result.Bundle.Nodes[0].Payload.AsMap()
				require.EqualValues(t, active, payload["activeCount"], "stable ordering must not return stale heartbeat data")
			}
		}
	}
	check(0)
	beat(0, 1)
	check(1)
	beat(1, 2)
	beat(0, 3)
	check(3)
}
