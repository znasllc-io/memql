package memql

import "testing"

// TestRewriteBarePayloadFilterFields verifies the bare-payload filter
// rewrite (epic #2292): a bare property becomes payload.<prop>, while
// reserved heads (intrinsics, actor/args/config, already-payload paths)
// are left untouched.
func TestRewriteBarePayloadFilterFields(t *testing.T) {
	cases := []struct {
		name string
		in   []string // FieldReference parts
		want []string
	}{
		{"bare payload prop", []string{"status"}, []string{"payload", "status"}},
		{"bare nested payload", []string{"address", "city"}, []string{"payload", "address", "city"}},
		{"already payload", []string{"payload", "status"}, []string{"payload", "status"}},
		{"actor head", []string{"actor", "role"}, []string{"actor", "role"}},
		{"args head", []string{"args", "spaceId"}, []string{"args", "spaceId"}},
		{"id intrinsic", []string{"id"}, []string{"id"}},
		{"createdBy intrinsic", []string{"createdBy"}, []string{"createdBy"}},
		{"concept intrinsic", []string{"concept"}, []string{"concept"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmp := &ComparisonExpression{
				Field:    FieldReference{Raw: joinParts(tc.in), Parts: tc.in},
				Operator: OpEq,
				Value:    "x",
			}
			if err := rewriteFilterFieldRefs(cmp); err != nil {
				t.Fatalf("rewriteFilterFieldRefs: %v", err)
			}
			got := cmp.Field.Parts
			if len(got) != len(tc.want) {
				t.Fatalf("parts = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parts = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
