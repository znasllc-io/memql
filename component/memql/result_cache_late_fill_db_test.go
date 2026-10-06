package memql

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

type pauseCacheReadKey struct{}
type pauseCacheRead struct {
	once         sync.Once
	read, resume chan struct{}
}

func (h *pauseCacheRead) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}
func (h *pauseCacheRead) AfterQuery(ctx context.Context, e *bun.QueryEvent) {
	if ctx.Value(pauseCacheReadKey{}) != true || e.Err != nil || !strings.HasPrefix(e.Query, "SELECT") || !strings.Contains(e.Query, "MemoryNodes") || !strings.Contains(e.Query, "id IN (") {
		return
	}
	h.once.Do(func() {
		close(h.read)
		select {
		case <-h.resume:
		case <-ctx.Done():
		}
	})
}

func TestResultCacheInflightStorageReadCannotRestorePreWriteRows(t *testing.T) {
	// Private pool: the hook must never pause another test's queries.
	eng, db, _ := readMergeTestEngine(t)
	owner := rowAuthzCallerCtx("late-cache-owner-" + uniqueSuffix("u"))
	todo := "late-cache-" + uniqueSuffix("todo")
	runMutation(t, owner, eng, "createTodo", map[string]any{"todoId": todo, "title": "before"})
	hook := &pauseCacheRead{read: make(chan struct{}), resume: make(chan struct{})}
	db.AddQueryHook(hook)
	ctx, cancel := context.WithTimeout(context.WithValue(owner, pauseCacheReadKey{}, true), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	query := `query todoById(todoId: "` + todo + `")`
	go func() {
		result, err := eng.Execute(ctx, query)
		if err == nil {
			rows := MaterializeRows(result)
			if len(rows) != 1 || rows[0]["title"] != "before" {
				err = fmt.Errorf("positive control: blocked read did not hold the pre-write row: %v", rows)
			}
		}
		done <- err
	}()
	select {
	case <-hook.read:
	case <-ctx.Done():
		t.Fatal("the query did not reach storage")
	}
	runMutation(t, owner, eng, "updateTodo", map[string]any{"todoId": todo, "payload": map[string]any{"title": "after"}})
	close(hook.resume)
	require.NoError(t, <-done)
	eng.cache.cache.Wait()
	result, err := eng.Execute(owner, query)
	require.NoError(t, err)
	rows := MaterializeRows(result)
	require.Len(t, rows, 1)
	require.Equal(t, "after", rows[0]["title"], "a completed write must not be hidden by a query that started before it")
}
