//go:build !computeruse

package tools

import "testing"

// TestDispatcher_MouseClickRoutesThroughAllowsAt_Headless is the
// headless half of the build-tag split from memql-cockpit#171. On
// the non-computeruse build cursorLocation() is the stub that always reports
// Known=false -- the correct headless behaviour: AllowsAt treats an
// unknown cursor as out-of-region and falls through to the gate.
func TestDispatcher_MouseClickRoutesThroughAllowsAt_Headless(t *testing.T) {
	cursor := mouseClickAllowsAtCursor(t)
	if cursor.Known {
		t.Errorf("headless build must report Known=false cursor; got %+v", cursor)
	}
}
