package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
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
		result, err := engine.Execute(ctx, fmt.Sprintf(`builtin conceptRowsSearch(conceptId: "v1:work:goal",search: "needle%%_",cursor: %s)`, langparser.QuoteString(cursor)))
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
	_, err = eng.Execute(ctx, fmt.Sprintf(`builtin conceptRowsSearch(conceptId: "v1:work:goal",search: "different",cursor: %s)`, langparser.QuoteString(token)))
	require.Error(t, err)
}

func TestConceptSearchFieldsExcludeSecretsAndNonText(t *testing.T) {
	concept := &memorynodes.Concept{Name: "v1:test:search", Schemas: map[string]json.RawMessage{"definition": json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"description":{"type":["string","null"]},"token":{"type":"string","x-secret":true},"count":{"type":"integer"},"body":{"type":"object"}}}`)}}
	fields, err := conceptSearchFields(concept)
	require.NoError(t, err)
	require.Equal(t, []string{"description", "name"}, fields)
}

// The inspector returns full raw rows, so its search must retain the generic
// browse PII boundary. A request-local binding is a planner optimization, not
// an authored projection that may disclose other users' contact fields.
func TestConceptRowsSearchPreservesGenericPIIBoundaryBeforePagingDB(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("concept-search-pii")}
	caller := f.person("caller", true)
	for n := 0; n < 25; n++ {
		f.person(fmt.Sprintf("foreign-%02d", n), true)
	}
	read := func(ctx context.Context, cursor string) ([]map[string]any, string) {
		t.Helper()
		result, err := eng.Execute(ctx, fmt.Sprintf(`builtin conceptRowsSearch(conceptId:"v1:identity:user",search:%s,cursor:%s)`, langparser.QuoteString(f.prefix), langparser.QuoteString(cursor)))
		require.NoError(t, err)
		envelope := MaterializeRows(result.OutputPayload())
		require.Len(t, envelope, 1)
		wire, err := json.Marshal(envelope[0])
		require.NoError(t, err)
		var page struct {
			Rows       []map[string]any `json:"rows"`
			NextCursor string           `json:"nextCursor"`
		}
		require.NoError(t, json.Unmarshal(wire, &page))
		return page.Rows, page.NextCursor
	}
	// Prime the same search as an operator; the narrower callers must never
	// reuse that answer. The operator still gets every row in bounded pages.
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleOwner} {
		ctx := shareDirActor(context.Background(), caller, role)
		cursor := ""
		seen := map[string]bool{}
		for _, size := range []int{10, 10, 6} {
			rows, next := read(ctx, cursor)
			require.Len(t, rows, size)
			for _, row := range rows {
				id := fmt.Sprint(row["id"])
				require.False(t, seen[id])
				seen[id] = true
			}
			cursor = next
		}
		require.Empty(t, cursor)
		require.Len(t, seen, 26)
	}
	// The caller's own row is older than more than a full page of foreign
	// matches. Filtering after LIMIT would silently hide it as exhaustion.
	for _, role := range []auth.Role{auth.RoleReader, auth.RoleWriter, auth.RoleDeveloper} {
		for _, identity := range []string{caller, BareShortId(caller)} {
			rows, cursor := read(shareDirActor(context.Background(), identity, role), "")
			require.Len(t, rows, 1, "role %s, caller %s", role, identity)
			require.Equal(t, caller, rows[0]["id"])
			payload := rows[0]["payload"].(map[string]any)
			require.Contains(t, payload["primaryEmail"], "caller-")
			require.Empty(t, cursor)
		}
	}
}
