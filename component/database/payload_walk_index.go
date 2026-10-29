package database

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"
)

// A PAYLOAD WALK INDEX is ((payload->>'<field>'), concept, "createdAt" DESC,
// id), partial on the concepts it serves.
//
// It holds what a payload lookup index holds, in time order: the versions
// carrying one value of the field, newest first, with the id tie-break the
// engine pages by (createdAt DESC, id ASC). A read that pages one value's rows
// newest first walks this range from the top and stops at its page, instead of
// learning every id the value ever carried (component/memql latestScanWalk,
// which says when the engine takes it, and why so narrowly).
//
// component/memql's payloadWalks names every index of this shape, and
// TestPayloadWalksAreTheMigratedWalkIndexes holds the two lists equal. Built
// and verified through ensureNamedWorkIndex, like the lookup indexes.

// payloadWalkIndexMatches reports whether an index is exactly the walk index
// for field over concepts, on the root or on a chunk.
func payloadWalkIndexMatches(field string, concepts []string) func(indexFacts) bool {
	root, folded := payloadLookupIndexPredicates(concepts)
	return func(f indexFacts) bool {
		return f.Method == "btree" && f.Partial && f.Expression &&
			(f.Predicate == root || f.Predicate == folded) &&
			f.Expressions == "(payload ->> '"+field+"'::text)" && len(f.KeyColumns) == 4 &&
			f.KeyColumns[0] == "" && f.KeyColumns[1] == "concept" && f.KeyColumns[2] == "createdAt" && f.KeyColumns[3] == "id" &&
			len(f.KeyDescending) == 4 && !f.KeyDescending[0] && !f.KeyDescending[1] && f.KeyDescending[2] && !f.KeyDescending[3]
	}
}

// ensurePayloadWalkIndex builds the walk index for field over concepts under
// name, or verifies the one already there.
func ensurePayloadWalkIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table, name, field string, concepts []string) error {
	root, _ := payloadLookupIndexPredicates(concepts)
	return ensureNamedWorkIndex(ctx, db, logger, table, name,
		`((payload->>'`+field+`'),concept,"createdAt" DESC,id)`, root, payloadWalkIndexMatches(field, concepts))
}
