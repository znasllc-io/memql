package memql

// count_window_db_test.go -- a count that reaches the engine's window says so
// (memql#6033), against a REAL engine and a REAL Postgres.
//
// A `count` query reads the matching set and returns its size, and the read
// stops at the engine's window (MEMQL_MEMORY_ENGINE_MAX_WINDOW, 5,000 by
// default). Before this, a set larger than the window answered the window as
// if it were exact -- an audience of 12,000 subscribers read "5,000" -- and a
// caller could not tell that from an audience of exactly 5,000. The count path
// now reads one row past the window, so it can tell: past it, the answer is
// the window and `atLeast` is true.
//
// The window is narrowed to three on a PRIVATE engine (readMergeTestEngine,
// because the test changes engine-level config), so the boundary is reached
// with a handful of rows. Each case seeds its own rows under a per-case
// createdBy scope and counts only those, with a raw count over that scope; a
// distinct query per case also keeps the result cache from answering one case
// with another's figure.

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExecuteCount_SaysWhenTheWindowWasReached(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	const window = 3
	eng.config.MaxWindow = window
	ctx := clusterOwnerCtx("u-count-window")
	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name    string
		rows    int
		count   int64
		atLeast bool
	}{
		{name: "below the window", rows: window - 1, count: window - 1, atLeast: false},
		{name: "exactly the window", rows: window, count: window, atLeast: false},
		{name: "one past the window", rows: window + 1, count: window, atLeast: true},
		{name: "far past the window", rows: window + 5, count: window, atLeast: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := "cw:" + uniqueSuffix(fmt.Sprintf("count-window-%d", tc.rows))
			for i := 0; i < tc.rows; i++ {
				id := fmt.Sprintf("%s:%s-%d", stagedDBConceptLive, owner, i)
				seedStagedRow(t, ctx, db, id, stagedDBConceptLive, base.Add(time.Duration(i)*time.Second), owner)
			}

			// One comparison, and a conjunction the engine compiles into one
			// combined SQL filter: the two scans size their reads differently,
			// and both must read one row past the window.
			for _, query := range []string{
				fmt.Sprintf(`count(createdBy==%q)`, owner),
				fmt.Sprintf(`count(createdBy==%q && concept==%q)`, owner, stagedDBConceptLive),
			} {
				res, err := eng.Execute(ctx, query)
				require.NoError(t, err, query)
				out, ok := res.FlatOutput()
				require.True(t, ok, "a count answers a flat aggregate, not rows: %s", query)
				require.Equal(t, map[string]any{"count": tc.count, "atLeast": tc.atLeast}, out, query)
				require.Equal(t, tc.count, res.GetMeta().Count, "the result's meta carries the same figure: %s", query)
			}
		})
	}
}
