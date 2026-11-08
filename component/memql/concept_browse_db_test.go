package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestConceptRowsSearchAuthorizedCursorDB(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	other, err := New(db)
	require.NoError(t, err)
	require.NoError(t, bootPrivateEngine(other, memorynodes.DefaultRegistry()))
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("concept-search")}
	owner := f.person("owner", true)
	stranger := f.person("stranger", true)
	ctx := shareDirActor(context.Background(), owner, auth.RoleReader)
	for n := 0; n < 25; n++ {
		f.insert("v1:work:goal", f.id("v1:work:goal", fmt.Sprintf("item-%02d", n)), map[string]any{"ownerUserId": owner, "statement": fmt.Sprintf("Generic NEEDLE%%_ %02d", n), "status": "open"})
	}
	f.insert("v1:work:goal", f.id("v1:work:goal", "foreign"), map[string]any{"ownerUserId": stranger, "statement": "Generic NEEDLE%_ foreign", "status": "open"})
	cursor := ""
	seen := map[string]bool{}
	for page, size := range []int{10, 10, 5} {
		engine := []*MemQLEngine{eng, other}[page%2]
		result, err := engine.Execute(ctx, fmt.Sprintf(`builtin conceptRowsSearch(conceptId: "v1:work:goal",search: "needle%%_",cursor: %q)`, cursor))
		require.NoError(t, err)
		envelope := MaterializeRows(result.OutputPayload())
		require.Len(t, envelope, 1)
		wire, err := json.Marshal(envelope[0])
		require.NoError(t, err)
		var response struct {
			Rows       []map[string]any `json:"rows"`
			NextCursor string           `json:"nextCursor"`
		}
		require.NoError(t, json.Unmarshal(wire, &response))
		require.Len(t, response.Rows, size)
		for _, row := range response.Rows {
			id := fmt.Sprint(row["id"])
			require.False(t, seen[id])
			seen[id] = true
			payload, ok := row["payload"].(map[string]any)
			require.True(t, ok, "raw payload must survive")
			require.Equal(t, owner, payload["ownerUserId"])
		}
		cursor = response.NextCursor
		if page < 2 {
			require.NotEmpty(t, cursor)
		} else {
			require.Empty(t, cursor)
		}
	}
	require.Len(t, seen, 25)
	_, err = eng.Execute(ctx, `builtin conceptRowsSearch(conceptId: "v1:missing:thing",search: "x")`)
	require.Error(t, err)
	_, err = eng.Execute(context.Background(), `builtin conceptRowsSearch(conceptId: "v1:work:goal",search: "needle")`)
	require.Error(t, err)
	// A changed search cannot continue another question's cursor.
	first, err := eng.Execute(ctx, `builtin conceptRowsSearch(conceptId: "v1:work:goal",search: "needle%_")`)
	require.NoError(t, err)
	token := fmt.Sprint(MaterializeRows(first.OutputPayload())[0]["nextCursor"])
	_, err = eng.Execute(ctx, fmt.Sprintf(`builtin conceptRowsSearch(conceptId: "v1:work:goal",search: "different",cursor: %q)`, token))
	require.Error(t, err)
}

func TestConceptSearchFieldsExcludeSecretsAndNonText(t *testing.T) {
	concept := &memorynodes.Concept{Name: "v1:test:search", Schemas: map[string]json.RawMessage{"definition": json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"description":{"type":["string","null"]},"token":{"type":"string","x-secret":true},"count":{"type":"integer"},"body":{"type":"object"}}}`)}}
	fields, err := conceptSearchFields(concept)
	require.NoError(t, err)
	require.Equal(t, []string{"description", "name"}, fields)
}
