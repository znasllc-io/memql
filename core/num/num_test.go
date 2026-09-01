package num

import (
	"math"
	"testing"
)

// The value the class exists to stop. Measured on amd64 in memql#4778: a bare
// int(1e30) answers with the integer indefinite value, so a count reads as
// hugely NEGATIVE and every `> 0` guard downstream inverts.
const indefinite = math.MinInt64

func TestBareConversionIsStillTheDefectThisPackageReplaces(t *testing.T) {
	// The reachable positive. Without this the whole file could be asserting
	// against a hazard the compiler had quietly stopped producing, and every
	// case below would pass while measuring nothing.
	//
	// THROUGH A VARIABLE, and that is not a stylistic detail: `int(1e30)`
	// written as a constant is a COMPILE error ("cannot convert"), because a
	// constant conversion must be representable. The defect only exists at
	// runtime, which is precisely why the compiler never catches it and a
	// taint-driven scanner had to.
	huge := 1e30
	got := int(huge)
	if got >= 0 {
		t.Fatalf("int(1e30) = %d, which is not negative -- this platform does not "+
			"reproduce the defect, so the assertions below prove nothing about it", got)
	}
	if got != indefinite {
		t.Logf("int(1e30) = %d (expected the amd64 answer %d); the direction is what "+
			"matters and it still holds", got, indefinite)
	}
}

func TestClampSaturatesAndKeepsTheOrder(t *testing.T) {
	if got := ClampFloat64(1e30); got != math.MaxInt {
		t.Errorf("ClampFloat64(1e30) = %d, want %d", got, math.MaxInt)
	}
	if got := ClampFloat64(-1e30); got != math.MinInt {
		t.Errorf("ClampFloat64(-1e30) = %d, want %d", got, math.MinInt)
	}
	// The property the answer exists for: a bigger input never produces a
	// smaller output. This is what a rank, a retry ordinal and a skill cap all
	// depend on, and it is exactly what the bare conversion destroys.
	if ClampFloat64(1e30) <= ClampFloat64(10) {
		t.Error("saturation inverted the order between 1e30 and 10")
	}
	if ClampInt64(math.MaxInt64) < ClampInt64(10) {
		t.Error("saturation inverted the order between MaxInt64 and 10")
	}
}

func TestClampFloat64HandlesTheInexactBound(t *testing.T) {
	// float64 rounds math.MaxInt UP to 2^63, so a guard written as
	// `v > math.MaxInt` lets 2^63 through and then converts it. These two
	// values sit either side of that gap.
	if got := ClampFloat64(math.Pow(2, 63)); got != math.MaxInt {
		t.Errorf("ClampFloat64(2^63) = %d, want %d -- the value at the rounded "+
			"bound is the one an inexact comparison lets through", got, math.MaxInt)
	}
	// The largest float64 strictly below 2^63 is 2^63 - 1024, and it must
	// convert exactly rather than saturating.
	near := math.Nextafter(math.Pow(2, 63), 0)
	if got := ClampFloat64(near); got != int(int64(near)) {
		t.Errorf("ClampFloat64(%v) = %d, want the exact conversion %d", near, got, int(int64(near)))
	}
	if got := ClampFloat64(-math.Pow(2, 63)); got != math.MinInt {
		t.Errorf("ClampFloat64(-2^63) = %d, want %d", got, math.MinInt)
	}
}

func TestNaNIsZeroInEveryAnswerThatCannotAskTheCaller(t *testing.T) {
	if got := ClampFloat64(math.NaN()); got != 0 {
		t.Errorf("ClampFloat64(NaN) = %d, want 0 -- NaN has no ordering and so no "+
			"saturation direction", got)
	}
	if got := Float64OrZero(math.NaN()); got != 0 {
		t.Errorf("Float64OrZero(NaN) = %d, want 0", got)
	}
	if got := Float64Or(math.NaN(), 7); got != 7 {
		t.Errorf("Float64Or(NaN, 7) = %d, want 7 -- a caller with a default gets it", got)
	}
}

func TestOrZeroLandsOnTheUnsetPath(t *testing.T) {
	if got := Float64OrZero(1e30); got != 0 {
		t.Errorf("Float64OrZero(1e30) = %d, want 0", got)
	}
	if got := Float64OrZero(-1e30); got != 0 {
		t.Errorf("Float64OrZero(-1e30) = %d, want 0", got)
	}
	if got := Int64OrZero(math.MaxInt64); got != math.MaxInt64 && math.MaxInt == math.MaxInt64 {
		t.Errorf("Int64OrZero(MaxInt64) = %d; on a 64-bit build it fits and must pass through", got)
	}
}

func TestOrReturnsTheCallersDefault(t *testing.T) {
	if got := Float64Or(1e30, 512); got != 512 {
		t.Errorf("Float64Or(1e30, 512) = %d, want 512", got)
	}
	if got := Float64Or(-1e30, 512); got != 512 {
		t.Errorf("Float64Or(-1e30, 512) = %d, want 512", got)
	}
	if got := Float64Or(12, 512); got != 12 {
		t.Errorf("Float64Or(12, 512) = %d, want 12 -- an in-range value is not a default", got)
	}
}

