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
		want := fmt.Sprintf(`CREATE INDEX %s ON public."MemoryNodes" USING btree (((payload ->> '%s'::text)), concept, id, "createdAt" DESC) WHERE (concept = ANY (ARRAY[%s]))`,
			lookup.index, lookup.field, strings.Join(quoted, ", "))
		require.Equal(t, want, catalog[lookup.index], "payloadLookups entry %s does not match the index the migrations build", lookup.index)
	}
	for name, definition := range catalog {
		require.True(t, registered[name], "index %s has the lookup shape but no payloadLookups entry, so no concept read can use it: %s", name, definition)
	}
}
