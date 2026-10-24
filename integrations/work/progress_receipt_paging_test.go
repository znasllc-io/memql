package work

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
)

type progressPagesEngine struct {
	Engine
	reads  int
	broken bool
}

func (e *progressPagesEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if !strings.Contains(query, "workProgressReceiptsForOwnerRun") {
		return e.Engine.Execute(ctx, query)
	}
	e.reads++
	if memql.CursorFromContext(ctx) == "older" {
		if e.broken {
			return nil, fmt.Errorf("saved receipt page unavailable")
		}
		return memql.NewResultWithOutput([]any{progressReceipt("older substantive result")}), nil
	}
	var rows []any
	for n := 0; n < 100; n++ {
		row := progressReceipt(fmt.Sprint(n))
		rowMap(row, "data")["tool"] = "recallWorkHistory"
		rows = append(rows, row)
	}
	result := memql.NewResultWithOutput(rows)
	result.SetCursor("older")
	result.SetHasMore(true)
	return result, nil
}

func TestProgressReadsPastNonProgressReceiptPage(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			i, e, _, rc := progressFixture(t)
			pages := &progressPagesEngine{Engine: e, broken: broken}
			i.engine = pages
			ctx := memql.ContextWithCursor(context.Background(), "unrelated caller cursor")
			ok, err := i.ContinueWorkProgress(ctx, rc, workstate.Spent{ModelCalls: 12}, 0)
			require.Equal(t, !broken, ok)
			if broken {
				require.ErrorContains(t, err, "saved receipt page unavailable")
				require.Empty(t, e.callsTo("updateWorkRun"))
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, 2, pages.reads)
		})
	}
}

func TestProgressRenewalDoesNotSkipConcurrentReceipt(t *testing.T) {
	i, e, run, rc := progressFixture(t)
	first := progressReceipt("available at read")
	first["createdAt"] = rfc(testNow.Add(-2 * time.Second))
	e.reply("workProgressReceiptsForOwnerRun", first)
	ok, err := i.ContinueWorkProgress(context.Background(), rc, workstate.Spent{ModelCalls: 12}, 0)
	require.NoError(t, err)
	require.True(t, ok)
	run["classification"] = e.callsTo("updateWorkRun")[0].Args(t)["classification"]
	// This receipt became visible after the read but before the window write.
	// It must still be eligible on a different replica's next renewal.
	concurrent := progressReceipt("arrived during renewal")
	concurrent["createdAt"] = rfc(testNow.Add(-time.Second))
	e.reply("workProgressReceiptsForOwnerRun", concurrent)
	peer := New(e, testLogger())
	peer.decisionGate = i.decisionGate
	ok, err = peer.ContinueWorkProgress(context.Background(), rc, workstate.Spent{ModelCalls: 24}, 0)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, e.callsTo("updateWorkRun"), 2)
}
