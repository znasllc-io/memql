package memoryNodes

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/core/id"
)

// concept_storage_id_2784_test.go pins the WRITE half of the by-id round trip
// that memql#2784 depends on.
//
// A mutation stamping `id: args.X` does not store X verbatim: Concept.Create
// routes it through storageId, which canonicalises a bare id to
// "{concept}:{X}". A query filtering `row.id == args.X` goes the other way,
// through component/memql's resolveFullId. The two must produce the identical
// string or every by-id query silently matches nothing -- silently, because a
// mismatch returns zero rows rather than erroring.
//
// The companion test on the read side (TestRowIdFilterRoundTripsWithStorageId,
// component/memql/executor_filter_test.go) compares resolveFullId against
// id.BuildNodeId. This one pins that storageId really is id.BuildNodeId for a
// bare id -- otherwise that test would be comparing the read path against a
// function the write path does not actually use, and storageId could drift
// underneath it.
func TestConceptStorageIdMatchesBuildNodeId(t *testing.T) {
	c := &Concept{Name: "v1:cluster:deployment"}

	t.Run("bare id is canonicalised", func(t *testing.T) {
		for _, bare := range []string{
			"d-9f3b7c2a",
			"UPPER-Case-Id",
			"id.with.dots",
			"id_with_underscores",
		} {
			require.Equal(t, id.BuildNodeId(c.Name, bare), c.storageId(bare),
				"storageId must compose exactly as BuildNodeId, or the read side resolves to a string the write side never stored")
		}
	})

	// The branch the read-side test cannot see: an already-qualified id is
	// returned untouched rather than double-prefixed. resolveFullId mirrors
	// this by returning a canonical id as-is.
	t.Run("already-qualified id is not double-prefixed", func(t *testing.T) {
		canonical := "v1:cluster:deployment:d-9f3b7c2a"
		require.Equal(t, canonical, c.storageId(canonical),
			"a concept-qualified id must pass through, not gain a second prefix")
	})

	t.Run("whitespace is trimmed before composing", func(t *testing.T) {
		require.Equal(t, id.BuildNodeId(c.Name, "d-1"), c.storageId("  d-1  "))
	})

	t.Run("empty id yields empty, so Create can reject it", func(t *testing.T) {
		require.Equal(t, "", c.storageId("   "))
	})
}
