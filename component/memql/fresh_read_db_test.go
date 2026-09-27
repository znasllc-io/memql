package memql

// fresh_read_db_test.go -- a fresh read sees a write another replica committed
// a moment ago; an ordinary read is answered by this node's result cache until
// the invalidation broadcast arrives (memql#5431).
//
// Two engines over one database are two replicas: each has its own cache, and a
// test process runs no mesh, so the broadcast that would evict this node's
// entry never comes. That is the window a cross-replica read-modify-write
// lives in, held open.
//
// The result cache is Ristretto, whose Set is BUFFERED and may be dropped by its
// admission policy under load, so "the first read filled the cache" has to be
// observed rather than assumed: the positive control below failed in a loaded
// full-package run when the second read arrived before the first read's Set
// had been applied. waitForCachedRead re-reads until this engine's cache
// visibly holds the to-do, which the database still agrees with at that point.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// todoConcept is the concept id a cached to-do read depends on.
const todoConcept = "v1:todos:todo"

// waitForCachedRead re-issues read until eng's result cache holds an entry
// that depends on concept and Ristretto has applied it.
func waitForCachedRead(t *testing.T, eng *MemQLEngine, concept string, read func()) {
	t.Helper()
	require.NotNil(t, eng.cache, "the engine under test has no result cache, so the positive control could never hold")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		read()
		eng.cache.depMu.Lock()
		keys := make([]string, 0, len(eng.cache.depIndex[concept]))
		for k := range eng.cache.depIndex[concept] {
			keys = append(keys, k)
		}
		eng.cache.depMu.Unlock()
		for _, k := range keys {
			if _, ok := eng.cache.get(k); ok {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no cached read of %s became visible in this engine's result cache", concept)
}

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
	waitForCachedRead(t, here, todoConcept, func() { read(owner) })

	// The other replica writes. This node is told nothing.
	runMutation(t, owner, there, "updateTodo", map[string]any{"todoId": todo, "payload": map[string]any{"title": "after"}})

	require.Equal(t, "before", read(owner),
		"the positive control: an ordinary read is answered by this node's cache, which has not heard of the other replica's write -- "+
			"if this fails, the cache was never in play and the assertion below proves nothing")
	require.Equal(t, "after", read(ContextWithFreshRead(owner)),
		"a fresh read is answered by the database, and sees the write the moment it committed")
}
