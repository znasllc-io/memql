package tools

import (
	"encoding/json"
	"math"
	"testing"
)

// These tests cover the build-agnostic window helpers (window.go):
// the wire shape window_list emits and the windowId argument
// parsing window_focus does. They run on the headless CI build AND
// the local computeruse-tagged build -- the platform listers themselves are
// exercised by the computeruse-tagged smoke test (window_gui_darwin_test.go).

func TestWindowListPayload_WireShape(t *testing.T) {
	wins := []WindowInfo{
		{ID: 42, Title: "inbox", App: "Mail", PID: 321, X: 10, Y: 20, Width: 800, Height: 600, Focused: true},
		{ID: 7, Title: "", App: "Terminal", PID: 99, X: -5, Y: 0, Width: 1, Height: 1},
	}
	raw, err := json.Marshal(windowListPayload(wins))
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var decoded struct {
		Windows []struct {
			ID      uint64 `json:"id"`
			Title   string `json:"title"`
			App     string `json:"app"`
			PID     int    `json:"pid"`
			X       int    `json:"x"`
			Y       int    `json:"y"`
			Width   int    `json:"width"`
			Height  int    `json:"height"`
			Focused bool   `json:"focused"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("payload does not round-trip the documented shape: %v (raw: %s)", err, raw)
	}
	if len(decoded.Windows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(decoded.Windows))
	}
	first := decoded.Windows[0]
	if first.ID != 42 || first.Title != "inbox" || first.App != "Mail" || first.PID != 321 ||
		first.X != 10 || first.Y != 20 || first.Width != 800 || first.Height != 600 || !first.Focused {
		t.Errorf("row 0 did not survive the mapping: %+v", first)
	}
	if decoded.Windows[1].Focused {
		t.Error("row 1 must keep focused=false")
	}

	// Every documented key must be present verbatim (consumers
	// pattern-match on them; absent != zero-value for JSON readers).
	var rawRows struct {
		Windows []map[string]json.RawMessage `json:"windows"`
	}
	if err := json.Unmarshal(raw, &rawRows); err != nil {
		t.Fatalf("re-decode raw rows: %v", err)
	}
	for _, key := range []string{"id", "title", "app", "pid", "x", "y", "width", "height", "focused"} {
		if _, ok := rawRows.Windows[0][key]; !ok {
			t.Errorf("row is missing documented key %q (raw: %s)", key, raw)
		}
	}
}

func TestWindowListPayload_EmptyMarshalsToArray(t *testing.T) {
	raw, err := json.Marshal(windowListPayload(nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(probe["windows"]) != "[]" {
		t.Errorf(`"windows" must marshal to [] (not null) for an empty list; got %s`, probe["windows"])
	}
}

func TestParseWindowID(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		want    uint64
		wantErr string // expected failure code; "" = success
	}{
		{"json number", map[string]any{"windowId": float64(42)}, 42, ""},
		{"json number zero", map[string]any{"windowId": float64(0)}, 0, ""},
		{"large id", map[string]any{"windowId": float64(67108870)}, 67108870, ""},
		{"largest represented unsigned float", map[string]any{"windowId": math.Nextafter(math.MaxUint64, 0)}, 18446744073709549568, ""},
		{"largest unsigned string", map[string]any{"windowId": "18446744073709551615"}, math.MaxUint64, ""},
		{"largest signed integer", map[string]any{"windowId": int64(math.MaxInt64)}, math.MaxInt64, ""},
		{"go int", map[string]any{"windowId": 7}, 7, ""},
		{"go int64", map[string]any{"windowId": int64(9)}, 9, ""},
		{"numeric string", map[string]any{"windowId": "1234"}, 1234, ""},
		{"numeric string with spaces", map[string]any{"windowId": " 1234 "}, 1234, ""},
		{"missing", map[string]any{}, 0, "bad_request"},
		{"negative number", map[string]any{"windowId": float64(-1)}, 0, "bad_request"},
		{"fractional number", map[string]any{"windowId": 1.5}, 0, "bad_request"},
		{"unsigned overflow", map[string]any{"windowId": float64(math.MaxUint64)}, 0, "bad_request"},
		{"huge number", map[string]any{"windowId": 1e30}, 0, "bad_request"},
		{"nan", map[string]any{"windowId": math.NaN()}, 0, "bad_request"},
		{"positive infinity", map[string]any{"windowId": math.Inf(1)}, 0, "bad_request"},
		{"negative infinity", map[string]any{"windowId": math.Inf(-1)}, 0, "bad_request"},
		{"negative int", map[string]any{"windowId": -3}, 0, "bad_request"},
		{"negative int64", map[string]any{"windowId": int64(-3)}, 0, "bad_request"},
		{"garbage string", map[string]any{"windowId": "front"}, 0, "bad_request"},
		{"negative string", map[string]any{"windowId": "-5"}, 0, "bad_request"},
		{"bool", map[string]any{"windowId": true}, 0, "bad_request"},
		{"object", map[string]any{"windowId": map[string]any{"id": 1}}, 0, "bad_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fail := parseWindowID(tc.args)
			if tc.wantErr == "" {
				if fail != nil {
					t.Fatalf("unexpected failure: %s: %s", fail.GetErrorCode(), fail.GetErrorMessage())
				}
				if got != tc.want {
					t.Errorf("parseWindowID = %d, want %d", got, tc.want)
				}
				return
			}
			if fail == nil {
				t.Fatalf("expected %s failure, got id %d", tc.wantErr, got)
			}
			if fail.GetErrorCode() != tc.wantErr {
				t.Errorf("failure code = %q, want %q (%s)", fail.GetErrorCode(), tc.wantErr, fail.GetErrorMessage())
			}
		})
	}
}
