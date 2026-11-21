package calls

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// Only the history page query uses this second, real engine. Its operator cap
// must not mutate sharedBoot or unrelated test configuration in this process.
type historyCappedEngine struct {
	Engine
	page  Engine
	sizes []int
}

func (e *historyCappedEngine) Execute(ctx context.Context, query string) (any, error) {
	if strings.Contains(query, "callsHistoryForKey(") {
		out, err := e.page.Execute(ctx, query)
		if err == nil {
			result := out.(*memql.ExecuteResult)
			e.sizes = append(e.sizes, len(memql.MaterializeRows(result.OutputPayload())))
		}
		return out, err
	}
	return e.Engine.Execute(ctx, query)
}

func TestHistoryPaginationHonorsLowerNativeResultCap(t *testing.T) {
	cw := newCallWorld(t, "history-lower-result-cap", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host := cw.host()
	completeHistory(t, cw, host)
	h, err := cw.svc.authenticatedKey(ctx, host)
	require.NoError(t, err)
	for i := range 26 {
		started := cw.clock.Now().Add(-time.Duration(i+1) * time.Minute)
		rowID := id.NewShortId()
		insights, resources, agent := i%2 == 0, i%3 == 0, i%4 == 0
		row := historyRow{ID: rowID, SiteID: cw.site, OwnerUserID: cw.owner, KeyID: h.row.KeyID, SourceKind: "room", SourceID: id.NewShortId(), Title: fmt.Sprintf("Retained call %02d", i), Role: "host", Status: "ended", OrderKey: historyOrder(started, rowID), StartedAt: started, EndedAt: started.Add(30 * time.Second), ConfiguredInsights: &insights, ConfiguredResources: &resources, AgentHosted: &agent}
		if i >= 17 {
			row.ArchivedAt, row.PurgeAt = cw.clock.Now(), cw.clock.Now().Add(historyArchiveWindow)
		}
		require.NoError(t, cw.svc.store.createHistory(ctx, cw.owner, row, cw.clock.Now()))
	}
	// Construct after the shared world exists: its engine keeps the default cap.
	t.Setenv("MEMQL_MEMORY_ENGINE_MAX_RESULTS", "7")
	low, err := memql.New(cw.db)
	require.NoError(t, err)
	low.Logger = cw.eng.Logger
	require.NoError(t, low.Init(memorynodes.DefaultRegistry()))
	engine := &historyCappedEngine{Engine: cw.svc.store.engine, page: liveEngine{low}}
	store := *cw.svc.store
	store.engine = engine
	svc := allowanceReplica(t, cw)
	svc.store = &store
	t.Run("public_cursor_continues_both_lists", func(t *testing.T) {
		for _, archived := range []bool{false, true} {
			cursor := ""
			seen := map[string]bool{}
			for page := 0; page < 8; page++ {
				out, e := svc.History(ctx, host, callrules.HistoryRequest{Archive: archived, Period: 7, Limit: 20, Cursor: cursor})
				require.NoError(t, e)
				for _, call := range out.Calls {
					require.False(t, seen[call.ID])
					seen[call.ID] = true
				}
				cursor = out.NextCursor
				if cursor == "" {
					break
				}
			}
			require.Empty(t, cursor)
			want := 17
			if archived {
				want = 9
			}
			require.Len(t, seen, want)
		}
	})
	t.Run("configured_usage_is_complete_only_after_exhaustion", func(t *testing.T) {
		out, e := svc.historyOverview(ctx, h, 7)
		require.NoError(t, e)
		require.Equal(t, "complete", out.Usage.Status)
		require.EqualValues(t, 26, *out.HostedCalls)
		require.EqualValues(t, 13, *out.Usage.InsightsCalls)
		require.EqualValues(t, 9, *out.Usage.ResourcesCalls)
		require.EqualValues(t, 7, *out.Usage.AgentHostedCalls)
	})
	require.Contains(t, engine.sizes, 7, "the native engine actually applies the reduced ceiling")
	for _, size := range engine.sizes {
		require.LessOrEqual(t, size, 7)
	}
}
