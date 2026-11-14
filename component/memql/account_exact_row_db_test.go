package memql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

type exactRowQueries struct {
	t     *testing.T
	db    *sql.DB
	width []int
}

func (q *exactRowQueries) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	// Both statements must use a bound id array, regardless of the kind's
	// size: alias resolution asks for two spellings, the head asks for one.
	require.Len(q.t, args, 2)
	value, err := args[1].(driver.Valuer).Value()
	require.NoError(q.t, err)
	var ids pq.StringArray
	require.NoError(q.t, ids.Scan(value))
	q.width = append(q.width, len(ids))
	return q.db.QueryContext(ctx, query, args...)
}

func TestAccountRowHeadByIDUsesTheMoveAliasAndItsLatestObservation(t *testing.T) {
	db := openAccountRowsDB(t)
	ctx := context.Background()
	for _, canonicalNewer := range []bool{true, false} {
		short := "bare-newer"
		if canonicalNewer {
			short = "canonical-newer"
		}
		canonical := rowsRun + ":" + short
		winner, other := short, canonical
		if canonicalNewer {
			winner, other = canonical, short
		}
		rowsVersion(t, db, `"MemoryNodes"`, rowsRun, other, 1, map[string]any{"accountId": rowsBeta})
		rowsVersion(t, db, `"MemoryNodes"`, rowsRun, winner, 2, map[string]any{"accountId": rowsBeta})
		// A newer observation of the OTHER alias must not change which stored
		// row a move resolves. The winner's own observation decides its account.
		rowsVersion(t, db, "memory_liveness", rowsRun, other, 5, map[string]any{"accountId": rowsBeta})
		rowsVersion(t, db, "memory_liveness", rowsRun, winner, 3, map[string]any{"accountId": rowsAcme})
		for _, input := range []string{short, canonical} {
			moveRow, found, err := newestRowOf(ctx, db, rowsRun, input)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, winner, moveRow.ID)
			q := &exactRowQueries{t: t, db: db}
			calls := 0
			require.NoError(t, AccountRowHeadByID(ctx, q, rowsRun, input, func(n memorynodes.MemoryNode, observed bool, first time.Time) error {
				calls++
				require.Equal(t, moveRow.ID, n.ID)
				require.True(t, observed)
				require.Equal(t, rowsClock.Add(3*time.Minute), n.CreatedAt)
				require.Equal(t, rowsClock.Add(2*time.Minute), first)
				var payload map[string]any
				require.NoError(t, json.Unmarshal(n.Payload, &payload))
				require.Equal(t, rowsAcme, payload["accountId"])
				return nil
			}))
			require.Equal(t, 1, calls)
			require.Equal(t, []int{2, 1}, q.width)
		}
	}
}

func TestAccountRowHeadByIDDoesNotReviveAMissingVersionedRow(t *testing.T) {
	db := openAccountRowsDB(t)
	id := rowsRun + ":observation-only"
	rowsVersion(t, db, "memory_liveness", rowsRun, id, 3, map[string]any{"accountId": rowsAcme})
	for _, input := range []string{id, rowsRun + ":missing"} {
		q := &exactRowQueries{t: t, db: db}
		require.NoError(t, AccountRowHeadByID(context.Background(), q, rowsRun, input, func(memorynodes.MemoryNode, bool, time.Time) error {
			t.Fatal("a missing versioned row must not be returned")
			return nil
		}))
		require.Equal(t, []int{2}, q.width)
	}
}