func TestInRangeValuesArePassedThroughUnchanged(t *testing.T) {
	// The floor under every case above: if the helpers rounded, truncated the
	// wrong way or clamped ordinary values, the out-of-range assertions would
	// still pass and the package would still be wrong for every real call.
	for _, v := range []float64{0, 1, -1, 10, 4096, 1 << 40, -(1 << 40), 2.9, -2.9} {
		want := int(int64(v)) // truncation toward zero, the Go conversion's own rule
		if got := ClampFloat64(v); got != want {
			t.Errorf("ClampFloat64(%v) = %d, want %d", v, got, want)
		}
		if got := Float64OrZero(v); got != want {
			t.Errorf("Float64OrZero(%v) = %d, want %d", v, got, want)
		}
		if got := Float64Or(v, -999); got != want {
			t.Errorf("Float64Or(%v, -999) = %d, want %d", v, got, want)
		}
	}
	for _, v := range []int64{0, 1, -1, 10, math.MaxInt32, math.MinInt32, 1 << 40} {
		if got := ClampInt64(v); int64(got) != v {
			t.Errorf("ClampInt64(%d) = %d, want %d", v, got, v)
		}
		if got := Int64OrZero(v); int64(got) != v {
			t.Errorf("Int64OrZero(%d) = %d, want %d", v, got, v)
		}
		if got := Int64Or(v, -999); int64(got) != v {
			t.Errorf("Int64Or(%d, -999) = %d, want %d", v, got, v)
		}
	}
}

func TestTheBoundIsThePlatformsNotInt32(t *testing.T) {
	// The behaviour change this package makes deliberately, recorded as an
	// assertion: three of the collapsed helpers bounded at math.MaxInt32 and
	// truncated a legitimate 5 GB file size to 2147483647. On a 64-bit build
	// the value survives.
	const fiveGB = int64(5) << 30
	if math.MaxInt == math.MaxInt64 {
		if got := ClampInt64(fiveGB); int64(got) != fiveGB {
			t.Errorf("ClampInt64(5GiB) = %d, want %d -- an int32 bound is a portability "+
				"proxy, not a domain rule, and it loses real values", got, fiveGB)
		}
		if got := ClampFloat64(float64(fiveGB)); int64(got) != fiveGB {
			t.Errorf("ClampFloat64(5GiB) = %d, want %d", got, fiveGB)
		}
	}
}

func TestClampFloat64ToInt64(t *testing.T) {
	if got := ClampFloat64ToInt64(1e30); got != math.MaxInt64 {
		t.Errorf("ClampFloat64ToInt64(1e30) = %d, want %d", got, int64(math.MaxInt64))
	}
	if got := ClampFloat64ToInt64(-1e30); got != math.MinInt64 {
		t.Errorf("ClampFloat64ToInt64(-1e30) = %d, want %d", got, int64(math.MinInt64))
	}
	if got := ClampFloat64ToInt64(math.NaN()); got != 0 {
		t.Errorf("ClampFloat64ToInt64(NaN) = %d, want 0", got)
	}
	for _, v := range []float64{0, 1, -1, 2.9, -2.9, 1 << 40} {
		if got := ClampFloat64ToInt64(v); got != int64(v) {
			t.Errorf("ClampFloat64ToInt64(%v) = %d, want %d", v, got, int64(v))
		}
	}
}

func TestWholeInt64IsExactWhereTheOldTestWasUndefined(t *testing.T) {
	// The expression this replaces, on the value that makes it undefined. The
	// reachable positive for the whole function: if `float64(int64(v)) == v`
	// ever became well-defined, the case below would be asserting nothing.
	huge := 1e30
	if float64(int64(huge)) == huge {
		t.Fatal("float64(int64(1e30)) == 1e30 held; this platform does not reproduce " +
			"the undefined conversion, so the contrast below proves nothing")
	}
	// 1e30 IS a whole number. The old test answers "no" by accident of the
	// hardware; this one answers "no" because an int64 cannot hold it, which
	// is the question the callers are actually asking.
	if _, ok := WholeInt64(huge); ok {
		t.Error("WholeInt64(1e30) reported a usable int64")
	}
	for _, v := range []float64{0, 1, -1, 1e15, -1e15, math.MinInt64} {
		got, ok := WholeInt64(v)
		if !ok || got != int64(v) {
			t.Errorf("WholeInt64(%v) = (%d, %t), want (%d, true)", v, got, ok, int64(v))
		}
	}
	for _, v := range []float64{0.5, -0.5, 2.9, math.NaN(), math.Inf(1), math.Inf(-1), 1e30} {
		if _, ok := WholeInt64(v); ok {
			t.Errorf("WholeInt64(%v) reported whole", v)
		}
	}
}
