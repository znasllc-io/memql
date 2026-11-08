package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

func TestRoutingHistoryPagesSearchAndRankDB(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("routing-browse")}
	user := f.person("operator", false)
	ctx := shareDirActor(context.Background(), user, auth.RoleAdmin)
	for n := 0; n < 25; n++ {
		model, prompt := "recent", "agentReply"
		if n < 12 {
			model, prompt = "old NEEDLE%_", "composeRoutingPolicy"
		}
		f.insert("v1:router:call", f.id("v1:router:call", fmt.Sprint(n)), map[string]any{
			"rule": f.prefix, "model": model, "promptName": prompt, "level": "strong", "door": "local", "outcome": "ok",
			"prompt": strings.Repeat("private prompt", 1000), "errorMessage": "private error",
		})
	}
	walk := func(args string, sizes []int) {
		t.Helper()
		cursor, seen := "", map[string]bool{}
		for n, size := range sizes {
			result, err := eng.Execute(ContextWithCursor(ctx, cursor), fmt.Sprintf(`query routerDecisionsRecent(rule: %q%s)`, f.prefix, args))
			require.NoError(t, err)
			rows := MaterializeRows(result.OutputPayload())
			require.Len(t, rows, size)
			for _, row := range rows {
				id := fmt.Sprint(row["id"])
				require.False(t, seen[id])
				seen[id] = true
				require.NotContains(t, row, "prompt")
				require.NotContains(t, row, "errorMessage")
			}
			payload, err := json.Marshal(rows)
			require.NoError(t, err)
			require.Less(t, len(payload), 16_000)
			cursor = result.GetMeta().Cursor
			if n == len(sizes)-1 {
				require.Empty(t, cursor)
			} else {
				require.NotEmpty(t, cursor)
			}
		}
	}
	walk("", []int{10, 10, 5})
	walk(`, search: "needle%_"`, []int{10, 2})
	walk(`, search: "Route composing", promptNames: ["composeRoutingPolicy"]`, []int{10, 2})
	walk(`, search: "compose routing", compactSearch: "composerouting"`, []int{10, 2})
	_, err := eng.Execute(shareDirActor(context.Background(), user, auth.RoleWriter), `query routerDecisionsRecent()`)
	require.Error(t, err, "the existing administrator floor must remain enforced")
}
