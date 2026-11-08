package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// SQL must use the same comparator as the in-process sort, including absent
// fields, blank Unicode whitespace, case, accents, ties and both directions.
// Historical values must never resurrect after a row's current value changes.
func TestPayloadKeysetAgreesWithInProcessOrderAndLatestVersions(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("payload-cursor")}
	owner := f.person("owner", true)
	ctx := shareDirActor(context.Background(), owner, auth.RoleOwner)
	const conceptID = "v1:pipelines:pipeline"
	concept, err := eng.concepts.Get(conceptID)
	require.NoError(t, err)
	values := []any{"zeta", "alpha", "ALPHA", "Éclair", "éclair", "alpha", nil, "", "\t\u2003\n", " quote'_% ", "Ωmega"}
	for i, value := range values {
		f.insert(conceptID, f.id(conceptID, fmt.Sprintf("row-%02d", i)), map[string]any{"ownerUserId": owner, "name": value})
	}
	// A current row no longer owned by this caller is absent, even though a
	// historical matching version exists. The sort position of an updated
	// row comes from its current value, not its earlier value.
	f.insert(conceptID, f.id(conceptID, "moved"), map[string]any{"ownerUserId": owner, "name": "before"})
	f.insert(conceptID, f.id(conceptID, "moved"), map[string]any{"ownerUserId": "other", "name": "after"})
	f.insert(conceptID, f.id(conceptID, "updated"), map[string]any{"ownerUserId": owner, "name": "before"})
	f.insert(conceptID, f.id(conceptID, "updated"), map[string]any{"ownerUserId": owner, "name": "updated"})
	expr := &LogicalExpression{Op: LogicalAnd, Left: fieldCmp("concept", OpEq, conceptID), Right: fieldCmp("payload.ownerUserId", OpEq, owner)}
	for _, direction := range []SortDirection{SortDirectionAsc, SortDirectionDesc} {
		t.Run(string(direction), func(t *testing.T) {
			spec := []SortField{{Field: "name", Direction: direction}, {Field: "row.id", Direction: SortDirectionDesc}}
			fallback, err := compileSortFields(spec)
			require.NoError(t, err)
			want, err := eng.evaluateExpression(ctx, expr, nil, 100, fallback)
			require.NoError(t, err)
			require.Len(t, want, len(values)+1)
			sorter, err := compileSortFields(spec)
			require.NoError(t, err)
			require.NoError(t, sorter.enablePayloadKeyset(concept))
			require.False(t, sorter.needsFullScan())
			var got []memorynodes.MemoryNode
			pageCtx := ctx
			for page := 0; page < 10; page++ {
				rows, err := eng.evaluateExpression(pageCtx, expr, nil, 3, sorter)
				require.NoError(t, err)
				got = append(got, rows...)
				if len(rows) < 3 {
					break
				}
				token, err := encodePageCursor(rows[len(rows)-1], sorter)
				require.NoError(t, err)
				pos, err := decodePageCursor(token, sorter)
				require.NoError(t, err)
				pageCtx = contextWithKeyset(ctx, pos)
			}
			ids := func(rows []memorynodes.MemoryNode) []string {
				out := make([]string, len(rows))
				for i, row := range rows {
					out[i] = row.ID
				}
				return out
			}
			require.Equal(t, ids(want), ids(got))
		})
	}
}

func TestPayloadKeysetRejectsInvalidCursorAndUnsupportedTypes(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	concept, err := eng.concepts.Get("v1:pipelines:pipeline")
	require.NoError(t, err)
	sorter, err := compileSortFields([]SortField{{Field: "name", Direction: SortDirectionAsc}})
	require.NoError(t, err)
	require.NoError(t, sorter.enablePayloadKeyset(concept))
	require.True(t, sorter.payloadKeyset)
	row := memorynodes.MemoryNode{ID: "v1:pipelines:pipeline:cursor", Payload: json.RawMessage(`{"name":"name"}`)}
	token, err := encodePageCursor(row, sorter)
	require.NoError(t, err)
	_, err = decodePageCursor(token, sorter)
	require.NoError(t, err)
	opposite, err := compileSortFields([]SortField{{Field: "name", Direction: SortDirectionDesc}})
	require.NoError(t, err)
	require.NoError(t, opposite.enablePayloadKeyset(concept))
	_, err = decodePageCursor(token, opposite)
	require.ErrorIs(t, err, ErrCursorSortMismatch)
	for _, invalid := range []string{"", "not-base64!", "e30", strings.Repeat("a", 17)} {
		_, err = decodePageCursor(invalid, sorter)
		require.ErrorIs(t, err, ErrCursorMalformed)
	}
	for _, field := range []string{"timings", "unknown", "secretNames"} {
		fallback, err := compileSortFields([]SortField{{Field: field}})
		require.NoError(t, err)
		require.NoError(t, fallback.enablePayloadKeyset(concept))
		require.True(t, fallback.needsFullScan(), field)
		require.False(t, fallback.payloadKeyset, field)
	}
}

// Explicit intrinsic tie directions are authored ordering too. The legacy
// (createdAt,id ASC) cursor must not be reused for id DESC or id-first pages.
func TestIntrinsicValueKeysetHonorsAuthoredIDOrder(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := clusterOwnerCtx("intrinsic-order-owner")
	owner := uniqueSuffix("intrinsic-order")
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var ids []string
	for i := 0; i < 13; i++ {
		id := fmt.Sprintf("%s:%s-%02d", keysetConcept, owner, i)
		seedKeysetRow(t, ctx, db, id, at, owner)
		ids = append(ids, id)
	}
	for _, clause := range []string{`"createdAt", "desc", "id", "desc"`, `"id", "desc"`, `"id", "asc"`} {
		call := fmt.Sprintf(`sort(paginate(concept==%s&&createdBy==%q,5),%s)`, keysetConcept, owner, clause)
		var got []string
		cursor := ""
		for page := 0; page < 3; page++ {
			result, err := eng.Execute(ContextWithCursor(ctx, cursor), call)
			require.NoError(t, err)
			got = append(got, pageIDs(t, result)...)
			cursor = result.GetMeta().Cursor
			if page < 2 {
				require.NotEmpty(t, cursor)
			} else {
				require.Empty(t, cursor)
			}
		}
		want := append([]string(nil), ids...)
		if !strings.HasSuffix(clause, `"asc"`) {
			slices.Reverse(want)
		}
		require.Equal(t, want, got, clause)
	}
}
