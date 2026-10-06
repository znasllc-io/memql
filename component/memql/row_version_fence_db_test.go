package memql

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func fenceTodo(t *testing.T, eng *MemQLEngine, ctx context.Context, suffix string) string {
	t.Helper()
	return runMutation(t, ctx, eng, "createTodo", map[string]any{"todoId": "fence-" + suffix + "-" + testRunNonce, "title": "Before"})
}
func fenceTodoVersion(t *testing.T, db *bun.DB, id string) time.Time {
	t.Helper()
	var at time.Time
	require.NoError(t, db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).Column("createdAt").Where("concept = ?", "v1:todos:todo").Where("id = ?", id).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background(), &at))
	return at
}
func updateFencedTodo(id, title string) string {
	return fmt.Sprintf(`mutation updateTodo(todoId: %s, payload: {title: %s})`, langparser.QuoteString(id), langparser.QuoteString(title))
}

// The same primitive protects a document-derived result or a task mutation;
// it knows nothing about pipelines, workers or CI policy.
func TestRowVersionFenceRefusesAStaleDecisionAndDoesNotGrantAuthority(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	ctx = auth.ContextWithUserActor(ctx, "fence-owner")
	source := fenceTodo(t, eng, ctx, "source")
	target := fenceTodo(t, eng, ctx, "result")
	observed := fenceTodoVersion(t, db, source)
	stale := ContextWithRowVersionFence(ctx, "v1:todos:todo", source, observed)
	_, err := eng.Execute(stale, updateFencedTodo(source, "New source"))
	require.NoError(t, err)
	_, err = eng.Execute(stale, updateFencedTodo(target, "Derived from old source"))
	require.ErrorIs(t, err, ErrRowVersionChanged)
	require.Equal(t, "Before", latestPayload(t, ctx, db, "v1:todos:todo", target)["title"])
	other := auth.ContextWithUserActor(context.Background(), "somebody-else")
	other = ContextWithRowVersionFence(other, "v1:todos:todo", target, fenceTodoVersion(t, db, target))
	_, err = eng.Execute(other, updateFencedTodo(target, "Not mine"))
	require.Error(t, err)
	require.Equal(t, "Before", latestPayload(t, ctx, db, "v1:todos:todo", target)["title"])
	// Refreshing a later fence cannot remove an earlier, stale witness.
	refreshed := ContextWithRowVersionFence(stale, "v1:todos:todo", source, fenceTodoVersion(t, db, source))
	_, err = eng.Execute(refreshed, updateFencedTodo(target, "Erased old guard"))
	require.ErrorIs(t, err, ErrRowVersionChanged)
}

func TestRowVersionFenceLetsOnlyOneConcurrentDecisionCommit(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	ctx = auth.ContextWithUserActor(ctx, "fence-race-owner")
	row := fenceTodo(t, eng, ctx, "concurrent")
	fenced := ContextWithRowVersionFence(ctx, "v1:todos:todo", row, fenceTodoVersion(t, db, row))
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, title := range []string{"First", "Second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := eng.Execute(fenced, updateFencedTodo(row, title))
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, ErrRowVersionChanged)
		}
	}
	require.Equal(t, 1, successes)
}

func TestRowVersionFenceSurvivesAReplacementClocksSkew(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	ctx = auth.ContextWithUserActor(ctx, "fence-clock-owner")
	row := fenceTodo(t, eng, ctx, "clock")
	var future memorynodes.MemoryNode
	require.NoError(t, db.NewSelect().Model(&future).Where("id = ?", row).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx))
	future.CreatedAt = time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	_, err := db.NewInsert().Model(&future).Exec(ctx)
	require.NoError(t, err)
	fenced := ContextWithRowVersionFence(ctx, "v1:todos:todo", row, future.CreatedAt)
	_, err = eng.Execute(fenced, updateFencedTodo(row, "Replacement"))
	require.NoError(t, err)
	require.True(t, fenceTodoVersion(t, db, row).After(future.CreatedAt))
	require.Equal(t, "Replacement", latestPayload(t, ctx, db, "v1:todos:todo", row)["title"])
}

func TestRowVersionFenceRollsBackWhenTheWriterConnectionDies(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	ctx = auth.ContextWithUserActor(ctx, "fence-rollback-owner")
	source := fenceTodo(t, eng, ctx, "rollback-source")
	fenced := ContextWithRowVersionFence(ctx, "v1:todos:todo", source, fenceTodoVersion(t, db, source))
	target := "fence-uncommitted-" + testRunNonce
	metadata, err := eng.concepts.Get("v1:todos:todo")
	require.NoError(t, err)
	err = eng.runInWriteTx(fenced, func(store memorynodes.Store) error {
		_, err := metadata.Create(fenced, store, memorynodes.CreateParams{Actor: "fence-rollback-owner", ID: target, Payload: map[string]any{"title": "Uncommitted", "done": false, "ownerUserId": "fence-rollback-owner"}})
		if err != nil {
			return err
		}
		tx := store.(*bunStore).db
		var pid int
		if err := tx.QueryRowContext(fenced, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return err
		}
		var statement, idle string
		if err := tx.QueryRowContext(fenced, `SELECT current_setting('statement_timeout'), current_setting('idle_in_transaction_session_timeout')`).Scan(&statement, &idle); err != nil {
			return err
		}
		if statement == "0" || idle == "0" {
			return fmt.Errorf("fenced critical section has no database timeout")
		}
		var terminated bool
		if err := db.QueryRowContext(ctx, `SELECT pg_terminate_backend(?)`, pid).Scan(&terminated); err != nil {
			return err
		}
		if !terminated {
			return fmt.Errorf("test writer session was not terminated")
		}
		return nil
	})
	require.Error(t, err, "a lost transaction connection reported success")
	count, err := db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", "v1:todos:todo").Where("id = ?", "v1:todos:todo:"+target).Count(ctx)
	require.NoError(t, err)
	require.Zero(t, count, "a receipt survived the loss of its fence transaction")
}
