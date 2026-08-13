package memql

import "testing"

// TestCompileSortFieldBarePayload verifies bare payload access for sort
// keys (epic #2292 / #2295): a bare property sorts on the payload
// surface, the explicit payload.<path> form still works, and intrinsics
// stay intrinsics.
func TestCompileSortFieldBarePayload(t *testing.T) {
	cases := []struct {
		name     string
		field    string
		wantKind sortFieldKind
		wantName string
	}{
		{"bare payload prop", "version", sortFieldPayload, "payload.version"},
		// Was "meta.rank" until memql#3613. `meta` is a reserved engine
		// namespace, so no concept can declare it and the path this compiled
		// to could never exist -- the case asserted a silent no-op ordering.
		// TestCompileSortFieldRefusesReservedHead pins the refusal; the nested
		// payload shape it was here to cover is unchanged.
		{"bare nested payload", "preferences.rank", sortFieldPayload, "payload.preferences.rank"},
		{"explicit payload passthrough", "payload.title", sortFieldPayload, "payload.title"},
		{"createdAt intrinsic", "createdAt", sortFieldCreatedAt, "createdAt"},
		{"id intrinsic", "id", sortFieldId, "id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := compileSortField(SortField{Field: tc.field, Direction: SortDirectionDesc})
			if err != nil {
				t.Fatalf("compileSortField(%q): %v", tc.field, err)
			}
			if got.kind != tc.wantKind {
				t.Errorf("kind = %v, want %v", got.kind, tc.wantKind)
			}
			if got.name != tc.wantName {
				t.Errorf("name = %q, want %q", got.name, tc.wantName)
			}
		})
	}

	if _, err := compileSortField(SortField{Field: "", Direction: SortDirectionAsc}); err == nil {
		t.Error("expected an error for an empty sort field")
	}
}

// TestCompileSortFieldRefusesReservedHead is the sort-key half of memql#3613.
// The same bare token meant the row envelope in a filter and the payload in a
// sort key:
//
//	filter provenance.kind == x  ->  provenance->>'kind'            (row column)
//	sort   "provenance.kind"     ->  payload #>> '{provenance,kind}'
//
// Now that no concept may declare any of these names, the payload path can
// never exist, so the old compilation was a guaranteed silent no-op ordering.
func TestCompileSortFieldRefusesReservedHead(t *testing.T) {
	for _, field := range []string{
		"provenance", "provenance.kind",
		"actor", "actor.userId",
		"args", "args.limit",
		"meta", "meta.rank",
		"payload", "now", "config", "trace", "schema", "partition",
		// Case-insensitively, matching how the plan parser classifies a head.
		"Provenance", "ACTOR.userId",
	} {
		if _, err := compileSortField(SortField{Field: field, Direction: SortDirectionDesc}); err == nil {
			t.Errorf("compileSortField(%q) succeeded; a reserved engine namespace is not a payload property", field)
		}
	}
}

// The refusal is scoped to the reserved head itself. A property that merely
// begins with one of those words, or nests under an ordinary property of the
// same shape, is untouched -- otherwise the guard would refuse legitimate
// payload sorts like "metadata" or "arguments".
func TestCompileSortFieldStillAcceptsLookalikePayloadProps(t *testing.T) {
	for _, field := range []string{"metadata", "arguments", "configuration", "rowCount", "actorUserId", "settings.meta"} {
		got, err := compileSortField(SortField{Field: field, Direction: SortDirectionAsc})
		if err != nil {
			t.Errorf("compileSortField(%q): %v", field, err)
			continue
		}
		if got.kind != sortFieldPayload {
			t.Errorf("compileSortField(%q).kind = %v, want sortFieldPayload", field, got.kind)
		}
	}
}

// The explicit `payload.` prefix names its surface, so it is left alone: a
// runtime caller who means the payload and says so keeps working.
func TestCompileSortFieldExplicitPayloadPrefixIsUnaffected(t *testing.T) {
	got, err := compileSortField(SortField{Field: "payload.provenance", Direction: SortDirectionAsc})
	if err != nil {
		t.Fatalf("compileSortField(\"payload.provenance\"): %v", err)
	}
	if got.name != "payload.provenance" {
		t.Errorf("name = %q, want %q", got.name, "payload.provenance")
	}
}
