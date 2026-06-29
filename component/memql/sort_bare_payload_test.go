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
		{"bare nested payload", "meta.rank", sortFieldPayload, "payload.meta.rank"},
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
