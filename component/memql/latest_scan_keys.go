package memql

import (
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// staged-data: GATE -- keys are an internal subquery, never returned to a
// reader. executeCombinedFilterQuery applies the complete compiled predicate,
// including the injected staged-data gate, to the joined current rows before
// ordering/limiting, then retains the true-latest admission recheck.
//
// latestScanKeys enumerates current keys using the covering (concept, id,
// createdAt DESC) index. Payload predicates and authorization still run in SQL
// against those rows, before ordering/limiting, and latestMatchingNodes retains
// its true-latest recheck (including rows whose concept changed).
//
// Only a mandatory concept equality makes this bounded. Never extract a
// constraint from OR, NOT, relationships, or other expression wrappers. ID
// conjuncts are also safe to push down: an ID never changes across versions.
func (e *MemQLEngine) latestScanKeys(expr ExpressionNode, timestamp *time.Time) *bun.SelectQuery {
	var concept string
	var ids []*ComparisonExpression
	var visit func(ExpressionNode)
	visit = func(expr ExpressionNode) {
		switch n := expr.(type) {
		case *LogicalExpression:
			if n != nil && n.Op == LogicalAnd {
				visit(n.Left)
				visit(n.Right)
			}
		case *ComparisonExpression:
			if n == nil || len(n.Field.Parts) != 1 {
				return
			}
			field, ok := resolveIntrinsicField(n.Field.Parts[0])
			if !ok {
				return
			}
			if field.kind == intrinsicFieldConcept && n.Operator == OpEq {
				if value, ok := n.Value.(string); ok {
					concept = value
				}
			}
			if field.kind == intrinsicFieldId {
				ids = append(ids, n)
			}
		}
	}
	visit(expr)
	if concept == "" {
		return nil
	}
	keys := e.database().NewSelect().Model((*memorynodes.MemoryNode)(nil)).
		Column("id", "createdAt").DistinctOn("id").
		Where("concept = ?", concept).OrderExpr(`id ASC, "createdAt" DESC`)
	for _, id := range ids {
		filter, err := e.compileComparisonExpressionWithContext(id, concept)
		if err != nil {
			return nil
		}
		keys = keys.Where(filter.sql, filter.args...)
	}
	if timestamp != nil {
		keys = keys.Where(`"createdAt" <= ?`, timestamp.UTC())
	}
	return keys
}
