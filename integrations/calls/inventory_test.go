package calls

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
)

type inventoryEngineFunc func(context.Context, string) (any, error)

func (f inventoryEngineFunc) Execute(ctx context.Context, q string) (any, error) { return f(ctx, q) }

func inventoryResult(rows []map[string]any, cursor string, more bool) *memql.ExecuteResult {
	r := memql.NewResultWithOutput(rows)
	r.SetMeta(&memql.ResultMeta{Cursor: cursor, HasMore: more})
	return r
}

func TestCompleteAccountingInventoryRejectsUnknownAndCyclicPages(t *testing.T) {
	for _, scenario := range []string{"nil", "missing_payload", "scalar", "bad_row", "without_cursor", "empty_cursor_page", "duplicate_row", "repeated_cursor", "short_cursor_cycle", "budget"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			store := &Store{engine: inventoryEngineFunc(func(ctx context.Context, q string) (any, error) {
				calls++
				switch scenario {
				case "nil":
					return (*memql.ExecuteResult)(nil), nil
				case "missing_payload":
					return &memql.ExecuteResult{Meta: &memql.ResultMeta{}}, nil
				case "scalar":
					return memql.NewResultWithOutput("unknown"), nil
				case "bad_row":
					return memql.NewResultWithOutput([]any{false}), nil
				case "without_cursor":
					return inventoryResult(nil, "", true), nil
				case "empty_cursor_page":
					return inventoryResult(nil, "cursor", true), nil
				case "duplicate_row":
					return inventoryResult([]map[string]any{{"id": "same"}}, fmt.Sprint(calls), true), nil
				case "repeated_cursor":
					return inventoryResult([]map[string]any{{"id": fmt.Sprint(calls)}}, "same", true), nil
				case "short_cursor_cycle":
					return inventoryResult([]map[string]any{{"id": fmt.Sprint(calls)}}, fmt.Sprint(calls%2), true), nil
				default:
					return inventoryResult([]map[string]any{{"id": fmt.Sprint(calls)}}, fmt.Sprint(calls), true), nil
				}
			})}
			rows, err := store.readCompleteClusterInventory(context.Background(), "test inventory", "queryName()", nil, 1000)
			require.ErrorIs(t, err, errIncompleteInventory)
			require.Nil(t, rows)
			require.LessOrEqual(t, calls, 5)
		})
	}
}

func TestCompleteAccountingInventoryPreservesCancellationAndScrubsReadFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	store := &Store{engine: inventoryEngineFunc(func(ctx context.Context, q string) (any, error) {
		calls++
		return nil, errors.New("sensitive fixture token")
	})}
	_, err := store.readCompleteClusterInventory(ctx, "test inventory", "queryName()", nil, 1000)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, calls)
	_, err = store.readCompleteClusterInventory(context.Background(), "test inventory", "queryName()", nil, 1000)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "sensitive fixture token")
	store.engine = inventoryEngineFunc(func(context.Context, string) (any, error) { return inventoryResult([]map[string]any{}, "", false), nil })
	rows, err := store.readCompleteClusterInventory(context.Background(), "test inventory", "queryName()", nil, 1000)
	require.NoError(t, err)
	require.Empty(t, rows)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	store.engine = inventoryEngineFunc(func(context.Context, string) (any, error) {
		cancel()
		return inventoryResult([]map[string]any{}, "", false), nil
	})
	_, err = store.readCompleteClusterInventory(ctx, "test inventory", "queryName()", nil, 1000)
	require.ErrorIs(t, err, context.Canceled)
}
