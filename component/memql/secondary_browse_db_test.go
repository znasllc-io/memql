package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestSecondaryBrowsePagesAndSearchDB(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("secondary-browse")}
	owner := f.person("owner", true)
	other := f.person("other", true)
	ctx := shareDirActor(context.Background(), owner, auth.RoleOwner)
	for n := 0; n < 25; n++ {
		name := f.prefix + " recent"
		if n < 12 {
			name = f.prefix + " older NEEDLE%_"
		}
		f.insert("v1:identity:accessRequest", f.id("v1:identity:accessRequest", fmt.Sprint(n)), map[string]any{"name": name, "email": fmt.Sprintf("%d@test.invalid", n), "status": "pending"})
		f.insert("v1:workbench:workspace", f.id("v1:workbench:workspace", fmt.Sprint(n)), map[string]any{"ownerUserId": owner, "runId": fmt.Sprint(n), "nodeId": "workbench", "storageRoot": name, "status": "provisioned"})
	}
	f.insert("v1:workbench:workspace", f.id("v1:workbench:workspace", "foreign"), map[string]any{"ownerUserId": other, "storageRoot": f.prefix, "status": "provisioned"})
	f.insert("v1:workbench:workspace", f.id("v1:workbench:workspace", "released"), map[string]any{"ownerUserId": owner, "storageRoot": f.prefix, "status": "released"})
	walk := func(call string, sizes []int) {
		cursor := ""
		seen := map[string]bool{}
		for n, size := range sizes {
			result, err := eng.Execute(ContextWithCursor(ctx, cursor), call)
			require.NoError(t, err)
			rows := MaterializeRows(result.OutputPayload())
			require.Len(t, rows, size, call)
			for _, row := range rows {
				id := fmt.Sprint(row["id"])
				require.False(t, seen[id])
				seen[id] = true
			}
			cursor = result.GetMeta().Cursor
			if n == len(sizes)-1 {
				require.Empty(t, cursor)
			} else {
				require.NotEmpty(t, cursor)
			}
		}
	}
	walk(fmt.Sprintf(`query pendingAccessRequests(search:%s)`, langparser.QuoteString(f.prefix)), []int{10, 10, 5})
	walk(fmt.Sprintf(`query pendingAccessRequests(search:%s)`, langparser.QuoteString(f.prefix+" older needle%_")), []int{10, 2})
	walk(fmt.Sprintf(`query myWorkspaces(search:%s,includeReleased:false)`, langparser.QuoteString(f.prefix)), []int{10, 10, 5})
	walk(fmt.Sprintf(`query myWorkspaces(search:%s,includeReleased:false)`, langparser.QuoteString(f.prefix+" older needle%_")), []int{10, 2})
	walk(fmt.Sprintf(`query myWorkspaces(search:%s)`, langparser.QuoteString(f.prefix)), []int{10, 10, 6})
	reader := shareDirActor(context.Background(), owner, auth.RoleReader)
	_, err := eng.Execute(reader, `query pendingAccessRequests()`)
	require.Error(t, err)
}
