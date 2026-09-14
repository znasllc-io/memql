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
)

// authoring_lower_db_test.go -- a session-authored edition-2026 query runs,
// and runs as SQL (memql#5366). The query applies a spec defined beside it in
// the same session; both are lowered at define, and what executes is the
// lowered IR with the session spec inlined -- which the combined SQL compiler
// accepts whole, so the filter pushes down rather than running as an
// in-process post-filter.

func TestAuthoringLower_SessionQueryRunsAndPushesDown(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	ctx := context.Background()
	tag := "authored-" + uniqueSuffix("lower")
	base := time.Date(2026, 9, 14, 13, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		payload, err := json.Marshal(map[string]any{
			"kind": "tool", "capability": fmt.Sprintf("%s-%d", tag, i), "description": tag, "sightingCount": i,
		})
		require.NoError(t, err)
		node := &memorynodes.MemoryNode{
			ID:         fmt.Sprintf("%s:%s-%d", irAgreementConcept, tag, i),
			Concept:    irAgreementConcept,
			CreatedBy:  "authored-probe",
			CreatedAt:  base.Add(time.Duration(i) * time.Second),
			Payload:    payload,
			Provenance: json.RawMessage(`{"kind":"direct","name":"authored-probe"}`),
		}
		_, err = db.NewInsert().Model(node).On(`CONFLICT (id, "createdAt") DO NOTHING`).Exec(ctx)
		require.NoError(t, err)
	}

	suffix := strings.ReplaceAll(uniqueSuffix("q"), "-", "")
	specName, queryName := "seenTwice"+suffix, "sessionGaps"+suffix
	bundle := `use platform.concepts.{ missingCapability }

/// Gaps seen more than once.
spec missingCapability ` + specName + ` = row => row.sightingCount > 1

/// This probe's gaps, seen more than once.
query missingCapability ` + queryName + ` {
  filter   row => row.description == "` + tag + `" && ` + specName + `(row)
  sort     "row.createdAt", "asc"
  paginate 10
}
`
	owner := "v1:identity:user:authored-probe"
	reg := NewAuthoredRuntimeRegistry()
	{
		res, err := eng.DefineSessionBundle(reg, owner, bundle, "")
		require.NoError(t, err, "%+v", res.Diagnostics)
	}

	// It pushes down: the query's lowered filter, with the session spec
	// inlined as execution inlines it, compiles to SQL whole.
	c, ok := reg.Lookup(owner, "query", queryName)
	require.True(t, ok)
	fn := c.Compiled.(*Function)
	expanded, err := eng.resolveAuthoredSpecOverlay(unwrapToFilter(fn.Expr), eng.buildAuthoredSpecOverlay(owner, nil, reg))
	require.NoError(t, err)
	compiled, pushed := eng.tryCompileCombinedFilter(ctx, expanded, irAgreementConcept)
	require.True(t, pushed, "the lowered filter, spec inlined, compiles to SQL: %s", canonicalExpression(expanded))
	require.Contains(t, compiled.sql, "sightingCount", "the session spec's condition is in the SQL")

	// It runs, returning exactly the rows the spec selects.
	res, err := eng.ExecuteAuthored(auth.ContextWithUserActor(ctx, owner), queryName+"()", owner, reg)
	require.NoError(t, err)
	var got []string
	if res != nil && res.Bundle != nil {
		for _, n := range res.Bundle.Nodes {
			if n != nil {
				got = append(got, strings.TrimPrefix(n.Id, irAgreementConcept+":"))
			}
		}
	}
	require.Equal(t, []string{tag + "-2", tag + "-3", tag + "-4"}, got)
}
