package agent

import (
	"slices"
	"testing"
)

// TestScopeToolsForDeliverableSurface is the memql#950 guard: a
// produceArtifact executor turn (deliverable_surface=workbench) must NOT keep
// canvasPublish, so the file body can't be dumped onto the canvas as a content
// card. Every other turn keeps its full tool set.
func TestScopeToolsForDeliverableSurface(t *testing.T) {
	full := []string{"workbenchHost", "canvasPublish", "respondToUser", "workerHost"}

	cases := []struct {
		name  string
		hints map[string]string
		want  []string
	}{
		{
			name:  "workbench surface drops canvasPublish",
			hints: map[string]string{"deliverable_surface": "workbench"},
			want:  []string{"workbenchHost", "respondToUser", "workerHost"},
		},
		{
			name:  "case-insensitive value",
			hints: map[string]string{"deliverable_surface": "Workbench"},
			want:  []string{"workbenchHost", "respondToUser", "workerHost"},
		},
		{
			name:  "absent hint keeps canvasPublish",
			hints: map[string]string{"execution_lane": "background"},
			want:  full,
		},
		{
			name:  "nil hints keeps canvasPublish",
			hints: nil,
			want:  full,
		},
		{
			name:  "other surface value keeps canvasPublish",
			hints: map[string]string{"deliverable_surface": "canvas"},
			want:  full,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Copy so the in-place filter can't corrupt the shared fixture.
			in := slices.Clone(full)
			got := ScopeToolsForDeliverableSurface(tc.hints, in)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ScopeToolsForDeliverableSurface = %v, want %v", got, tc.want)
			}
		})
	}
}
