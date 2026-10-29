package memql

import (
	"strings"
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// payloadLookupTerm is a required equality or membership term on one payload
// field. Every accepted value is a set string.
type payloadLookupTerm struct {
	field  string
	values []string
}

// payloadLookupTermOf reads a comparison as a lookup term when it can narrow
// a lookup (see latestLookupKeys for what is refused, and why).
func payloadLookupTermOf(n *ComparisonExpression) (payloadLookupTerm, bool) {
	if len(n.Field.Parts) != 2 || !strings.EqualFold(strings.TrimSpace(n.Field.Parts[0]), "payload") {
		return payloadLookupTerm{}, false
	}
	field := strings.TrimSpace(n.Field.Parts[1])
	if field == "" {
		return payloadLookupTerm{}, false
	}
	var members []any
	switch n.Operator {
	case OpEq:
		members = []any{n.Value}
	case OpIn:
		var hasUnset bool
		var err error
		members, hasUnset, err = splitUnsetMembers(n.Value)
		if err != nil || hasUnset {
			return payloadLookupTerm{}, false
		}
	default:
		return payloadLookupTerm{}, false
	}
	if len(members) == 0 {
		return payloadLookupTerm{}, false
	}
	values := make([]string, 0, len(members))
	for _, member := range members {
		value, ok := member.(string)
		if !ok || isUnsetValue(value) {
			return payloadLookupTerm{}, false
		}
		values = append(values, value)
	}
	return payloadLookupTerm{field: field, values: values}, true
}

// staged-data: GATE -- the lookup only narrows latestScanKeys' internal
// enumeration of keys. executeCombinedFilterQuery still applies the complete
// compiled predicate, the injected staged-data gate included, to every
// enumerated id's current row, and latestMatchingNodes still rechecks the true
// latest version.
//
// THE LOOKUP: ENUMERATE ONLY THE IDS A LOOKUP INDEX FINDS.
//
// latestScanKeys enumerates every id of the concept at its latest version and
// the filter runs on those rows afterwards, so a read that requires
// payload.emailDigest == D still reads every recipient's latest version to keep
// one (measured at 200,000 recipients: 804,053 buffers, about a second per
// read, hit or miss). When the filter REQUIRES a payload field to equal a set
// value or belong to a set-string list, and a lookup index serves that field
// for the concept (payloadLookups), the enumeration starts from the ids carrying
// one of the accepted values on some version, each taken at its latest version.
//
// IT RETURNS THE SAME ROWS, BY CONSTRUCTION. A row the read returns is an id
// whose latest version satisfies the whole filter, so that version carries
// an accepted value for every required term -- and it is a version of the id,
// so the lookup finds the id. Dropping ids the lookup does not find drops only
// rows the filter would have dropped: same rows, order, page, and cursor. The index
// holds every version, so it also finds ids by values they no longer carry;
// those are enumerated at their latest version and the filter rejects them
// there, as it did before. Nothing here returns a version -- only an id, read
// again at its latest -- so a stale version can never match.
//
// WHAT CANNOT NARROW THE LOOKUP:
//   - a term under OR or NOT, or inside any other wrapper: it is not required;
//   - != and exclusion: a lookup finds versions carrying an accepted value;
//   - an UNSET value ("" or nil): every version written before a field was set
//     lacks it, so the ids "lacking" a value are the concept's history -- after
//     a backfill, every id it had;
//   - anything but a string: the lookup compares the TEXT extraction, which
//     agrees with the typed comparison only for strings (5.0 is not the text of
//     5, so an id whose latest version matches could go unfound);
//   - a nested path: no lookup index leads with one.
//
// ONLY AN INDEXED TERM IS READ IN THE LOOKUP, AND ONLY OF THE FINEST GRAIN.
//
// The lookup visits every VERSION in its range, and a term in its WHERE clause
// that no index answers is read from each of those versions' payloads: a heap
// fetch and a TOAST read per version. So a term is ANDed into the lookup only
// when a lookup index serves it, and every other required term is decided by
// the filter on each found id's latest version -- once per id, not once per
// version. Without an index serving at least one term there is no lookup at
// all: reading every version's payload is the cost the latest-key enumeration
// exists to avoid (a heartbeat concept carries hundreds of thousands of
// versions of a few ids).
//
// This used to AND every required term, on the reasoning that each can only
// narrow. That is true of the ids and wrong about the cost. Production's
// cluster owner owns every pipeline run, so the owner index's range for them
// is the run history, and workRunsForGoal read the goalId of every historical
// version in it until the request pool cancelled it at eight seconds
// (2026-10-29, SQLSTATE 57014).
//
// When terms of both grains are served, only the fine ones are ANDed
// (payloadLookup.coarse). The planner cannot choose between two lookup
// indexes: PostgreSQL ignores a partial index's statistics when it estimates
// a payload equality, so both ranges cost the default selectivity and the tie
// goes to whichever index it considers first. Measured after ANALYZE: the
// owner index, with the goal term as a per-version filter. Leaving the coarse
// term to the filter leaves the planner one index to use. A coarse term alone
// still starts a lookup -- one person's runs among many system runs, the read
// its index was built for.
//
// TestWorkRunReadsDoNotScanTheOwnersHistory holds both rules to the real SQL.
//
// The latest version of each found id is a LATERAL probe of the latest-row
// index, so the plan is the matching lookup ranges plus one probe per id
// whatever the statistics say: the same fence the payload probe in
// executeCombinedFilterQuery relies on.
func (e *MemQLEngine) latestLookupKeys(concept string, terms []payloadLookupTerm, idFilters []compiledExpression, timestamp *time.Time) *bun.SelectQuery {
	drivers := lookupDrivers(concept, terms)
	if len(drivers) == 0 {
		return nil
	}
	db := e.database()
	lookup := db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).Column("id").Distinct().
		Where("concept = ?", concept)
	for _, term := range drivers {
		if len(term.values) == 1 {
			lookup = lookup.Where("(payload->>?) = ?", term.field, term.values[0])
		} else {
			lookup = lookup.Where("(payload->>?) IN (?)", term.field, bun.In(term.values))
		}
	}
	// An id never changes across versions, so an id term holds for every
	// version of a row exactly when it holds for the latest.
	for _, filter := range idFilters {
		lookup = lookup.Where(filter.sql, filter.args...)
	}
	latest := `CROSS JOIN LATERAL (SELECT id, "createdAt" FROM "MemoryNodes" WHERE concept = ? AND id = lookup.id`
	args := []any{concept}
	if timestamp != nil {
		lookup = lookup.Where(`"createdAt" <= ?`, timestamp.UTC())
		latest += ` AND "createdAt" <= ?`
		args = append(args, timestamp.UTC())
	}
	latest += ` ORDER BY "createdAt" DESC LIMIT 1) AS latest`
	return db.NewSelect().TableExpr("(?) AS lookup", lookup).
		ColumnExpr(`latest.id, latest."createdAt"`).
		Join(latest, args...)
}

// lookupDrivers is the required terms a lookup reads its range through: the
// ones a lookup index serves, the fine-grained in preference to the coarse
// (see latestLookupKeys). Empty when no index serves any of them.
func lookupDrivers(concept string, terms []payloadLookupTerm) []payloadLookupTerm {
	var fine, coarse []payloadLookupTerm
	for _, term := range terms {
		lookup, served := payloadLookupFor(concept, term.field)
		switch {
		case !served:
		case lookup.coarse:
			coarse = append(coarse, term)
		default:
			fine = append(fine, term)
		}
	}
	if len(fine) > 0 {
		return fine
	}
	return coarse
}
