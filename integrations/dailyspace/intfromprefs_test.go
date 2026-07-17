package dailyspace

import (
	"encoding/json"
	"math"
	"testing"
)

// intfromprefs_test.go covers intFromPrefs' int64->int narrowing, hardened
// against go/incorrect-integer-conversion (CodeQL alerts #411/#412). The
// overflow guard only fires on a 32-bit build (where int is narrower than
// int64); on the amd64/arm64 targets int == int64 and every in-range value
// must pass through unchanged.

func TestIntFromPrefs_ReadsEachNumericType(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want int
	}{
		{"int", 7, 7},
		{"int64", int64(30), 30},
		{"float64 (JSON default)", float64(14), 14},
		{"json.Number", json.Number("90"), 90},
		{"missing key", nil, 0},
		{"wrong type", "not-a-number", 0},
		{"zero", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prefs := map[string]any{}
			if c.val != nil {
				prefs["k"] = c.val
			}
			if got := intFromPrefs(prefs, "k"); got != c.want {
				t.Fatalf("intFromPrefs(%v) = %d, want %d", c.val, got, c.want)
			}
		})
	}
}

func TestClampInt64_GuardsNarrowing(t *testing.T) {
	// In-range values are returned verbatim; the out-of-range cases only
	// differ from int(v) on a 32-bit build, but the boundary values must
	// still round-trip on every platform.
	for _, v := range []int64{0, 1, -1, math.MaxInt32, math.MinInt32, math.MaxInt, math.MinInt} {
		if got := clampInt64(v); int64(got) != v {
			t.Fatalf("clampInt64(%d) = %d, want %d (in-range value must pass through)", v, got, v)
		}
	}
}
