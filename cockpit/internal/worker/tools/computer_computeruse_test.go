//go:build computeruse

package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests cover the pure (pre-RobotGo) validation paths of the
// memql-cockpit#166 handlers only -- nothing here posts real input.
// The post-validation paths drive the live cursor / keyboard and are
// exercised manually (and by the agent itself once the bff enum
// unlocks the actions).

func TestMouseButtonArg(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		want    string
		wantErr bool
	}{
		{"absent defaults to left", map[string]any{}, "left", false},
		{"left", map[string]any{"button": "left"}, "left", false},
		{"right", map[string]any{"button": "right"}, "right", false},
		{"middle maps to robotgo center", map[string]any{"button": "middle"}, "center", false},
		{"center accepted as alias", map[string]any{"button": "center"}, "center", false},
		{"case + whitespace tolerant", map[string]any{"button": " Right "}, "right", false},
		// robotgo's CheckMouse silently falls back to LEFT_BUTTON on
		// unknown names -- we must reject instead of misclicking.
		{"unknown button rejected", map[string]any{"button": "back"}, "", true},
		{"wheel buttons rejected", map[string]any{"button": "wheelDown"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fail := mouseButtonArg(tc.args, "mouse_down")
			if tc.wantErr {
				if fail == nil || fail.GetErrorCode() != "bad_request" {
					t.Fatalf("expected bad_request; got button=%q fail=%+v", got, fail)
				}
				return
			}
			if fail != nil {
				t.Fatalf("unexpected failure: %+v", fail)
			}
			if got != tc.want {
				t.Errorf("button = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMouseClickModifiers covers the pure (pre-RobotGo) validation of
// the mouse_click `modifiers` arg (memql-cockpit#184): the schema
// vocabulary + aliases canonicalize to RobotGo key names, order is
// preserved, duplicates collapse, and an unknown modifier is a
// structured bad_request rather than a silent no-op.
func TestMouseClickModifiers(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		want    []string
		wantErr bool
	}{
		{"absent yields none", map[string]any{}, nil, false},
		{"empty list yields none", map[string]any{"modifiers": []any{}}, nil, false},
		{"single cmd", map[string]any{"modifiers": []any{"cmd"}}, []string{"cmd"}, false},
		{"schema vocabulary", map[string]any{"modifiers": []any{"cmd", "shift", "alt", "ctrl"}},
			[]string{"cmd", "shift", "alt", "ctrl"}, false},
		{"aliases canonicalize", map[string]any{"modifiers": []any{"command", "option", "control"}},
			[]string{"cmd", "alt", "ctrl"}, false},
		{"case + whitespace tolerant", map[string]any{"modifiers": []any{" Cmd ", "SHIFT"}},
			[]string{"cmd", "shift"}, false},
		{"duplicates collapse, order preserved", map[string]any{"modifiers": []any{"shift", "cmd", "shift"}},
			[]string{"shift", "cmd"}, false},
		{"unknown rejected", map[string]any{"modifiers": []any{"hyper"}}, nil, true},
		{"one bad among good rejected", map[string]any{"modifiers": []any{"cmd", "fn"}}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fail := mouseClickModifiers(tc.args)
			if tc.wantErr {
				if fail == nil || fail.GetErrorCode() != "bad_request" {
					t.Fatalf("expected bad_request; got mods=%v fail=%+v", got, fail)
				}
				return
			}
			if fail != nil {
				t.Fatalf("unexpected failure: %+v", fail)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("modifiers = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("modifiers[%d] = %q, want %q (full %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

func TestGuiKeyHold_RequiresKey(t *testing.T) {
	success, fail := guiKeyHold(map[string]any{"durationMs": float64(10)})
	if success != nil {
		t.Fatalf("key_hold without a key must not succeed; got %+v", success)
	}
	if fail == nil || fail.GetErrorCode() != "bad_request" {
		t.Fatalf("expected bad_request; got %+v", fail)
	}
	if !strings.Contains(fail.GetErrorMessage(), "key required") {
		t.Errorf("message should say the key is required; got %q", fail.GetErrorMessage())
	}
}

func TestGuiMouseDownUp_RejectBadButton(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(map[string]any) (errCode string)
	}{
		{"mouse_down", func(args map[string]any) string {
			_, fail := guiMouseDown(args)
			if fail == nil {
				return ""
			}
			return fail.GetErrorCode()
		}},
		{"mouse_up", func(args map[string]any) string {
			_, fail := guiMouseUp(args)
			if fail == nil {
				return ""
			}
			return fail.GetErrorCode()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := tc.call(map[string]any{"button": "nope"}); code != "bad_request" {
				t.Errorf("%s with an unknown button must be bad_request; got %q", tc.name, code)
			}
		})
	}
}

// Live multi-display tests (memql-cockpit#165). These exercise the
// RobotGo-backed enumeration so they only run on computeruse-tagged builds
// with a reachable display; the geometry/bounds logic itself is
// covered headlessly in coords_test.go.

// TestEnumerateDisplays pins the enumeration invariants: never
// empty, display 0 present and primary, every rect non-degenerate,
// ids unique, and a positive scale everywhere.
func TestEnumerateDisplays(t *testing.T) {
	displays := enumerateDisplays()
	if len(displays) == 0 {
		t.Fatal("enumerateDisplays must never return an empty list")
	}
	seen := map[int]bool{}
	primaries := 0
	for _, d := range displays {
		if seen[d.id] {
			t.Errorf("display id %d enumerated twice", d.id)
		}
		seen[d.id] = true
		if d.w <= 0 || d.h <= 0 {
			t.Errorf("display %d has degenerate size %dx%d", d.id, d.w, d.h)
		}
		if d.scale <= 0 {
			t.Errorf("display %d has non-positive scale %f", d.id, d.scale)
		}
		if d.primary {
			primaries++
			if d.id != 0 {
				t.Errorf("primary display must be id 0; got %d", d.id)
			}
			if d.x != 0 || d.y != 0 {
				t.Errorf("primary display must sit at the virtual-desktop origin; got (%d,%d)", d.x, d.y)
			}
		}
	}
	if primaries != 1 {
		t.Errorf("exactly one display must be primary; got %d", primaries)
	}
}

// TestMapperForDisplay pins the routing: display 0 is the exact
// liveMapper geometry (zero origin -- single-display behavior
// unchanged), unknown ids are a structured bad_request, and any
// enumerated secondary gets an origin-offset logical==captured
// mapper.
func TestMapperForDisplay(t *testing.T) {
	displays := enumerateDisplays()

	m, fail := mapperForDisplay(displays, 0)
	if fail != nil {
		t.Fatalf("display 0 must always resolve; got %s: %s", fail.GetErrorCode(), fail.GetErrorMessage())
	}
	if m != liveMapper() {
		t.Errorf("display 0 mapper must equal liveMapper(); got %+v vs %+v", m, liveMapper())
	}
	if m.OriginX != 0 || m.OriginY != 0 {
		t.Errorf("display 0 mapper must have a zero origin; got (%d,%d)", m.OriginX, m.OriginY)
	}

	if _, fail := mapperForDisplay(displays, 9999); fail == nil {
		t.Error("unknown display id must fail")
	} else if fail.GetErrorCode() != "bad_request" {
		t.Errorf("unknown display id error code = %q, want bad_request", fail.GetErrorCode())
	}

	for _, d := range displays {
		if d.id == 0 {
			continue
		}
		m, fail := mapperForDisplay(displays, d.id)
		if fail != nil {
			t.Fatalf("enumerated display %d must resolve; got %s", d.id, fail.GetErrorMessage())
		}
		if m.OriginX != d.x || m.OriginY != d.y {
			t.Errorf("display %d mapper origin = (%d,%d), want (%d,%d)", d.id, m.OriginX, m.OriginY, d.x, d.y)
		}
		if m.LogicalW != d.w || m.LogicalH != d.h {
			t.Errorf("display %d mapper logical = %dx%d, want %dx%d", d.id, m.LogicalW, m.LogicalH, d.w, d.h)
		}
		if m.CapturedW != d.w || m.CapturedH != d.h {
			t.Errorf("display %d capture path is logical resolution; captured = %dx%d, want %dx%d",
				d.id, m.CapturedW, m.CapturedH, d.w, d.h)
		}
	}
}

// TestGuiDisplayInfoWireShape pins the display_info payload
// (memql-cockpit#165): the legacy top-level width/height stay, plus
// a displays array of {id, x, y, width, height, scale, primary} and
// a top-level primary id that names a display marked primary.
func TestGuiDisplayInfoWireShape(t *testing.T) {
	success, fail := guiDisplayInfo(nil)
	if fail != nil {
		t.Fatalf("display_info failed: %s: %s", fail.GetErrorCode(), fail.GetErrorMessage())
	}
	var payload struct {
		Width    int `json:"width"`
		Height   int `json:"height"`
		Primary  int `json:"primary"`
		Displays []struct {
			ID      int     `json:"id"`
			X       int     `json:"x"`
			Y       int     `json:"y"`
			Width   int     `json:"width"`
			Height  int     `json:"height"`
			Scale   float64 `json:"scale"`
			Primary bool    `json:"primary"`
		} `json:"displays"`
	}
	if err := json.Unmarshal(success.GetResultJson(), &payload); err != nil {
		t.Fatalf("display_info payload is not the expected shape: %v (raw: %s)", err, success.GetResultJson())
	}
	if payload.Width <= 0 || payload.Height <= 0 {
		t.Errorf("legacy top-level width/height must stay positive; got %dx%d", payload.Width, payload.Height)
	}
	if len(payload.Displays) == 0 {
		t.Fatal("displays array must never be empty")
	}
	foundPrimary := false
	for _, d := range payload.Displays {
		if d.Width <= 0 || d.Height <= 0 {
			t.Errorf("display %d has degenerate size %dx%d", d.ID, d.Width, d.Height)
		}
		if d.Scale <= 0 {
			t.Errorf("display %d has non-positive scale %f", d.ID, d.Scale)
		}
		if d.ID == payload.Primary {
			foundPrimary = true
			if !d.Primary {
				t.Errorf("top-level primary %d must point at a display marked primary", payload.Primary)
			}
		}
	}
	if !foundPrimary {
		t.Errorf("top-level primary %d is not in the displays array", payload.Primary)
	}
}
