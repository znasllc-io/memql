package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

func TestAskBrowsePagesSearchAndOwnerDB(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("ask-browse")}
	owner := f.person("owner", true)
	other := f.person("other", true)
	ctx := shareDirActor(context.Background(), owner, auth.RoleOwner)
	for n := 0; n < 25; n++ {
		title := f.prefix + " recent"
		if n < 12 {
			title = f.prefix + " old NEEDLE%_"
		}
		f.insert("v1:os:askConversation", f.id("v1:os:askConversation", fmt.Sprint(n)), map[string]any{"ownerUserId": owner, "title": title, "transcript": map[string]any{"turns": []any{map[string]any{"prompt": "private transcript body"}}}})
	}
	f.insert("v1:os:askConversation", f.id("v1:os:askConversation", "foreign"), map[string]any{"ownerUserId": other, "title": f.prefix + " old NEEDLE%_", "transcript": map[string]any{}})
	walk := func(search string, sizes []int) {
		t.Helper()
		cursor := ""
		ids := map[string]bool{}
		for n, size := range sizes {
			result, err := eng.Execute(ContextWithCursor(ctx, cursor), fmt.Sprintf(`query myAskConversationsBrowse(search: %q)`, search))
			require.NoError(t, err)
			rows := MaterializeRows(result.OutputPayload())
			require.Len(t, rows, size)
			for _, row := range rows {
				id := fmt.Sprint(row["id"])
				require.False(t, ids[id])
				ids[id] = true
				require.NotContains(t, row, "transcript")
			}
			cursor = result.GetMeta().Cursor
			if n == len(sizes)-1 {
				require.Empty(t, cursor)
			} else {
				require.NotEmpty(t, cursor)
			}
		}
	}
	walk(f.prefix, []int{10, 10, 5})
	walk(f.prefix+" old needle%_", []int{10, 2})
	result, err := eng.Execute(ctx, `query myAskConversationsBrowse()`)
	require.NoError(t, err)
	payload, err := json.Marshal(MaterializeRows(result.OutputPayload()))
	require.NoError(t, err)
	before, err := eng.Execute(ctx, `query myAskConversations()`)
	require.NoError(t, err)
	baseline, err := json.Marshal(MaterializeRows(before.OutputPayload()))
	require.NoError(t, err)
	require.Len(t, MaterializeRows(before.OutputPayload()), 25)
	t.Logf("Ask same-fixture first load: before %d summaries/%d bytes; after %d summaries/%d bytes; transcript omitted", len(MaterializeRows(before.OutputPayload())), len(baseline), len(MaterializeRows(result.OutputPayload())), len(payload))
}
