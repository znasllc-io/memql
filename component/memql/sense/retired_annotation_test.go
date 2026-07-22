package sense

import (
	"strings"
	"testing"
)

// TestSenseSurfacesRetiredAnnotationBury pins the third bury gate (#2708 /
// #2709): the sense editor's semantic diagnostics must surface the SAME
// pointed retirement message the loader emits, not a generic
// not-valid-for-receiver message -- so an author sees the migration hint at
// edit time. semanticDiagnostics only runs with a non-nil registry, so this
// uses a stub (New(nil) skips the annotation gate entirely).
func TestSenseSurfacesRetiredAnnotationBury(t *testing.T) {
	s := New(&stubRegistry{})
	cases := []struct {
		name   string
		src    string
		ticket string
	}{
		{"role", "@role(\"admin\")\nquery user queryAdminThing {\n  filter isActiveRecord\n}\n", "#2709"},
		{"internal", "@internal\nquery user queryHiddenThing {\n  filter isActiveRecord\n}\n", "#2708"},
		{"permission", "@permission(\"read:users\")\nquery user queryPermThing {\n  filter isActiveRecord\n}\n", "#2713"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var found bool
			for _, d := range s.Diagnose(tc.src, "x/queries.memql") {
				if d.Code == "invalid-annotation" && strings.Contains(d.Message, tc.ticket) {
					found = true
					if d.Severity != SeverityError {
						t.Errorf("bury diagnostic severity = %v, want SeverityError", d.Severity)
					}
				}
			}
			if !found {
				t.Fatalf("sense must surface the pointed %s bury message for @%s", tc.ticket, tc.name)
			}
		})
	}
}
