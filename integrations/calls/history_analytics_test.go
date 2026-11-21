package calls

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
)

func TestHistoryAnalyticsMalformedInventoryNeverBecomesCompleteZero(t *testing.T) {
	first := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"nil", "missing_meta", "missing_payload", "scalar", "malformed_list", "without_cursor", "empty_cursor_page", "duplicate_row", "short_cursor_cycle"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			store := &Store{engine: inventoryEngineFunc(func(context.Context, string) (any, error) {
				calls++
				switch scenario {
				case "nil":
					return (*memql.ExecuteResult)(nil), nil
				case "missing_meta":
					r := memql.NewResultWithOutput([]map[string]any{})
					r.Meta = nil
					return r, nil
				case "missing_payload":
					return &memql.ExecuteResult{Meta: &memql.ResultMeta{}}, nil
				case "scalar":
					return memql.NewResultWithOutput("not a list"), nil
				case "malformed_list":
					return memql.NewResultWithOutput([]any{false}), nil
				case "without_cursor":
					return inventoryResult(nil, "", true), nil
				case "empty_cursor_page":
					return inventoryResult(nil, "cursor", true), nil
				}
				rowID := fmt.Sprint("entry-", calls)
				if scenario == "duplicate_row" {
					rowID = "same"
				}
				row := map[string]any{"id": rowID, "siteId": "site", "keyId": "key", "kind": "topup", "requestDigest": strings.Repeat("a", 64), "createdAt": first.Add(time.Hour), "balanceCents": 1000, "creditedCents": 1000, "debitedCents": 0}
				return inventoryResult([]map[string]any{row}, fmt.Sprint(calls%2), true), nil
			})}
			rows, limited, err := store.historyLedgerEntries(context.Background(), "owner", "site", "key", first, first.AddDate(0, 0, 1))
			require.True(t, err != nil || limited, "missing/partial source must never become a completed zero")
			require.Nil(t, rows)
			require.LessOrEqual(t, calls, 3)
		})
	}
}
func TestHistoryAnalyticsInventoryCancellationAfterRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &Store{engine: inventoryEngineFunc(func(context.Context, string) (any, error) {
		cancel()
		return inventoryResult([]map[string]any{}, "", false), nil
	})}
	first := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	rows, _, err := store.historyLedgerEntries(ctx, "owner", "site", "key", first, first.AddDate(0, 0, 1))
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
}
