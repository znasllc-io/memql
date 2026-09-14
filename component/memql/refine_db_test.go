package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/metrics"
)

// refine_db_test.go -- the refine clause over a REAL page (memql#5366): a query
// with an edition-2026 filter, sort and paginate, and a refine, compiled by the
// loader exactly as a tree construct is, executed through Execute against
// Postgres. What it pins is the construct's contract end to end: the filter
// pushes down, the SQL page is read, refine drops the rows its predicate
// refuses (so a page can come back SHORT), the cursor continues from the last
// row SQL returned (so paging never skips the rows a thinned page hid), and the
// drop is counted.

func TestRefine_RunsOverTheSQLPageAndKeepsTheCursor(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	ctx := context.Background()
	tag := "refine-" + uniqueSuffix("probe")
	base := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	// Eight rows, oldest first, sightingCount 0..7. Written RAW into the
	// tier-free concept so nothing fires on them (keysetConcept's property).
	for i := 0; i < 8; i++ {
		payload, err := json.Marshal(map[string]any{
			"kind": "tool", "capability": fmt.Sprintf("%s-%d", tag, i), "description": "refine probe", "sightingCount": i,
		})
		require.NoError(t, err)
		node := &memorynodes.MemoryNode{
			ID:         fmt.Sprintf("%s:%s-%d", irAgreementConcept, tag, i),
			Concept:    irAgreementConcept,
			CreatedBy:  "refine-probe",
			CreatedAt:  base.Add(time.Duration(i) * time.Second),
			Payload:    payload,
			Provenance: json.RawMessage(`{"kind":"direct","name":"refine-probe"}`),
		}
		_, err = db.NewInsert().Model(node).On(`CONFLICT (id, "createdAt") DO NOTHING`).Exec(ctx)
		require.NoError(t, err)
	}

	name := "refineProbe" + strings.ReplaceAll(uniqueSuffix("q"), "-", "")
	fn, err := compileAuthoredFunction(SandboxConstruct{Kind: "query", Name: name, Source: `use platform.concepts.{ missingCapability }

/// A page of four gaps by capability prefix, refined to drop one sighting count.
query missingCapability ` + name + ` {
  args {
    prefix  string!
    skip    int!
  }
  filter   row => row.capability startsWith args.prefix
  sort     "row.createdAt", "asc"
  paginate 4
  refine   row => row.sightingCount != args.skip && row.description.includes("probe")
}
`}, eng.concepts)
	require.NoError(t, err)
	require.NotNil(t, fn.V1Filter, "the filter lowered through Lower at load")
	require.NotNil(t, refineIn(fn.Expr))
	require.NoError(t, eng.functions.Upsert(fn))
	t.Cleanup(func() { eng.functions.Remove(name) })

	// Row 3 -- the LAST row of the first SQL page -- is the one refine drops.
	// That is what makes the cursor's anchor observable: a cursor minted from
	// the last row refine KEPT (row 2) would start the second page at row 3
	// and end it at row 6, and row 7 would be one page further away.
	call := fmt.Sprintf(`%s(prefix: %q, skip: 3)`, name, tag)
	reader := auth.ContextWithUserActor(ctx, "v1:identity:user:refine-probe")

	ids := func(res *ExecuteResult) []string {
		var out []string
		if res != nil && res.Bundle != nil {
			for _, n := range res.Bundle.Nodes {
				if n != nil {
					out = append(out, strings.TrimPrefix(n.Id, irAgreementConcept+":"))
				}
			}
		}
		return out
	}

	keptBefore := metrics.QueryRefineRowsValue(name, "kept")
	droppedBefore := metrics.QueryRefineRowsValue(name, "dropped")

	first, err := eng.Execute(reader, call)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{tag + "-0", tag + "-1", tag + "-2"}, ids(first),
		"the SQL page is rows 0..3 and refine drops row 3, so the page comes back SHORT")
	require.NotNil(t, first.Meta)
	require.True(t, first.Meta.HasMore, "the SQL page was full, so there is more -- whatever refine kept")
	require.NotEmpty(t, first.Meta.Cursor)
	require.Equal(t, float64(3), metrics.QueryRefineRowsValue(name, "kept")-keptBefore)
	require.Equal(t, float64(1), metrics.QueryRefineRowsValue(name, "dropped")-droppedBefore)

	second, err := eng.Execute(ContextWithCursor(reader, first.Meta.Cursor), call)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{tag + "-4", tag + "-5", tag + "-6", tag + "-7"}, ids(second),
		"the cursor continues after the last row SQL returned (row 3), not the last row refine kept (row 2)")
}
