//go:build computeruse

package tools

import "testing"

// Targeted clicks never borrow the unrelated global cursor's region exemption.
// The fake gate denies; this test emits no input.
func TestDispatcher_MouseClickRoutesThroughAllowsAt_GUI(t *testing.T) {
	cursor := mouseClickAllowsAtCursor(t)
	if cursor.Known {
		t.Errorf("explicit target must not inherit live cursor consent: %+v", cursor)
	}
}
