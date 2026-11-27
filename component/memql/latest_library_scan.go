package memql

import (
	"context"
	"strings"
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// staged-data: GATE -- the complete compiled read predicate, including staged
// data and row authorization, is applied before the limit. The ordinary
// latestMatchingNodes check still runs afterward.
//
// A Library browse used to hydrate every artifact before taking ten rows.
// Artifacts are a thin directory with few versions per ID: walk an owner's
// time range, reject superseded versions and apply ALL filters inside the
// page. Unlike latestScanWalk, a filter may reject a candidate here. The
// limit must follow that rejection, or a sparse folder would end early.
// Fine folder/run lookups retain their narrower path. Other concepts keep
// their existing plan; heartbeat histories must not use a payload-per-version
// walk. SQL-plan tests exercise production planner settings and many chunks.
func (e *MemQLEngine) latestLibraryScan(ctx context.Context, expr ExpressionNode, filter compiledExpression, timestamp *time.Time, sorter *compiledSort) func(int) *bun.SelectQuery {
	concept, terms, ids := latestScanTerms(expr, e.specBody)
	if concept != "v1:library:artifact" || len(ids) != 0 || len(lookupDrivers(concept, terms)) != 0 {
		return nil
	}
	owner := ""
	for _, term := range terms {
		if term.field == "ownerUserId" && len(term.values) == 1 {
			owner = term.values[0]
		}
	}
	if owner == "" {
		return nil
	}
	if sorter == nil || sorter.needsFullScan() || len(sorter.fields) < 2 ||
		sorter.fields[0].kind != sortFieldCreatedAt || sorter.fields[1].kind != sortFieldId ||
		sorter.fields[0].direction != sorter.fields[1].direction {
		return nil
	}
	return func(limit int) *bun.SelectQuery {
		q := e.database().NewSelect().Model((*memorynodes.MemoryNode)(nil)).
			Where("mn.concept = ? AND (mn.payload->>'ownerUserId') = ?", concept, owner).
			Where(filter.sql, filter.args...)
		newer := e.database().NewSelect().TableExpr(`"MemoryNodes" AS later`).ColumnExpr("1").
			Where(`later.concept = ? AND later.id = mn.id AND later."createdAt" > mn."createdAt"`, concept)
		if timestamp != nil {
			q = q.Where(`mn."createdAt" <= ?`, timestamp.UTC())
			newer = newer.Where(`later."createdAt" <= ?`, timestamp.UTC())
		}
		// OFFSET 0 keeps this an indexed subplan with execution-time chunk
		// exclusion, rather than an anti join visiting every chunk per row.
		q = q.Where("NOT EXISTS (? LIMIT 1 OFFSET 0)", newer)
		if position, ok := keysetFromContext(ctx); ok {
			if eligible, desc := keysetEligibleSort(sorter); eligible {
				predicate, args := keysetWhere(position, desc)
				if sorter != nil && sorter.payloadKeyset {
					predicate, args = payloadKeysetWhere(position, sorter)
				}
				q = q.Where(predicate, args...)
			}
		}
		// Both ordering columns are NOT NULL; their default NULL ordering
		// matches forward and backward scans of the same page index.
		for _, order := range combinedFilterOrderExprs(sorter) {
			q = q.OrderExpr(strings.ReplaceAll(order, ` NULLS LAST`, ``))
		}
		return q.Limit(int64(limit))
	}
}
