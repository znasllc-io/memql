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

// TestIntFromPrefsAnswersZeroOutOfRange pins THIS site's answer, which is the
// one that differs from the rest of the tree.
//
// Zero is deliberate here and documented on the decoder: every caller reads 0
// as "unset" through its own `> 0` guard, so an absurd preference lands on the
// same path an absent one does. Saturating instead would hand a `> 0` guard a
// true and run a sweep with a cap of 2^63.
func TestIntFromPrefsAnswersZeroOutOfRange(t *testing.T) {
	for _, v := range []any{1e30, -1e30, math.NaN()} {
		if got := intFromPrefs(map[string]any{"k": v}, "k"); got != 0 {
			t.Fatalf("intFromPrefs(%v) = %d, want 0", v, got)
		}
	}
	// The floor under that: ordinary values are untouched, or the assertion
	// above would pass on a decoder that had simply stopped working.
	for _, v := range []int64{0, 1, -1, math.MaxInt32, math.MinInt32} {
		if got := intFromPrefs(map[string]any{"k": v}, "k"); int64(got) != v {
			t.Fatalf("intFromPrefs(%d) = %d, want %d (in-range value must pass through)", v, got, v)
		}
	}
}
