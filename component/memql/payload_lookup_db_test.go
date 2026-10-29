package memql

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// lookupIndexShape matches the definition of an index a concept read may look
// rows up through: a btree led by one payload text extraction, then the
// latest-row key. TestMain migrates public before the package runs, so the
// catalog read below is the one every node boots against.
var lookupIndexShape = regexp.MustCompile(`USING btree \(\(\(payload ->> '[^']+'::text\)\), concept, id, "createdAt" DESC\)`)

// THE LOOKUP REGISTRY IS THE CATALOG'S LOOKUP INDEXES, IN BOTH DIRECTIONS.
//
// latestScanKeys trusts payloadLookups to say which indexes exist; it never
// reads the catalog at runtime. This holds that trust to the catalog the
// migrations leave behind:
//
//   - every registered lookup is an index of EXACTLY the definition its entry
//     renders, usable, partial on exactly the entry's concepts, each of them
//     a concept this engine knows -- an entry the migrations do not build, or
//     build differently, would send reads to an enumeration no index serves;
//   - every index of the lookup shape is registered, so a lookup index cannot
//     land without the planner learning of it.
func TestPayloadLookupsAreTheMigratedLookupIndexes(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	rows, err := db.QueryContext(context.Background(), `
SELECT ic.relname, pg_get_indexdef(i.indexrelid), i.indisvalid AND i.indisready AND i.indislive
  FROM pg_index i
  JOIN pg_class ic ON ic.oid = i.indexrelid
 WHERE i.indrelid = 'public."MemoryNodes"'::regclass`)
	require.NoError(t, err)
	defer rows.Close()
	catalog := map[string]string{}
	for rows.Next() {
		var name, definition string
		var usable bool
		require.NoError(t, rows.Scan(&name, &definition, &usable))
		if !lookupIndexShape.MatchString(definition) {
			continue
		}
		require.True(t, usable, "lookup index %s is not usable (invalid, not ready or being dropped)", name)
		catalog[name] = definition
	}
	require.NoError(t, rows.Err())

	require.NotEmpty(t, payloadLookups)
	registered := map[string]bool{}
	for _, lookup := range payloadLookups {
		require.False(t, registered[lookup.index], "index %s is registered twice", lookup.index)
		registered[lookup.index] = true
		quoted := make([]string, len(lookup.concepts))
		for i, concept := range lookup.concepts {
			_, err := eng.concepts.Get(concept)
			require.NoError(t, err, "lookup %s names concept %q, which this engine does not declare", lookup.index, concept)
			quoted[i] = "'" + concept + "'::text"
		}
		want := fmt.Sprintf(`CREATE INDEX %s ON public."MemoryNodes" USING btree (((payload ->> '%s'::text)), concept, id, "createdAt" DESC) WHERE %s`,
			lookup.index, lookup.field, lookupPredicate(quoted, lookup.partial))
		require.Equal(t, want, catalog[lookup.index], "payloadLookups entry %s does not match the index the migrations build", lookup.index)
	}
	for name, definition := range catalog {
		require.True(t, registered[name], "index %s has the lookup shape but no payloadLookups entry, so no concept read can use it: %s", name, definition)
	}
}

// lookupPredicate renders a lookup or walk index's partial predicate as
// pg_get_indexdef deparses it: the concept list, ANDed with each further
// equality a partial lookup carries.
func lookupPredicate(quotedConcepts []string, partial []payloadEquality) string {
	concepts := "(concept = ANY (ARRAY[" + strings.Join(quotedConcepts, ", ") + "]))"
	if len(partial) == 0 {
		return concepts
	}
	conditions := []string{concepts}
	for _, equality := range partial {
		conditions = append(conditions, fmt.Sprintf("((payload ->> '%s'::text) = '%s'::text)", equality.field, equality.value))
	}
	return "(" + strings.Join(conditions, " AND ") + ")"
}

// walkIndexShape matches the definition of a newest-first walk index: a btree
// led by one payload text extraction, then the concept and the engine's page
// order (createdAt DESC, id).
var walkIndexShape = regexp.MustCompile(`USING btree \(\(\(payload ->> '[^']+'::text\)\), concept, "createdAt" DESC, id\)`)

// THE WALK REGISTRY IS THE CATALOG'S WALK INDEXES, IN BOTH DIRECTIONS, for
// TestPayloadLookupsAreTheMigratedLookupIndexes's reasons: latestScanWalk
// trusts payloadWalks and never reads the catalog. An entry the migrations do
// not build would still walk correctly -- the walk is a query, not an index
// hint -- but through a sort of the whole range, which is the cost it exists
// to avoid.
func TestPayloadWalksAreTheMigratedWalkIndexes(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	rows, err := db.QueryContext(context.Background(), `
SELECT ic.relname, pg_get_indexdef(i.indexrelid), i.indisvalid AND i.indisready AND i.indislive
  FROM pg_index i
  JOIN pg_class ic ON ic.oid = i.indexrelid
 WHERE i.indrelid = 'public."MemoryNodes"'::regclass`)
	require.NoError(t, err)
	defer rows.Close()
	catalog := map[string]string{}
	for rows.Next() {
		var name, definition string
		var usable bool
		require.NoError(t, rows.Scan(&name, &definition, &usable))
		if !walkIndexShape.MatchString(definition) {
			continue
		}
		require.True(t, usable, "walk index %s is not usable (invalid, not ready or being dropped)", name)
		catalog[name] = definition
	}
	require.NoError(t, rows.Err())

	require.NotEmpty(t, payloadWalks)
	registered := map[string]bool{}
	for _, walk := range payloadWalks {
		require.False(t, registered[walk.index], "index %s is registered twice", walk.index)
		registered[walk.index] = true
		quoted := make([]string, len(walk.concepts))
		for i, concept := range walk.concepts {
			_, err := eng.concepts.Get(concept)
			require.NoError(t, err, "walk %s names concept %q, which this engine does not declare", walk.index, concept)
			quoted[i] = "'" + concept + "'::text"
		}
		want := fmt.Sprintf(`CREATE INDEX %s ON public."MemoryNodes" USING btree (((payload ->> '%s'::text)), concept, "createdAt" DESC, id) WHERE %s`,
			walk.index, walk.field, lookupPredicate(quoted, nil))
		require.Equal(t, want, catalog[walk.index], "payloadWalks entry %s does not match the index the migrations build", walk.index)
	}
	for name, definition := range catalog {
		require.True(t, registered[name], "index %s has the walk shape but no payloadWalks entry, so no read walks it: %s", name, definition)
	}
}
