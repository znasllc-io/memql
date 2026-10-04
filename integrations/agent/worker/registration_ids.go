//go:build agent || planner

package worker

import "strings"

// sameRegistration compares two machine registration ids that may be spelled
// canonically (`v1:worker:registration:abc`) on one side and bare (`abc`) on
// the other: a pin arrives canonical from the AiChat seam, and a row read
// back from the graph may be either.
//
// It keeps the text-after-the-last-colon rule that sameSubject used to apply
// to everything. A registration id names a machine, never an actor, so the
// confusion that rule caused for people -- `system:automation:ana` read as
// `ana` -- cannot arise here, and people are compared by sameSubject.
func sameRegistration(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return a != ""
	}
	return a != "" && b != "" && lastSegment(a) == lastSegment(b)
}
