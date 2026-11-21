package calls

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
)

func TestLifecycleScanAdvancesAfterRowFailureAndWraps(t *testing.T) {
	firstError := errors.New("unavailable row")
	seen := []string{}
	scan := lifecycleScan{}
	store := &Store{engine: inventoryEngineFunc(func(ctx context.Context, _ string) (any, error) {
		if memql.CursorFromContext(ctx) == "" {
			return inventoryResult([]map[string]any{{"id": "held"}, {"id": "broken"}}, "later", true), nil
		}
		require.Equal(t, "later", memql.CursorFromContext(ctx))
		return inventoryResult([]map[string]any{{"id": "due"}}, "", false), nil
	})}
	visit := func(row map[string]any) error {
		seen = append(seen, rowString(row, "id"))
		if rowString(row, "id") == "broken" {
			return firstError
		}
		return nil
	}
	ctx := memql.ContextWithCursor(context.Background(), "foreign request cursor")
	require.ErrorIs(t, scan.run(ctx, store, "callsWorkflowStates", 200, visit), firstError)
	require.NoError(t, scan.run(ctx, store, "callsWorkflowStates", 200, visit))
	require.Equal(t, []string{"held", "broken", "due"}, seen)
	require.Empty(t, scan.cursor)
	require.ErrorIs(t, scan.run(ctx, store, "callsWorkflowStates", 200, visit), firstError)
	require.Equal(t, []string{"held", "broken", "due", "held", "broken"}, seen)
}

func TestLifecycleScanCancellationRetainsPageAndSkipsConcurrentPass(t *testing.T) {
	scan := lifecycleScan{cursor: "before"}
	reads := 0
	store := &Store{engine: inventoryEngineFunc(func(context.Context, string) (any, error) {
		reads++
		return inventoryResult([]map[string]any{{"id": "one"}, {"id": "two"}}, "after", true), nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	visited := 0
	require.ErrorIs(t, scan.run(ctx, store, "callsResourceStates", 200, func(map[string]any) error {
		visited++
		cancel()
		return nil
	}), context.Canceled)
	require.Equal(t, 1, visited)
	require.Equal(t, "before", scan.cursor)
	require.ErrorIs(t, scan.run(ctx, store, "callsResourceStates", 200, nil), context.Canceled)
	scan.mu.Lock()
	require.NoError(t, scan.run(context.Background(), store, "callsResourceStates", 200, nil))
	scan.mu.Unlock()
	require.Equal(t, 1, reads)
}

func TestLifecycleScanResetsOnlyInvalidNativeCursor(t *testing.T) {
	for _, failure := range []error{memql.ErrCursorMalformed, memql.ErrCursorSortMismatch, errors.New("read unavailable")} {
		scan := lifecycleScan{cursor: "old"}
		store := &Store{engine: inventoryEngineFunc(func(context.Context, string) (any, error) { return nil, failure })}
		require.ErrorIs(t, scan.run(context.Background(), store, "callsWorkflowStates", 200, nil), failure)
		if errors.Is(failure, memql.ErrCursorMalformed) || errors.Is(failure, memql.ErrCursorSortMismatch) {
			require.Empty(t, scan.cursor)
		} else {
			require.Equal(t, "old", scan.cursor)
		}
	}
}

func TestLifecycleScanRejectsIncompleteContinuationWithoutVisiting(t *testing.T) {
	for _, result := range []any{
		(*memql.ExecuteResult)(nil),
		&memql.ExecuteResult{Meta: &memql.ResultMeta{}},
		inventoryResult([]map[string]any{{"id": "one"}}, "", true),
		inventoryResult([]map[string]any{{"id": "one"}}, "old", true),
		inventoryResult([]map[string]any{}, "next", true),
		inventoryResult([]map[string]any{{"id": "one"}, {"id": "two"}}, "", false),
		memql.NewResultWithOutput("invalid"),
	} {
		scan := lifecycleScan{cursor: "old"}
		store := &Store{engine: inventoryEngineFunc(func(context.Context, string) (any, error) { return result, nil })}
		require.Error(t, scan.run(context.Background(), store, "callsResourceStates", 1, func(map[string]any) error {
			t.Fatal("an incomplete page is not safe to process")
			return nil
		}))
	}
}

func TestLifecycleScanRejectsShortCursorCycleAndRestarts(t *testing.T) {
	scan := lifecycleScan{}
	next := []string{"page-two", "page-three", "page-two"}
	reads, visits := 0, 0
	store := &Store{engine: inventoryEngineFunc(func(context.Context, string) (any, error) {
		cursor := next[reads]
		reads++
		return inventoryResult([]map[string]any{{"id": cursor}}, cursor, true), nil
	})}
	visit := func(map[string]any) error { visits++; return nil }
	require.NoError(t, scan.run(context.Background(), store, "callsResourceStates", 200, visit))
	require.NoError(t, scan.run(context.Background(), store, "callsResourceStates", 200, visit))
	require.ErrorIs(t, scan.run(context.Background(), store, "callsResourceStates", 200, visit), errLifecycleContinuation)
	require.Equal(t, 2, visits)
	require.Empty(t, scan.cursor)
	require.Empty(t, scan.recent)
}
