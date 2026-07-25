package memql

import (
	"strings"
	"testing"
)

// TestRowNamespaceResolvesIntrinsics locks in memql#2779: `row.<intrinsic>`
// is the canonical way to name a row intrinsic in a filter predicate, and it
// must resolve to the intrinsic itself -- NOT fall through the bare-payload
// rewrite into `payload.row.<intrinsic>`, a JSON path no stored row carries.
//
// Before the fix `row` was absent from reservedFilterHead, so
// barePayloadFieldRef prefixed it like any other bare property and the query
// silently matched zero rows forever.
func TestRowNamespaceResolvesIntrinsics(t *testing.T) {
	cases := []struct {
		authored []string
		want     []string
	}{
		{[]string{"row", "id"}, []string{"id"}},
		{[]string{"row", "concept"}, []string{"concept"}},
		{[]string{"row", "type"}, []string{"type"}},
		{[]string{"row", "createdAt"}, []string{"createdAt"}},
		{[]string{"row", "createdBy"}, []string{"createdBy"}},
		{[]string{"row", "provenance", "kind"}, []string{"provenance", "kind"}},
		// Case-insensitive head + canonicalised leaf casing.
		{[]string{"Row", "createdat"}, []string{"createdAt"}},
	}

	for _, tc := range cases {
		ref := FieldReference{Raw: strings.Join(tc.authored, "."), Parts: tc.authored}
		got, err := filterFieldRef(ref)
		if err != nil {
			t.Fatalf("filterFieldRef(%v): unexpected error: %v", tc.authored, err)
		}
		if strings.Join(got.Parts, ".") != strings.Join(tc.want, ".") {
			t.Errorf("filterFieldRef(%v).Parts = %v, want %v", tc.authored, got.Parts, tc.want)
		}
		if got.Raw != strings.Join(tc.want, ".") {
			t.Errorf("filterFieldRef(%v).Raw = %q, want %q", tc.authored, got.Raw, strings.Join(tc.want, "."))
		}
	}
}

// TestRowNamespaceRejectsNonIntrinsicLeaf keeps the namespace honest: `row.`
// addresses the row envelope only. A payload property written under it (the
// natural mistake once `row.` exists) must be a loud error naming the bare
// form, never a silent payload lookup.
func TestRowNamespaceRejectsNonIntrinsicLeaf(t *testing.T) {
	for _, leaf := range []string{"region", "status", "ownerUserId"} {
		ref := FieldReference{Raw: "row." + leaf, Parts: []string{"row", leaf}}
		got, err := filterFieldRef(ref)
		if err == nil {
			t.Fatalf("filterFieldRef(row.%s) = %v, want an error", leaf, got.Parts)
		}
		if !strings.Contains(err.Error(), "row."+leaf) {
			t.Errorf("filterFieldRef(row.%s) error must quote the offending path, got: %v", leaf, err)
		}
		// The advice must be actionable: name the bare replacement.
		if !strings.Contains(err.Error(), "`"+leaf+"`") {
			t.Errorf("filterFieldRef(row.%s) must suggest the bare form, got: %v", leaf, err)
		}
	}
}

// TestRowNamespaceRejectsUnfilterableIntrinsics -- `partition` and `schema`
// ARE row intrinsics, but the filter compiler has no push-down for either.
// Telling the author to "write it bare" (the payload advice) would send them
// into the same wall one edit later, so they get their own message.
func TestRowNamespaceRejectsUnfilterableIntrinsics(t *testing.T) {
	for _, leaf := range []string{"partition", "schema"} {
		ref := FieldReference{Raw: "row." + leaf, Parts: []string{"row", leaf}}
		_, err := filterFieldRef(ref)
		if err == nil {
			t.Fatalf("filterFieldRef(row.%s) = nil error, want an error", leaf)
		}
		if !strings.Contains(err.Error(), "not filter-comparable") {
			t.Errorf("filterFieldRef(row.%s) must say it is not filter-comparable, got: %v", leaf, err)
		}
		if strings.Contains(err.Error(), "named BARE") {
			t.Errorf("filterFieldRef(row.%s) must NOT suggest the bare form -- it fails too. Got: %v", leaf, err)
		}
	}
}

// TestRowNamespaceRejectsNestedPathsOnScalars -- the namespace must not be
// WEAKER than the bare spelling. validateFieldReference rejects `id.foo` at
// parse time; before this check `row.id.foo` slipped through the rewrite and
// failed later at SQL compile, with the authored `row.` missing from the
// message.
func TestRowNamespaceRejectsNestedPathsOnScalars(t *testing.T) {
	for _, parts := range [][]string{
		{"row", "id", "foo"},
		{"row", "createdAt", "foo"},
		{"row", "concept", "a", "b"},
	} {
		ref := FieldReference{Raw: strings.Join(parts, "."), Parts: parts}
		got, err := filterFieldRef(ref)
		if err == nil {
			t.Fatalf("filterFieldRef(%v) = %v, want a nested-path error", parts, got.Parts)
		}
		if !strings.Contains(err.Error(), "nested paths") {
			t.Errorf("filterFieldRef(%v) should name the nested-path problem, got: %v", parts, err)
		}
	}
}

