package memql

import (
	"strings"
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// payloadEquality is a required `payload.<field> == "<value>"` term: one
// field, a set string value.
type payloadEquality struct {
	field, value string
}

// payloadEqualityOf reads a comparison as a payloadEquality when it can narrow
// a lookup (see latestLookupKeys for what is refused, and why).
func payloadEqualityOf(n *ComparisonExpression) (payloadEquality, bool) {
	if n.Operator != OpEq || len(n.Field.Parts) != 2 || !strings.EqualFold(strings.TrimSpace(n.Field.Parts[0]), "payload") {
		return payloadEquality{}, false
	}
	value, ok := n.Value.(string)
	field := strings.TrimSpace(n.Field.Parts[1])
	if !ok || isUnsetValue(value) || field == "" {
		return payloadEquality{}, false
	}
	return payloadEquality{field: field, value: value}, true
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
// value and a lookup index serves that field for the concept (payloadLookups),
// the enumeration starts instead from the ids carrying the value on some
// version, each taken at its latest version.
//
// IT RETURNS THE SAME ROWS, BY CONSTRUCTION. A row the read returns is an id
// whose latest version satisfies the whole filter, so that version carries
// every required value -- and it is a version of the id, so the lookup finds
// the id. Dropping the ids the lookup does not find drops only rows the filter
// would have dropped: same rows, same order, same page, same cursor. The index
// holds every version, so it also finds ids by values they no longer carry;
// those are enumerated at their latest version and the filter rejects them
// there, as it did before. Nothing here returns a version -- only an id, read
// again at its latest -- so a stale version can never match.
//
// WHAT CANNOT NARROW THE LOOKUP:
//   - a term under OR or NOT, or inside any other wrapper: it is not required;
//   - != and membership: a lookup finds versions carrying a value;
//   - an UNSET value ("" or nil): every version written before a field was set
//     lacks it, so the ids "lacking" a value are the concept's history -- after
//     a backfill, every id it had;
//   - anything but a string: the lookup compares the TEXT extraction, which
//     agrees with the typed comparison only for strings (5.0 is not the text of
//     5, so an id whose latest version matches could go unfound);
//   - a nested path: no lookup index leads with one.
//
// Every set string equality the filter requires is ANDed into the lookup, the
// indexed one and any other: each is a property the latest version has, so
// each can only narrow. But only a field a lookup index serves STARTS a lookup:
// without the index the lookup reads every version's payload, which is the cost
// the latest-key enumeration exists to avoid (a heartbeat concept carries
// hundreds of thousands of versions of a few ids).
//
// The latest version of each found id is a LATERAL probe of the latest-row
// index, so the plan is one range of the lookup index plus one probe per id
// whatever the statistics say: the same fence the payload probe in
// executeCombinedFilterQuery relies on.
func (e *MemQLEngine) latestLookupKeys(concept string, equalities []payloadEquality, idFilters []compiledExpression, timestamp *time.Time) *bun.SelectQuery {
	served := false
	for _, equality := range equalities {
		if payloadLookupFor(concept, equality.field) {
			served = true
			break
		}
	}
	if !served {
		return nil
	}
	db := e.database()
	lookup := db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).Column("id").Distinct().
		Where("concept = ?", concept)
	for _, equality := range equalities {
		lookup = lookup.Where("(payload->>?) = ?", equality.field, equality.value)
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
