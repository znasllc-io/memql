package database

import (
	"context"
	"log/slog"
	"strings"

	"github.com/uptrace/bun"
)

// A PAYLOAD LOOKUP INDEX is ((payload->>'<field>'), concept, id, "createdAt"
// DESC), partial on the concepts it serves.
//
// The leading key finds the versions carrying one value of the field; the
// latest-row key after it keeps those versions in (concept, id) order, so the
// ids carrying one value are a single index range, read in id order with no
// sort (and skipped across duplicates by TimescaleDB's SkipScan). That order is
// what keeps the planner on this index rather than on the concept's whole
// latest-row range when a reader asks for the DISTINCT ids: measured on
// chunks without statistics, the two older payload indexes that end at the
// field lost exactly that choice.
//
// component/memql's payloadLookups names every index of this shape, and
// TestPayloadLookupsAreTheMigratedLookupIndexes holds the two lists equal: a
// concept read whose filter requires the field to equal a value enumerates only
// the ids this range holds (latestScanKeys) instead of every id of the concept.
//
// Built and verified through ensureNamedWorkIndex: the session build lock, one
// transaction per chunk on a hypertable, and a re-inspection of every chunk
// before the migration reports success.

// payloadLookupIndexPredicates renders the partial predicate as Postgres
// deparses it on the root, and as TimescaleDB folds it when it copies the index
// onto a chunk.
func payloadLookupIndexPredicates(concepts []string) (root, folded string) {
	quoted := make([]string, len(concepts))
	for i, concept := range concepts {
		quoted[i] = "'" + concept + "'::text"
	}
	root = "(concept = ANY (ARRAY[" + strings.Join(quoted, ", ") + "]))"
	folded = "(concept = ANY ('{" + strings.Join(concepts, ",") + "}'::text[]))"
	return root, folded
}

// payloadLookupIndexMatches reports whether an index is exactly the lookup
// index for field over concepts, on the root or on a chunk.
func payloadLookupIndexMatches(field string, concepts []string) func(indexFacts) bool {
	root, folded := payloadLookupIndexPredicates(concepts)
	return func(f indexFacts) bool {
		return f.Method == "btree" && f.Partial && f.Expression &&
			(f.Predicate == root || f.Predicate == folded) &&
			f.Expressions == "(payload ->> '"+field+"'::text)" && len(f.KeyColumns) == 4 &&
			f.KeyColumns[0] == "" && f.KeyColumns[1] == "concept" && f.KeyColumns[2] == "id" && f.KeyColumns[3] == "createdAt" &&
			len(f.KeyDescending) == 4 && !f.KeyDescending[0] && !f.KeyDescending[1] && !f.KeyDescending[2] && f.KeyDescending[3]
	}
}

// ensurePayloadLookupIndex builds the lookup index for field over concepts
// under name, or verifies the one already there.
func ensurePayloadLookupIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table, name, field string, concepts []string) error {
	root, _ := payloadLookupIndexPredicates(concepts)
	return ensureNamedWorkIndex(ctx, db, logger, table, name,
		`((payload->>'`+field+`'),concept,id,"createdAt" DESC)`, root, payloadLookupIndexMatches(field, concepts))
}
