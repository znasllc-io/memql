package memql

// first_version_event_db_test.go -- graph.node.created says whether the write
// materialised the row's FIRST version (memql#5368).
//
// Every write publishes graph.node.created, because the store is append-only
// and every write lands a new version. An email rule on "created" read the
// topic and fired on every change to the row: a welcome email when a user is
// created went out again on every later write to that user. The marker is what
// lets a created rule fire once per row, and it is read off the prior-version
// lookup the write path already makes.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/id"
)

func TestCreatedEventSaysWhetherTheWriteIsTheRowsFirstVersion(t *testing.T) {
	eng, _, ctx := readMergeTestEngine(t)
	bus := events.NewBus()
	eng.SetEventBus(bus)
	t.Cleanup(bus.Close)

	created := make(chan map[string]any, 16)
	unsubscribe := bus.Subscribe("graph.node.created.v1:todos:todo", func(ev events.Event) {
		created <- ev.Payload
	})
	t.Cleanup(unsubscribe)

	// Each write is awaited before the next: the bus delivers every event
	// in its own goroutine, so consecutive writes could otherwise arrive out
	// of order.
	write := func(name string, args map[string]any) map[string]any {
		t.Helper()
		stored := runMutation(t, ctx, eng, name, args)
		select {
		case payload := <-created:
			require.Equal(t, stored, payload["id"], "the event belongs to the write just made")
			return payload
		case <-time.After(10 * time.Second):
			t.Fatalf("%s published no graph.node.created", name)
			return nil
		}
	}

	first := "fv-" + id.NewShortId()
	require.Equal(t, true, write("createTodo", map[string]any{"todoId": first, "title": "a"})["firstVersion"],
		"the insert that materialises a new row is its first version")
	require.Equal(t, false, write("updateTodo", map[string]any{"todoId": first, "payload": map[string]any{"title": "b"}})["firstVersion"],
		"a later write to the same row is not, although it publishes .created too")
	require.Equal(t, false, write("updateTodo", map[string]any{"todoId": first, "payload": map[string]any{"title": "c"}})["firstVersion"])

	second := "fv-" + id.NewShortId()
	require.Equal(t, true, write("createTodo", map[string]any{"todoId": second, "title": "a"})["firstVersion"],
		"a second new row is a first version of its own")
}
