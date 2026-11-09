package memql

import (
	"strings"
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
// A mandatory equality or membership on an indexed payload field narrows the
// enumeration to the ids that index finds (latestLookupKeys). A mandatory
// row.createdAt lower bound narrows it to the versions written since
// (latestScanLowerBounds).
func (e *MemQLEngine) latestScanKeys(expr ExpressionNode, timestamp *time.Time) *bun.SelectQuery {
	concept, terms, ids := latestScanTerms(expr)
	if concept == "" {
		return nil
	}
	idFilters := make([]compiledExpression, 0, len(ids))
	for _, id := range ids {
		filter, err := e.compileComparisonExpressionWithContext(id, concept)
		if err != nil {
			return nil
		}
		idFilters = append(idFilters, filter)
	}
	if keys := e.latestLookupKeys(concept, terms, idFilters, timestamp); keys != nil && !memorynodes.HasLiveness(concept) {
		return keys
	}
	keys := e.database().NewSelect().Model((*memorynodes.MemoryNode)(nil)).
		Column("id", "createdAt").DistinctOn("id").
		Where("concept = ?", concept).OrderExpr(`id ASC, "createdAt" DESC`)
	if memorynodes.HasLiveness(concept) {
		keys = keys.ModelTableExpr(memorynodes.ObservedRowsSQL + " AS mn")
	}
	for _, filter := range idFilters {
		keys = keys.Where(filter.sql, filter.args...)
	}
	for _, bound := range latestScanLowerBounds(expr) {
		keys = keys.Where(bound.sql, bound.args...)
	}
	if timestamp != nil {
		keys = keys.Where(`"createdAt" <= ?`, timestamp.UTC())
	}
	return keys
}

// latestScanLowerBounds compiles the row.createdAt lower bounds the latest-key
// enumeration narrows on.
//
// A WALK SINCE A WATERMARK COSTS WHAT WAS WRITTEN SINCE, NOT THE CONCEPT'S
// HISTORY (2026-11-08). Without it the enumeration collapses every version of
// the concept to its latest, and only then does the filter's createdAt bound
// drop the old ones -- after a payload probe per id. Measured for the email
// statistics walk (component/emailstats) on 100,000 versions of 50,000
// deliveries over 60 days, at the default planner settings and at
// production's: a steady-state page, since five minutes ago, read 101,620
// index entries and probed 51,591 payloads to keep 648 rows, 1,106 and 1,099
// ms, and every five-minute run paid that once per source. With the bound it
// sorted the 643 keys at or after it and probed 643 payloads: 15.8 and 16.3
// ms. A first run plans and costs what it did (1,132 and 1,137 ms):
// every version is at or after a bound four hundred days back, so the bound
// excludes nothing.
//
// THE TRIGGER, AND ONLY IT. A top-level AND conjunct `createdAt >= X` on the
// intrinsic, where X is a plan constant: an argument or a literal, which
// argument expansion has already folded to the instant's text, and which
// compileCreatedAtComparison reads exactly as it reads the same conjunct in
// the filter. Never a term under OR or NOT or inside another wrapper (it is not
// required there), never one against a field or an expression still to be
// evaluated, never a payload field named createdAt. Any other shape enumerates
// as it did, byte for byte, so no other query's plan changes; the lookup and
// the newest-first walk (latest_scan_lookup.go, latest_scan_walk.go) are not
// touched either.
//
// WHY IT IS EXACT. A row whose TRUE latest version satisfies createdAt >= X has
// that version among the versions at or after X, and no newer version can be
// below X; so collapsing only those versions gives the same ids at the same
// latest versions for every row the filter can return. An id with no version at
// or after X is dropped, and its latest version is one the filter rejects. The
// page, its order and its cursor are unchanged, and latestMatchingNodes still
// re-reads each id's true latest version.
//
// WHY ONLY THE LOWER BOUND. An upper bound is not exact: collapsing the
// versions before Y yields an older version of an id whose true latest is
// later, and the filter would then be judged on a version the read must not
// return. Making that exact needs a true-latest check of its own -- more engine
// than a walk since a watermark needs. `>` would be as exact as `>=`, and is
// left out to keep the change to the one shape that walk writes.
//
// THE PRODUCTION-PLAN CLASS (latest_scan_walk.go, "THE QUERY'S SHAPE IS WHAT
// MAKES THE PLANNER WALK"): a plan measured at the timescaledb-ha default of
// random_page_cost 1.1 over three chunks was not production's at 4 over ten.
// Two things keep this change out of that class.
//
//   - The bound is a constant on the hypertable's partitioning column, so
//     TimescaleDB excludes every chunk wholly before it while planning,
//     whatever the cost settings.
//   - The bound gives the planner one new choice, and both answers cost the
//     range, not the history. It can read the covering index
//     (memory_nodes_concept_id_created_at_desc_idx: concept, id, createdAt
//     DESC) in the order DISTINCT ON reads, or the retention index
//     (memory_nodes_retention_time_idx: concept, createdAt, id) from the
//     bound and sort what that range holds. Either way it reads only the
//     chunks left and no payload. The payload probe, which was the cost, runs
//     once per key the bound keeps.
//
// Measured at production's settings over ten one-day chunks, a bound half a
// day before the newest version planned the range and the sort: one chunk
// scanned, 230 index entries sorted, 48 payloads read, under a millisecond.
// The walk's steady state above planned the same way, and its first run kept
// the covering index's order with no sort.
//
// Tests:
//   - TestLatestScanLowerBoundReadsOnlyTheVersionsAfterIt pins the chunks,
//     the entries, the size of any sort and the payload reads at those
//     settings, not the index chosen.
//   - TestLatestScanKeysPushesOnlyATopLevelCreatedAtLowerBound pins the
//     trigger.
//   - The queryFilter cases of test/conformance/2026 hold the rows the bound
//     and the full collapse could disagree on, and the differential lane reads
//     them through this path.
func latestScanLowerBounds(expr ExpressionNode) []compiledExpression {
	var bounds []compiledExpression
	var visit func(ExpressionNode)
	visit = func(expr ExpressionNode) {
		switch n := expr.(type) {
		case *LogicalExpression:
			if n != nil && n.Op == LogicalAnd {
				visit(n.Left)
				visit(n.Right)
			}
		case *ComparisonExpression:
			if n == nil || n.Operator != OpGe || n.TextConcat != nil || len(n.Field.Parts) != 1 {
				return
			}
			if field, ok := resolveIntrinsicField(n.Field.Parts[0]); !ok || field.kind != intrinsicFieldCreatedAt {
				return
			}
			if _, constant := n.Value.(string); !constant {
				return
			}
			// The filter's own compilation of the conjunct, so the
			// enumeration and the filter compare one instant the same way.
			// Text that is no instant adds no bound, and the enumeration
			// reads as it did; what such a read does is the filter's to say.
			if bound, err := compileCreatedAtComparison(n.Operator, n.Value); err == nil {
				bounds = append(bounds, bound)
			}
		}
	}
	visit(expr)
	return bounds
}

// latestScanTerms reads the mandatory conjuncts a latest-key enumeration may
// narrow on: the concept equality, the payload lookup terms and the id
// comparisons. Only top-level AND conjuncts count (see latestScanKeys).
func latestScanTerms(expr ExpressionNode) (concept string, terms []payloadLookupTerm, ids []*ComparisonExpression) {
	var visit func(ExpressionNode)
	visit = func(expr ExpressionNode) {
		switch n := expr.(type) {
		case *LogicalExpression:
			if n != nil && n.Op == LogicalAnd {
				visit(n.Left)
				visit(n.Right)
			}
		case *ComparisonExpression:
			if n == nil {
				return
			}
			if term, ok := payloadLookupTermOf(n); ok {
				terms = append(terms, term)
				return
			}
			if len(n.Field.Parts) != 1 {
				return
			}
			field, ok := resolveIntrinsicField(n.Field.Parts[0])
			if !ok {
				return
			}
			if field.kind == intrinsicFieldConcept && n.Operator == OpEq {
				if value, ok := n.Value.(string); ok {
					// Match compileConceptComparisonIn's normalization.
					concept = strings.TrimSpace(value)
				}
			}
			if field.kind == intrinsicFieldId {
				ids = append(ids, n)
			}
		}
	}
	visit(expr)
	return concept, terms, ids
}
