package memql

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/baseparser"
)

// TestRemovedAnnotationsRejected locks in #989: the recognized-but-unused and
// retired annotations are gone from the per-construct allow-lists, so each now
// hard-errors as an unknown annotation at load time instead of slipping through
// to the generic attribute map. Zero in-tree occurrences (#964) backs the hard
// cutover -- there is nothing to migrate.
func TestRemovedAnnotationsRejected(t *testing.T) {
	cases := []struct {
		annotation string
		kindLabel  string
		header     string // construct header line (keyword + name + concept where needed)
		allowed    map[string]bool
	}{
		// Query allow-list removals.
		{"deprecated", "query", "query lead removedAnnoQuery", allowedQueryAnnotations},
		{"cacheTTL", "query", "query lead removedAnnoQuery", allowedQueryAnnotations},
		{"timeout", "query", "query lead removedAnnoQuery", allowedQueryAnnotations},
		{"rateLimit", "query", "query lead removedAnnoQuery", allowedQueryAnnotations},
		{"retry", "query", "query lead removedAnnoQuery", allowedQueryAnnotations},
		{"audit", "query", "query lead removedAnnoQuery", allowedQueryAnnotations},
		{"role", "query", "query lead removedAnnoQuery", allowedQueryAnnotations},
		{"permission", "query", "query lead removedAnnoQuery", allowedQueryAnnotations},
		// Mutation allow-list removals.
		{"idempotent", "mutation", "mutation lead removedAnnoMutation", allowedMutationAnnotations},
		{"destructive", "mutation", "mutation lead removedAnnoMutation", allowedMutationAnnotations},
		{"requiresConfirmation", "mutation", "mutation lead removedAnnoMutation", allowedMutationAnnotations},
		{"audit", "mutation", "mutation lead removedAnnoMutation", allowedMutationAnnotations},
		// Logic allow-list removal.
		{"deprecated", "logic", "logic removedAnnoLogic", allowedLogicAnnotations},
		// Automation allow-list removals.
		{"schedule", "automation", "automation removedAnnoAutomation", allowedAutomationAnnotations},
		{"async", "automation", "automation removedAnnoAutomation", allowedAutomationAnnotations},
		{"deprecated", "automation", "automation removedAnnoAutomation", allowedAutomationAnnotations},
	}

	for _, tc := range cases {
		t.Run(tc.kindLabel+"/@"+tc.annotation, func(t *testing.T) {
			src := "@" + tc.annotation + "\n" + tc.header + " {\n}\n"
			err := baseparser.ValidateConstructAnnotations(src, tc.kindLabel, tc.allowed)
			if err == nil {
				t.Fatalf("expected @%s on a %s to be rejected, but it was accepted", tc.annotation, tc.kindLabel)
			}
			// @role (#2709) and @permission (#2713) graduated from the
			// generic unknown-annotation error to the pointed BURY
			// message -- the load rejection this test locks in is
			// preserved, sharper.
			if buried := map[string]string{"role": "#2709", "permission": "#2713"}; buried[tc.annotation] != "" {
				if !strings.Contains(err.Error(), buried[tc.annotation]) {
					t.Fatalf("expected the @%s bury message (%s), got: %v", tc.annotation, buried[tc.annotation], err)
				}
				return
			}
			if !strings.Contains(err.Error(), "unknown "+tc.kindLabel+" annotation @"+tc.annotation) {
				t.Fatalf("expected unknown-annotation error for @%s on %s, got: %v", tc.annotation, tc.kindLabel, err)
			}
		})
	}
}

// TestKeptAnnotationsStillAccepted guards against over-removal: the surviving
// construct annotations must still pass the gate.
func TestKeptAnnotationsStillAccepted(t *testing.T) {
	cases := []struct {
		annotation string
		kindLabel  string
		header     string
		allowed    map[string]bool
	}{
		{"description", "query", "query lead keptAnnoQuery", allowedQueryAnnotations},
		{"public", "query", "query lead keptAnnoQuery", allowedQueryAnnotations},
		{"actor", "mutation", "mutation lead keptAnnoMutation", allowedMutationAnnotations},
		{"trigger", "automation", "automation keptAnnoAutomation", allowedAutomationAnnotations},
		{"filter", "automation", "automation keptAnnoAutomation", allowedAutomationAnnotations},
	}

	for _, tc := range cases {
		t.Run(tc.kindLabel+"/@"+tc.annotation, func(t *testing.T) {
			src := "@" + tc.annotation + "\n" + tc.header + " {\n}\n"
			if err := baseparser.ValidateConstructAnnotations(src, tc.kindLabel, tc.allowed); err != nil {
				t.Fatalf("expected @%s on a %s to be accepted, got: %v", tc.annotation, tc.kindLabel, err)
			}
		})
	}
}
