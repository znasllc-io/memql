package workbench

import "testing"

// TestPlanOwnerFromRow is the memql#952 guard: the produceArtifact deliverable
// must be owned by the USER who requested it (payload.requestedBy), not the
// row-intrinsic createdBy. On that path the Plan row is inserted by the
// planner's system actor, so createdBy is "system:planner"; using it stamped
// the generatedOutput with the wrong owner -> invisible in the Library and a
// false plan-failed from the owner-scoped success check (memql#939).
func TestPlanOwnerFromRow(t *testing.T) {
	cases := []struct {
		name string
		row  map[string]any
		want string
	}{
		{
			name: "requestedBy wins over system createdBy",
			row: map[string]any{
				"requestedBy": "v1:identity:user:u1",
				"createdBy":   "system:planner",
			},
			want: "v1:identity:user:u1",
		},
		{
			name: "falls back to createdBy when requestedBy absent",
			row:  map[string]any{"createdBy": "v1:identity:user:u2"},
			want: "v1:identity:user:u2",
		},
		{
			name: "blank requestedBy falls back to createdBy",
			row: map[string]any{
				"requestedBy": "   ",
				"createdBy":   "v1:identity:user:u3",
			},
			want: "v1:identity:user:u3",
		},
		{
			name: "empty row yields empty owner (promotion skips)",
			row:  map[string]any{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planOwnerFromRow(tc.row); got != tc.want {
				t.Fatalf("planOwnerFromRow = %q, want %q", got, tc.want)
			}
		})
	}
}
