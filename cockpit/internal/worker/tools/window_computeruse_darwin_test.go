//go:build computeruse && darwin

package tools

import "testing"

// TestListWindows_Smoke is a local-only smoke test for the
// CGWindowList-backed lister (CI runs headless, so this never runs
// there). It asserts the call neither panics nor returns a
// malformed row; it makes no assumption about WHICH windows exist.
// Skips gracefully when no window-server session is reachable
// (e.g. an ssh login without Aqua).
func TestListWindows_Smoke(t *testing.T) {
	wins, fail := listWindows()
	if fail != nil {
		if fail.GetErrorCode() == "window_list_failed" {
			t.Skipf("no window-server session: %s", fail.GetErrorMessage())
		}
		t.Fatalf("listWindows failed: %s: %s", fail.GetErrorCode(), fail.GetErrorMessage())
	}
	focused := 0
	for i, w := range wins {
		if w.ID == 0 {
			t.Errorf("window[%d] has zero id: %+v", i, w)
		}
		if w.App == "" {
			t.Errorf("window[%d] has empty app (kCGWindowOwnerName is always available): %+v", i, w)
		}
		if w.Focused {
			focused++
		}
	}
	if len(wins) > 0 && focused != 1 {
		t.Errorf("expected exactly one focused window in a non-empty list, got %d (of %d)", focused, len(wins))
	}
	t.Logf("listWindows: %d layer-0 on-screen windows", len(wins))
}

// TestFocusWindow_NotFound pins the window_not_found contract using
// an id no real window will hold (CGWindowIDs are uint32; the lister
// can never return an id above that range).
func TestFocusWindow_NotFound(t *testing.T) {
	if _, fail := listWindows(); fail != nil {
		t.Skipf("no window-server session: %s", fail.GetErrorMessage())
	}
	_, _, fail := focusWindow(uint64(1) << 40)
	if fail == nil {
		t.Fatal("expected window_not_found for an impossible id")
	}
	if fail.GetErrorCode() != "window_not_found" {
		t.Errorf("failure code = %q, want window_not_found (%s)", fail.GetErrorCode(), fail.GetErrorMessage())
	}
}