// TestRowProvenanceLeafValidation -- provenance is the one object-valued
// intrinsic. Its leaf is validated at parse time against the same allow-list
// the compiler enforces, so the error keeps the authored `row.` spelling.
func TestRowProvenanceLeafValidation(t *testing.T) {
	for _, leaf := range []string{"kind", "name", "trigger", "via"} {
		parts := []string{"row", "provenance", leaf}
		got, err := filterFieldRef(FieldReference{Raw: strings.Join(parts, "."), Parts: parts})
		if err != nil {
			t.Fatalf("filterFieldRef(row.provenance.%s): unexpected error: %v", leaf, err)
		}
		if strings.Join(got.Parts, ".") != "provenance."+leaf {
			t.Errorf("filterFieldRef(row.provenance.%s).Parts = %v, want [provenance %s]", leaf, got.Parts, leaf)
		}
	}
	for _, bad := range [][]string{
		{"row", "provenance"},              // object, no leaf
		{"row", "provenance", "bogus"},     // not a provenance field
		{"row", "provenance", "kind", "x"}, // too deep
	} {
		if _, err := filterFieldRef(FieldReference{Raw: strings.Join(bad, "."), Parts: bad}); err == nil {
			t.Errorf("filterFieldRef(%v) = nil error, want a provenance error", bad)
		}
	}
}

// TestRowNamespaceRejectsBareRow -- `row` alone addresses nothing.
func TestRowNamespaceRejectsBareRow(t *testing.T) {
	ref := FieldReference{Raw: "row", Parts: []string{"row"}}
	if _, err := filterFieldRef(ref); err == nil {
		t.Fatal("filterFieldRef(row) = nil error, want an error")
	}
}

// TestFilterFieldRefLeavesOtherHeadsAlone guards against over-reach: the
// reserved engine heads and bare payload properties must behave exactly as
// they did before the `row.` namespace existed.
func TestFilterFieldRefLeavesOtherHeadsAlone(t *testing.T) {
	cases := []struct {
		authored []string
		want     []string
	}{
		{[]string{"actor", "userId"}, []string{"actor", "userId"}},
		{[]string{"args", "spaceId"}, []string{"args", "spaceId"}},
		{[]string{"payload", "status"}, []string{"payload", "status"}},
		{[]string{"provenance", "kind"}, []string{"provenance", "kind"}},
		// Bare payload properties still get the payload prefix (epic #2292).
		{[]string{"status"}, []string{"payload", "status"}},
		{[]string{"preferences", "theme"}, []string{"payload", "preferences", "theme"}},
		// A property that merely STARTS with "row" is not the namespace.
		{[]string{"rowCount"}, []string{"payload", "rowCount"}},
		{[]string{"rows", "id"}, []string{"payload", "rows", "id"}},
	}

	for _, tc := range cases {
		ref := FieldReference{Raw: strings.Join(tc.authored, "."), Parts: tc.authored}
		got, err := filterFieldRef(ref)
		if err != nil {
			t.Fatalf("filterFieldRef(%v): unexpected error: %v", tc.authored, err)
		}
		if strings.Join(got.Parts, ".") != strings.Join(tc.want, ".") {
			t.Errorf("filterFieldRef(%v).Parts = %v, want %v", tc.authored, got.Parts, tc.want)
		}
	}
}

// TestRowIsReservedFilterHead -- the head must be reserved so no other
// rewrite pass treats it as a payload property.
func TestRowIsReservedFilterHead(t *testing.T) {
	for _, name := range []string{"row", "Row", "ROW", " row "} {
		if !reservedFilterHead(name) {
			t.Errorf("reservedFilterHead(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"rows", "rowCount", "status"} {
		if reservedFilterHead(name) {
			t.Errorf("reservedFilterHead(%q) = true, want false", name)
		}
	}
}

// TestRewriteFilterFieldRefsPropagatesError -- the error from a bad `row.`
// leaf must surface out of the whole-tree walk, not be swallowed mid-tree.
func TestRewriteFilterFieldRefsPropagatesError(t *testing.T) {
	expr := &LogicalExpression{
		Op:   LogicalAnd,
		Left: &ComparisonExpression{Field: FieldReference{Raw: "status", Parts: []string{"status"}}},
		Right: &ComparisonExpression{
			Field: FieldReference{Raw: "row.region", Parts: []string{"row", "region"}},
		},
	}
	if err := rewriteFilterFieldRefs(expr); err == nil {
		t.Fatal("rewriteFilterFieldRefs = nil error, want the nested row.region error")
	}
}
