package memql

// version_time_event_db_test.go -- a graph write's events carry the time of
// the version they announce, exactly as the row keeps it (memql#6033).
//
// A row's version time is its `createdAt`: the store is append-only, so every
// write lands a new version, and the latest version is the one with the
// greatest createdAt. A client folding events onto rows it read compares that
// field to tell an older version from a newer one -- the bus delivers every
// event in its own goroutine, so they do not arrive in the order they were
// written, and a read can be answered after an event it is newer than. That
// comparison needs the field on every event and at the precision the read
// returns: .created used to carry it to the second (a newer version in the
// same second compared as older than the row a read returned), and .updated
// did not carry it at all.
//
// Postgres-gated, same fixture as graph_write_cause_test.go: createNode /
// updateNodeHealth, the latter a genuine `update { ... }` mutation that
// publishes .updated as well as .created.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/id"
)

func TestGraphWriteEventsCarryTheVersionTimeTheRowKeeps(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	bus := events.NewBus()
	eng.SetEventBus(bus)
	t.Cleanup(bus.Close)

	created := make(chan map[string]any, 16)
	unsubCreated := bus.Subscribe("graph.node.created.v1:cluster:node", func(ev events.Event) {
		created <- ev.Payload
	})
	t.Cleanup(unsubCreated)
	updated := make(chan map[string]any, 16)
	unsubUpdated := bus.Subscribe("graph.node.updated.v1:cluster:node", func(ev events.Event) {
		updated <- ev.Payload
	})
	t.Cleanup(unsubUpdated)

	await := func(ch chan map[string]any, label string) map[string]any {
		t.Helper()
		select {
		case payload := <-ch:
			return payload
		case <-time.After(10 * time.Second):
			t.Fatalf("%s published nothing", label)
			return nil
		}
	}

	nodeId := "version-node-" + id.NewShortId()
	stored := runMutation(t, ctx, eng, "createNode", map[string]any{
		"id":       nodeId,
		"nodeType": "bff",
		"address":  "10.0.0.7:50051",
		"health":   "healthy",
	})
	first := storedVersionTime(t, ctx, db, "v1:cluster:node", stored)
	requireVersionTime(t, first, await(created, "createNode")["createdAt"],
		"the insert's .created event carries the version time the row keeps")

	runMutation(t, ctx, eng, "updateNodeHealth", map[string]any{
		"id":       nodeId,
		"health":   "degraded",
		"lastSeen": "2026-06-18T00:00:00Z",
	})
	second := storedVersionTime(t, ctx, db, "v1:cluster:node", stored)
	require.True(t, second.After(first), "the update landed a newer version")
	requireVersionTime(t, second, await(created, "updateNodeHealth")["createdAt"],
		"update()'s underlying .created event carries the NEW version's time")
	requireVersionTime(t, second, await(updated, "updateNodeHealth")["createdAt"],
		"the .updated event carries the new version's time too")
}

// storedVersionTime reads the createdAt of the row's latest version, as the
// column keeps it -- what any read of the row returns.
func storedVersionTime(t *testing.T, ctx context.Context, db *bun.DB, conceptName, rowId string) time.Time {
	t.Helper()
	var node memorynodes.MemoryNode
	err := db.NewSelect().Model(&node).
		Where("concept = ?", conceptName).
		Where("id = ?", rowId).
		Order("createdAt DESC").
		Limit(1).
		Scan(ctx)
	require.NoError(t, err, "read-back of %s/%s failed", conceptName, rowId)
	return node.CreatedAt
}

// requireVersionTime asserts an event's createdAt names exactly `want`, in
// RFC 3339 with every fractional digit the instant has: not to the second.
func requireVersionTime(t *testing.T, want time.Time, got any, msg string) {
	t.Helper()
	stamp, ok := got.(string)
	require.True(t, ok, "%s: createdAt is a string on the event, got %T (%v)", msg, got, got)
	parsed, err := time.Parse(time.RFC3339Nano, stamp)
	require.NoError(t, err, "%s: createdAt %q parses as RFC 3339", msg, stamp)
	require.True(t, want.Equal(parsed), "%s: want %s, got %s", msg, want.UTC().Format(time.RFC3339Nano), stamp)
}
