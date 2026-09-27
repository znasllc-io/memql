package memql

// fresh_read_db_test.go -- a fresh read sees a write another replica committed
// a moment ago; an ordinary read is answered by this node's result cache until
// the invalidation broadcast arrives (memql#5431).
//
// Two engines over one database are two replicas: each has its own cache, and a
// test process runs no mesh, so the broadcast that would evict this node's
// entry never comes. That is the window a cross-replica read-modify-write
// lives in, held open.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFreshReadSeesAnotherReplicasWriteThisNodesCacheHasNotHeardAbout(t *testing.T) {
	here, _, _ := readMergeTestEngine(t)
	there, _, _ := sharedReadMergeEngine(t)

	owner := rowAuthzCallerCtx("fresh-read-owner-" + uniqueSuffix("u"))
	todo := "fresh-read-" + uniqueSuffix("todo")
	runMutation(t, owner, here, "createTodo", map[string]any{"todoId": todo, "title": "before"})

	read := func(ctx context.Context) string {
		t.Helper()
		res, err := here.Execute(ctx, `query todoById(todoId: "`+todo+`")`)
		require.NoError(t, err)
		rows := MaterializeRows(res)
		require.Len(t, rows, 1, "the owner reads their own to-do")
		title, _ := rows[0]["title"].(string)
		return title
	}
	require.Equal(t, "before", read(owner), "the first read fills this node's cache")

	// The other replica writes. This node is told nothing.
	runMutation(t, owner, there, "updateTodo", map[string]any{"todoId": todo, "payload": map[string]any{"title": "after"}})

	require.Equal(t, "before", read(owner),
		"the positive control: an ordinary read is answered by this node's cache, which has not heard of the other replica's write -- "+
			"if this fails, the cache was never in play and the assertion below proves nothing")
	require.Equal(t, "after", read(ContextWithFreshRead(owner)),
		"a fresh read is answered by the database, and sees the write the moment it committed")
}
