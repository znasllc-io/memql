package planner

import "testing"

// memql#900: planRowsFromExecuteResult reads the opt-in watch flag off
// Plan.input.watchExecution so executeApprovedPlan can route the turn
// through the interactive streaming lane instead of the background default.
func TestPlanRowsFromExecuteResult_WatchExecution(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
		want bool
	}{
		{
			name: "watch opt-in true",
			data: map[string]any{"id": "v1:planner:plan:p1", "input": map[string]any{"watchExecution": true}},
			want: true,
		},
		{
			name: "watch explicit false",
			data: map[string]any{"id": "p2", "input": map[string]any{"watchExecution": false}},
			want: false,
		},
		{
			name: "input present without the flag",
			data: map[string]any{"id": "p3", "input": map[string]any{"attachmentId": "a1"}},
			want: false,
		},
		{
			name: "no input object at all",
			data: map[string]any{"id": "p4"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := planRowsFromExecuteResult(map[string]any{"data": tc.data})
			if len(rows) != 1 {
				t.Fatalf("expected 1 row, got %d", len(rows))
			}
			if rows[0].WatchExecution != tc.want {
				t.Fatalf("WatchExecution = %v, want %v", rows[0].WatchExecution, tc.want)
			}
		})
	}
}
