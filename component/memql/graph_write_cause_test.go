package memql

// graph_write_cause_test.go -- a graph write publishes graph.node.created
// (and, on an update() mutation, graph.node.updated too) carrying the cause
// its context held, when it held one (memql#5382, Task 2 of epic memql#5380).
// A later automation triggered by that write derives its own place in the
// chain from the triggering event's cause, so every publisher a run's writes
// reach must forward it -- this is the graph-write half; the publish/event
// step's half is component/automations/steps/event_cause_test.go.
//
// Postgres-gated, same setup as first_version_event_db_test.go: it needs a
// REAL engine (readMergeTestEngine, not the shared one -- SetEventBus mutates
// engine state) driving a real write through executeWrite / executeUpdate.
//
// createNode / updateNodeHealth (not the todos mutations) is the fixture: the
// todos domain has no in-place update{} mutation at all -- its own comment
// says so ("there is no in-place update block") -- so it can never publish
// graph.node.updated. updateNodeHealth is a genuine `update { accept {...} }`
// mutation (dsl/cluster/mutations.memql), already exercised read-merge-side by
// TestReadMerge_UpdateNodeHealth_PreservesAddress in
// executor_mutation_readmerge_db_test.go, which is where this copies its
// createNode/updateNodeHealth call shape from.
import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/id"
)

func TestGraphWriteEventsCarryTheContextCause(t *testing.T) {
	eng, _, ctx := readMergeTestEngine(t)
	bus := events.NewBus()
	eng.SetEventBus(bus)
	t.Cleanup(bus.Close)

	created := make(chan events.Event, 16)
	unsubCreated := bus.Subscribe("graph.node.created.v1:cluster:node", func(ev events.Event) {
		created <- ev
	})
	t.Cleanup(unsubCreated)
	updated := make(chan events.Event, 16)
	unsubUpdated := bus.Subscribe("graph.node.updated.v1:cluster:node", func(ev events.Event) {
		updated <- ev
	})
	t.Cleanup(unsubUpdated)

	cause := events.Cause{
		CausationId:   "run-1",
		CorrelationId: "corr-1",
		Depth:         3,
		Chain: []events.Link{
			{Automation: "a", RunId: "run-0"},
			{Automation: "b", RunId: "run-1"},
		},
	}
	causeCtx := events.ContextWithCause(ctx, cause)

	// Each event is awaited before the next call is made: the bus delivers
	// every event in its own goroutine (first_version_event_db_test.go), so
	// consecutive writes could otherwise arrive out of order.
	awaitCreated := func(label string) events.Event {
		t.Helper()
		select {
		case ev := <-created:
			return ev
		case <-time.After(10 * time.Second):
			t.Fatalf("%s published no graph.node.created", label)
			return events.Event{}
		}
	}
	awaitUpdated := func(label string) events.Event {
		t.Helper()
		select {
		case ev := <-updated:
			return ev
		case <-time.After(10 * time.Second):
			t.Fatalf("%s published no graph.node.updated", label)
			return events.Event{}
		}
	}

	nodeId := "cause-node-" + id.NewShortId()
	runMutation(t, causeCtx, eng, "createNode", map[string]any{
		"id":       nodeId,
		"nodeType": "bff",
		"address":  "10.0.0.7:50051",
		"health":   "healthy",
	})
	require.Equal(t, cause, awaitCreated("createNode").Cause,
		"the insert's .created event carries the write's context cause")

	runMutation(t, causeCtx, eng, "updateNodeHealth", map[string]any{
		"id":       nodeId,
		"health":   "degraded",
		"lastSeen": "2026-06-18T00:00:00Z",
	})
	require.Equal(t, cause, awaitCreated("updateNodeHealth").Cause,
		"update()'s underlying .created event (MemQL is append-only) carries the cause too")
	require.Equal(t, cause, awaitUpdated("updateNodeHealth").Cause,
		"the .updated transition event carries the write's context cause")

	// Negative: a write made under a context carrying NO cause publishes a
	// root event, never a leftover from the writes above.
	rootId := "cause-node-root-" + id.NewShortId()
	runMutation(t, ctx, eng, "createNode", map[string]any{
		"id":       rootId,
		"nodeType": "bff",
		"health":   "healthy",
	})
	require.True(t, awaitCreated("root createNode").Cause.IsZero(),
		"a write with no context cause publishes a root event")
}
