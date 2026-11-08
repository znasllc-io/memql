package memql

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// Verify the authored DSL against storage, including the composite owner tier.
// A ten-record directory still carries provenance needed to explain each row;
// private source snapshots are stored separately and never join this read.
func TestComposeBrowsePagesSearchArchiveAndOwnerScopeDB(t *testing.T) {
	engine, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: engine, db: db, prefix: uniqueSuffix("compose-browse")}
	owner, stranger := f.person("owner", true), f.person("stranger", true)
	caller := rankActorCtx(owner, auth.RoleWriter)
	for _, resource := range []struct{ concept, query, detail, argument string }{
		{"composition", "compositionsBrowse", "compositionById", "compositionId"},
		{"composeTemplate", "composeTemplatesBrowse", "composeTemplateById", "templateId"},
		{"recipe", "composeRecipesBrowse", "composeRecipeById", "recipeId"},
	} {
		t.Run(resource.query, func(t *testing.T) {
			concept := "v1:compose:" + resource.concept
			for n := 0; n < 25; n++ {
				name := fmt.Sprintf("%s Recent %02d", f.prefix, n)
				if n < 12 {
					name = fmt.Sprintf("%s NEEDLE%%_ %02d", f.prefix, n)
				}
				f.insert(concept, f.id(concept, fmt.Sprintf("item-%02d", n)), map[string]any{
					"ownerUserId": owner, "name": name, "statement": "Make the report", "description": "Useful description", "format": "pdf", "status": "ready", "archived": false,
					"sources":    []map[string]any{{"kind": "concept_row", "ref": "invoice-1", "label": "INV-1"}},
					"modelsUsed": []map[string]any{{"provider": "test", "model": "test-model", "calls": 1}},
					"request":    strings.Repeat("private source snapshot", 1024),
				})
			}
			foreign := f.id(concept, "foreign")
			f.insert(concept, foreign, map[string]any{"ownerUserId": stranger, "name": f.prefix + " foreign", "archived": false})
			f.insert(concept, f.id(concept, "archived"), map[string]any{"ownerUserId": owner, "name": f.prefix + " archived", "archived": true, "format": "pdf"})
			walk := func(search string, includeArchived bool, sizes []int) {
				cursor := ""
				seen := map[string]bool{}
				call := fmt.Sprintf("query %s(search: %q, includeArchived: %t)", resource.query, search, includeArchived)
				for page, size := range sizes {
					result, err := engine.Execute(ContextWithCursor(caller, cursor), call)
					require.NoError(t, err)
					rows := MaterializeRows(result.OutputPayload())
					require.Len(t, rows, size)
					payload, err := json.Marshal(rows)
					require.NoError(t, err)
					require.Less(t, len(payload), 16*1024)
					for _, row := range rows {
						id := fmt.Sprint(row["id"])
						require.False(t, seen[id])
						seen[id] = true
						require.Equal(t, BareShortId(owner), BareShortId(fmt.Sprint(row["ownerUserId"])))
						require.NotContains(t, row, "request")
						if !includeArchived {
							require.NotEqual(t, true, row["archived"])
						}
						if resource.concept == "composition" && row["archived"] != true {
							require.NotEmpty(t, row["sources"])
							require.NotEmpty(t, row["modelsUsed"])
						}
					}
					cursor = result.GetMeta().Cursor
					if page < len(sizes)-1 {
						require.NotEmpty(t, cursor)
					} else {
						require.Empty(t, cursor)
					}
				}
			}
			walk(f.prefix, false, []int{10, 10, 5})
			walk(f.prefix+" needle%_", false, []int{10, 2})
			walk(f.prefix, true, []int{10, 10, 6})
			call := fmt.Sprintf("query %s(%s: %q)", resource.detail, resource.argument, BareShortId(foreign))
			denied, err := engine.Execute(caller, call)
			require.NoError(t, err)
			require.Empty(t, MaterializeRows(denied.OutputPayload()))
			admitted, err := engine.Execute(rankActorCtx(owner, auth.RoleOwner), call)
			require.NoError(t, err)
			require.Len(t, MaterializeRows(admitted.OutputPayload()), 1)
		})
	}
}
