package agents

import (
	"math"
	"testing"
)

// The narrowing in intFromAnyLoose was CodeQL alerts #1033 / #1034
// (go/incorrect-integer-conversion, memql#4777). These tests pin the
// saturation, and each one says which build it actually bites on -- on a
// 64-bit build `int` is already 64 bits, so an int64 that "saturates" is
// simply exact, and only the float64 arm can distinguish the fix from the
// bug it replaces.

// TestIntFromAnyLoose_FloatOutOfRangeSaturates is the reachable positive: it
// FAILS against the bare `int(x)` on every platform, 64-bit included.
//
// Go leaves an out-of-range float64 -> int conversion implementation-defined,
// and on amd64/arm64 the hardware answers with the "integer indefinite"
// value, math.MinInt64. So the old code turned a maxSkills of 1e300 into a
// large NEGATIVE cap -- not a clamped one, and not a zero either.
func TestIntFromAnyLoose_FloatOutOfRangeSaturates(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want int
	}{
		{"above int range", 1e300, math.MaxInt},
		{"below int range", -1e300, math.MinInt},
		{"positive infinity", math.Inf(1), math.MaxInt},
		{"negative infinity", math.Inf(-1), math.MinInt},
		// NaN has no ordering, so it has no clamp either: it reads as the
		// same "no usable value" the absent-field default reads as.
		{"NaN", math.NaN(), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := intFromAnyLoose(tc.in); got != tc.want {
				t.Errorf("intFromAnyLoose(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestIntFromAnyLoose_SaturationPreservesOrdering encodes the reason
// saturating beats truncating for this particular field. maxSkills is a cap
// handed to the agentFactoryAnalyze prompt, so what has to survive the
// coercion is the ORDER: a role that declares more skills must never read as
// more restrictive than one that declares fewer. A wrapping conversion
// inverts exactly that, which is the failure a magnitude-only assertion
// cannot see.
func TestIntFromAnyLoose_SaturationPreservesOrdering(t *testing.T) {
	ascending := []float64{0, 1, 8, 4096, 1 << 40, 1e30, 1e300}
	prev := intFromAnyLoose(ascending[0])
	for _, in := range ascending[1:] {
		got := intFromAnyLoose(in)
		if got < prev {
			t.Fatalf("ordering inverted: intFromAnyLoose(%v) = %d, below the previous %d", in, got, prev)
		}
		prev = got
	}
}

// TestIntFromAnyLoose_PassesInRangeValuesThrough guards the other direction:
// the clamp must not disturb the ordinary values, which are all this field
// ever holds in practice.
func TestIntFromAnyLoose_PassesInRangeValuesThrough(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"float64 (what JSON unmarshal produces)", float64(8), 8},
		{"int", int(8), 8},
		{"int64 (what a DSL integer literal decodes to)", int64(8), 8},
		{"int32", int32(8), 8},
		{"negative float64", float64(-3), -3},
		{"absent field", nil, 0},
		{"wrong type", "8", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := intFromAnyLoose(tc.in); got != tc.want {
				t.Errorf("intFromAnyLoose(%#v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestClampInt64ToInt_Saturates asserts the int64 arm at both extremes. The
// wanted value is math.MaxInt rather than math.MaxInt64 deliberately: on a
// 64-bit build the two are equal and the assertion is exact, and on a 32-bit
// one it is the saturation. The guard itself is unreachable on amd64/arm64 --
// this test proves the contract, not that the branch ran here.
func TestClampInt64ToInt_Saturates(t *testing.T) {
	if got := clampInt64ToInt(math.MaxInt64); got != math.MaxInt {
		t.Errorf("clampInt64ToInt(MaxInt64) = %d, want %d", got, math.MaxInt)
	}
	if got := clampInt64ToInt(math.MinInt64); got != math.MinInt {
		t.Errorf("clampInt64ToInt(MinInt64) = %d, want %d", got, math.MinInt)
	}
	if got := clampInt64ToInt(42); got != 42 {
		t.Errorf("clampInt64ToInt(42) = %d, want 42", got)
	}
}

// TestRoleSnapshotFromRow_ClampsMaxSkills runs the coercion through its one
// real caller, so a later "simplification" of the call site back to a bare
// conversion fails here and not only in the helper's own test.
func TestRoleSnapshotFromRow_ClampsMaxSkills(t *testing.T) {
	role, ok := roleSnapshotFromRow(map[string]any{
		"payload": map[string]any{
			"slug":      "accountant",
			"maxSkills": 1e300,
		},
	})
	if !ok {
		t.Fatal("roleSnapshotFromRow refused a row carrying a slug")
	}
	if role.MaxSkills != math.MaxInt {
		t.Errorf("MaxSkills = %d, want %d (a cap out of range must saturate, never wrap negative)", role.MaxSkills, math.MaxInt)
	}
}
