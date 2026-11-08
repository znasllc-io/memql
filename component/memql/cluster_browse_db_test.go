package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestClusterBrowsePagesSearchAndScopeDB(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("cluster-browse")}
	owner := f.person("owner", true)
	ctx := shareDirActor(context.Background(), owner, auth.RoleOwner)
	for n := 0; n < 25; n++ {
		text := f.prefix + " recent"
		if n < 12 {
			text = f.prefix + " old NEEDLE%_"
		}
		f.insert("v1:agents:agent", f.id("v1:agents:agent", fmt.Sprint(n)), map[string]any{"name": fmt.Sprintf("Agent %d", n), "description": text, "active": true, "systemPrompt": "large detail"})
		f.insert("v1:identity:auditEvent", f.id("v1:identity:auditEvent", fmt.Sprint(n)), map[string]any{"category": "security", "action": text, "outcome": "success", "actorUserId": owner})
		f.insert("v1:platform:outboxEntry", f.id("v1:platform:outboxEntry", fmt.Sprint(n)), map[string]any{"target": f.prefix, "status": "dead", "conceptId": "v1:test:item", "rowRef": text, "action": "insert"})
	}
	walk := func(call string, sizes []int) []map[string]any {
		t.Helper()
		cursor := ""
		all := []map[string]any{}
		for n, size := range sizes {
			page, err := eng.Execute(ContextWithCursor(ctx, cursor), call)
			require.NoError(t, err)
			rows := MaterializeRows(page.OutputPayload())
			require.Len(t, rows, size, call)
			all = append(all, rows...)
			cursor = page.GetMeta().Cursor
			if n == len(sizes)-1 {
				require.Empty(t, cursor)
			} else {
				require.NotEmpty(t, cursor)
			}
		}
		return all
	}
	walk(fmt.Sprintf(`query allAgents(search: %s)`, langparser.QuoteString(f.prefix)), []int{10, 10, 5})
	walk(fmt.Sprintf(`query allAgents(search: %s)`, langparser.QuoteString(f.prefix+" old needle%_")), []int{10, 2})
	walk(fmt.Sprintf(`query auditEventsBrowse(search: %s)`, langparser.QuoteString(f.prefix)), []int{10, 10, 5})
	walk(fmt.Sprintf(`query auditEventsBrowse(search: %s)`, langparser.QuoteString(f.prefix+" old needle%_")), []int{10, 2})
	walk(fmt.Sprintf(`query outboxDeadLettersBrowse(target: %s)`, langparser.QuoteString(f.prefix)), []int{10, 10, 5})
	walk(fmt.Sprintf(`query outboxDeadLettersBrowse(target: %s,search: "needle%%_")`, langparser.QuoteString(f.prefix)), []int{10, 2})
	admin := shareDirActor(context.Background(), owner, auth.RoleAdmin)
	for _, call := range []string{fmt.Sprintf(`query auditEventsBrowse(search: %s)`, langparser.QuoteString(f.prefix)), fmt.Sprintf(`query outboxDeadLettersBrowse(target: %s)`, langparser.QuoteString(f.prefix))} {
		result, err := eng.Execute(admin, call)
		require.NoError(t, err)
		require.Empty(t, MaterializeRows(result.OutputPayload()))
	}
}
