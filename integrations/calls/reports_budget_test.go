package calls

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
)

func TestReportsBudgetRefusesIncompleteInventory(t *testing.T) {
	for _, scenario := range []string{"nil", "missing_meta", "bad_row", "without_cursor", "cycle", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			store := &Store{engine: inventoryEngineFunc(func(context.Context, string) (any, error) {
				reads++
				switch scenario {
				case "nil":
					return (*memql.ExecuteResult)(nil), nil
				case "missing_meta":
					return &memql.ExecuteResult{}, nil
				case "bad_row":
					return memql.NewResultWithOutput([]any{false}), nil
				case "without_cursor":
					return inventoryResult([]map[string]any{}, "", true), nil
				case "canceled":
					cancel()
					return inventoryResult([]map[string]any{}, "", false), nil
				default:
					return inventoryResult([]map[string]any{{"id": fmt.Sprint(reads)}}, fmt.Sprint(reads%2), true), nil
				}
			})}
			err := store.reportBudget(ctx, "budget-owner", "budget-site", "budget-key", "budget-room", time.Now())
			require.Error(t, err, "a partial or malformed result must never authorize a report")
			if scenario == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
			}
			require.Greater(t, reads, 0)
			require.LessOrEqual(t, reads, 3)
		})
	}
}
