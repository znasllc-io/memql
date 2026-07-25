package memql

import (
	"strings"
	"testing"
)

// TestRowNamespaceInSortKeys locks the ordering half of memql#2779. Before
// it, `sort "row.createdAt"` fell through the bare-payload branch and ordered
// on the JSONB path {row,createdAt} -- a path no stored row carries, so the
// sort was a silent no-op rather than an error.
func TestRowNamespaceInSortKeys(t *testing.T) {
	cases := []struct {
		key      string
		wantKind sortFieldKind
		wantName string
	}{
		{"row.createdAt", sortFieldCreatedAt, "createdAt"},
		{"row.createdBy", sortFieldCreatedBy, "createdBy"},
		{"row.id", sortFieldId, "id"},
		{"row.concept", sortFieldConcept, "concept"},
		{"row.type", sortFieldType, "type"},
		// Case-insensitive head and leaf.
		{"Row.CreatedAt", sortFieldCreatedAt, "createdAt"},
	}

	for _, tc := range cases {
		got, err := compileSortField(SortField{Field: tc.key, Direction: SortDirectionDesc})
		if err != nil {
			t.Fatalf("compileSortField(%q): unexpected error: %v", tc.key, err)
		}
		if got.kind != tc.wantKind {
			t.Errorf("compileSortField(%q).kind = %v, want %v", tc.key, got.kind, tc.wantKind)
		}
		if got.name != tc.wantName {
			t.Errorf("compileSortField(%q).name = %q, want %q", tc.key, got.name, tc.wantName)
		}
		if len(got.payloadPath) != 0 {
			t.Errorf("compileSortField(%q) resolved to payload path %v -- must be an intrinsic", tc.key, got.payloadPath)
		}
	}
}

// TestRowNamespaceSortRejectsNonIntrinsic -- a payload property under `row.`
// is an error, not a silent payload ordering. `provenance` is an
// object-valued intrinsic with no ordering, so it is rejected too.
func TestRowNamespaceSortRejectsNonIntrinsic(t *testing.T) {
	for _, key := range []string{"row.title", "row.status", "row.schema", "row.provenance"} {
		got, err := compileSortField(SortField{Field: key})
		if err == nil {
			t.Fatalf("compileSortField(%q) = %+v, want an error", key, got)
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("compileSortField(%q) error must quote the offending key, got: %v", key, err)
		}
	}
}

// TestSortKeysWithoutRowNamespaceUnchanged guards against over-reach: bare
// intrinsics and bare payload properties keep compiling exactly as before, so
// existing `sort "createdAt", "desc"` clauses and every runtime/SDK caller
// are untouched.
func TestSortKeysWithoutRowNamespaceUnchanged(t *testing.T) {
	cases := []struct {
		key      string
		wantKind sortFieldKind
	}{
		{"createdAt", sortFieldCreatedAt},
		{"id", sortFieldId},
		{"title", sortFieldPayload},
		{"payload.title", sortFieldPayload},
		// Properties that merely start with "row" are payload properties.
		{"rowCount", sortFieldPayload},
		{"rows.title", sortFieldPayload},
	}

	for _, tc := range cases {
		got, err := compileSortField(SortField{Field: tc.key})
		if err != nil {
			t.Fatalf("compileSortField(%q): unexpected error: %v", tc.key, err)
		}
		if got.kind != tc.wantKind {
			t.Errorf("compileSortField(%q).kind = %v, want %v", tc.key, got.kind, tc.wantKind)
		}
	}
}
