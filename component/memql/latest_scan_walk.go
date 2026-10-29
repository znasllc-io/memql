package memql

import (
	"context"
	"strings"
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// staged-data: GATE -- the walk is an internal subquery, never returned to a
// reader. executeCombinedFilterQuery applies the complete compiled predicate,
// the injected staged-data gate included, to the rows it yields before the
// page is taken, and latestMatchingNodes still rechecks the true latest version.
//
// THE NEWEST-FIRST WALK: AN OWNER'S FEED COSTS ITS PAGE, NOT ITS HISTORY
// (2026-10-29).
//
// A coarse lookup term (payloadLookup.coarse: the run owner) can cover a
// concept's whole history: production's cluster owner owns every pipeline
// run, and every step boundary writes the run again. latestLookupKeys reads
// every version in that range to learn the ids, then every id's latest
// payload, before the sort takes the page. For a read that wants the NEWEST
// rows the range has a better order. latestScanWalk walks the term's versions
// newest first through a walk index (payloadWalks). It keeps a version only
// when no newer version of its id exists, and stops when the page is full.
// That costs the versions written since the page's oldest row -- each run's
// versions, for the runs on the page -- whatever the history behind them.
// Measured on 3,000 runs of 20 versions: 4,152 buffers for a page of 50,
// where the lookup took 97,252.
//
// The newer-version probe is a LATERAL subquery with LIMIT 1 rather than NOT
// EXISTS. TimescaleDB excludes chunks for the LATERAL form at run time, and
// it probed every chunk's index for the NOT EXISTS form (10,145 buffers for
// the same page). The filter runs above an OFFSET 0 fence. Below it, the
// planner would evaluate the payload filter on every version the walk
// passes, a TOAST read each, instead of on the latest ones it keeps.
//
// WHEN IT IS TAKEN, AND WHY SO NARROWLY. Only when the term decides the read:
// with the term and the concept assumed, the filter is true for every row
// (decidedByTerm; actor terms folded to their constants, and anything else
// that reads a row disqualifies it). A filter that can reject the latest
// versions the walk finds -- a status, a goal signature -- may need the whole
// history to fill a page, and the walk pays a probe per version where the
// lookup pays a heap fetch: measured at 4x the lookup's buffers when nothing
// matched. Such a read is a selective one, and its bound is a lookup index on
// what it selects (payloadLookups). The walk is also taken only in the walk
// index's own order, newest first (createdAt DESC, id ASC). Any other order
// is sorted after the read and gains nothing.
//
// IT RETURNS THE SAME ROWS. The walk yields each id at its latest version, and
// only when that version carries the term: exactly the ids whose latest
// version satisfies it, which includes every row the read can return. The
// page, its order and its cursor are unchanged. The keyset is pushed into the
// walk as `createdAt <= t AND (createdAt < t OR id > i)`, a form the index can
// start from. The engine's own spelling, a top-level OR, sent the planner to a
// bitmap scan and a sort of the whole range (measured: 127,725 buffers for a
// deep page; 4,431 in this form).
//
// TestWorkRunReadsDoNotScanTheOwnersHistory pins the pages, their cursors and
// their cost on the real SQL; TestLatestScanWalkTakesOnlyReadsItsTermDecides
// pins when the walk is taken.
func (e *MemQLEngine) latestScanWalk(ctx context.Context, expr ExpressionNode, timestamp *time.Time, sorter *compiledSort) *bun.RawQuery {
	if !newestFirst(sorter) {
		return nil
	}
	concept, terms, ids := latestScanTerms(expr)
	if concept == "" || len(ids) > 0 || memorynodes.HasLiveness(concept) {
		return nil
	}
	drivers := lookupDrivers(concept, terms)
	if len(drivers) != 1 {
		return nil
	}
	driver := drivers[0]
	if !driver.lookup.coarse || len(driver.term.values) != 1 || !payloadWalkFor(concept, driver.term.field) {
		return nil
	}
	folded, err := resolveActorComparisonsToConstants(ctx, expr)
	if err != nil || !decidedByTerm(folded, concept, driver.term) {
		return nil
	}

	newer := ""
	where := ""
	args := []any{concept}
	if timestamp != nil {
		newer = ` AND later."createdAt" <= ?`
		args = append(args, timestamp.UTC())
	}
	args = append(args, concept, driver.term.field, driver.term.values[0])
	if timestamp != nil {
		where += ` AND w."createdAt" <= ?`
		args = append(args, timestamp.UTC())
	}
	if keyset, ok := keysetFromContext(ctx); ok {
		where += ` AND w."createdAt" <= ? AND (w."createdAt" < ? OR w.id > ?)`
		args = append(args, keyset.createdAt, keyset.createdAt, keyset.id)
	}
	return e.database().NewRaw(`SELECT w.* FROM "MemoryNodes" AS w
LEFT JOIN LATERAL (SELECT 1 AS found FROM "MemoryNodes" AS later
  WHERE later.concept = ? AND later.id = w.id AND later."createdAt" > w."createdAt"`+newer+` LIMIT 1) AS latest_walk ON true
WHERE w.concept = ? AND (w.payload->>?) = ? AND latest_walk.found IS NULL`+where+`
ORDER BY w."createdAt" DESC, w.id ASC
OFFSET 0`, args...)
}

// newestFirst reports whether sorter pages newest first and nothing else:
// createdAt DESC with the id ASC tie-break, the walk index's own order.
func newestFirst(sorter *compiledSort) bool {
	if sorter == nil {
		return true // the default order: createdAt DESC, id ASC
	}
	if sorter.needsFullScan() || len(sorter.fields) == 0 || sorter.fields[0].kind != sortFieldCreatedAt {
		return false
	}
	for _, field := range sorter.fields {
		switch {
		case field.kind == sortFieldCreatedAt && field.direction != SortDirectionAsc:
		case field.kind == sortFieldId && field.direction == SortDirectionAsc:
		default:
			return false
		}
	}
	return true
}

// decidedByTerm reports whether node, with every actor term already folded to
// its constant, is true for EVERY row of concept that satisfies term: the
// walk's admission rule. It answers false for anything it cannot decide,
// which keeps the read on the lookup.
func decidedByTerm(node ExpressionNode, concept string, term payloadLookupTerm) bool {
	switch n := node.(type) {
	case *constantBoolExpression:
		return n != nil && n.value
	case *LogicalExpression:
		if n == nil {
			return false
		}
		switch n.Op {
		case LogicalAnd:
			return decidedByTerm(n.Left, concept, term) && decidedByTerm(n.Right, concept, term)
		case LogicalOr:
			return decidedByTerm(n.Left, concept, term) || decidedByTerm(n.Right, concept, term)
		}
	case *ComparisonExpression:
		if n == nil {
			return false
		}
		if found, ok := payloadLookupTermOf(n); ok {
			return found.field == term.field && len(found.values) == 1 && found.values[0] == term.values[0]
		}
		if len(n.Field.Parts) != 1 {
			return false
		}
		field, ok := resolveIntrinsicField(n.Field.Parts[0])
		value, isString := n.Value.(string)
		if !ok || field.kind != intrinsicFieldConcept || !isString {
			return false
		}
		switch n.Operator {
		case OpEq:
			return strings.TrimSpace(value) == concept
		case OpNe:
			// The staged-data gate's `concept != <staged>`.
			return strings.TrimSpace(value) != concept
		}
	}
	return false
}
