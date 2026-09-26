package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestLatestScanKeysOnlyPushesMandatoryIntrinsicConstraints(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	bound := irConceptEq(keysetConcept)
	for name, expr := range map[string]ExpressionNode{
		"disjunction":  &LogicalExpression{Op: LogicalOr, Left: bound, Right: irConceptEq(stagedDBConceptLive)},
		"negation":     irNot(bound),
		"relationship": &RelationshipExpression{Function: RelOwns, Target: bound},
	} {
		t.Run(name, func(t *testing.T) { require.Nil(t, eng.latestScanKeys(expr, nil)) })
	}
	expr := &LogicalExpression{Op: LogicalAnd, Left: bound, Right: irPayloadCmp("status", OpEq, "active")}
	keys := eng.latestScanKeys(expr, nil)
	require.NotNil(t, keys)
	require.NotContains(t, keys.String(), "payload", "history enumeration must not read historical JSON values")
	require.Contains(t, keys.String(), `"mn"."id", "mn"."createdAt"`)
	id := &ComparisonExpression{Field: FieldReference{Parts: []string{"id"}}, Operator: OpEq, Value: "one"}
	keys = eng.latestScanKeys(&LogicalExpression{Op: LogicalAnd, Left: bound, Right: id}, nil)
	require.Contains(t, keys.String(), keysetConcept+":one", "point reads must stay bounded to their resolved ID")
}

// A heartbeat history must not revive an old match or hide a newer match.
// Exercise the production scan AND its latest-version recheck, including asOf.
func TestLatestScanKeysPreservesPayloadFilteringAndHistory(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := clusterOwnerCtx("u-latest-keys")
	prefix := uniqueSuffix("latest-keys")
	owner := "latest:" + prefix
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var nodes []memorynodes.MemoryNode
	ids := []string{keysetConcept + ":" + prefix + "-active", keysetConcept + ":" + prefix + "-retired"}
	for n, id := range ids {
		for v := 0; v < 200; v++ {
			active := n == 1
			if v == 199 {
				active = !active
			}
			payload, err := json.Marshal(map[string]any{"available": active})
			require.NoError(t, err)
			nodes = append(nodes, memorynodes.MemoryNode{ID: id, Concept: keysetConcept, CreatedAt: base.Add(time.Duration(v) * time.Second), CreatedBy: owner, Payload: payload})
		}
	}
	_, err := db.NewInsert().Model(&nodes).Exec(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where(`"createdBy" = ?`, owner).Exec(context.Background())
	})
	query := fmt.Sprintf(`concept==%s && createdBy==%q && payload.available==true`, keysetConcept, owner)
	result, err := eng.Execute(ctx, query)
	require.NoError(t, err)
	require.Equal(t, []string{ids[0]}, pageIDs(t, result))
	cutoff := base.Add(198 * time.Second)
	plan, err := eng.parseWithFunctions(query, nil, nil, false)
	require.NoError(t, err)
	filter, ok := eng.tryCompileCombinedFilter(ctx, plan.Root, "")
	require.True(t, ok)
	historical, err := eng.executeCombinedFilterQuery(ctx, plan.Root, filter, &cutoff, 5000, nil)
	require.NoError(t, err)
	require.Len(t, historical, 1)
	require.Equal(t, ids[1], historical[0].ID)
	latest, err := eng.loadLatestNodes(ctx, ids, &cutoff)
	require.NoError(t, err)
	require.Len(t, latest, 2)
	require.True(t, latest[ids[0]].CreatedAt.Equal(cutoff))
}
