// Package num holds the narrowing from a decoded payload number to a Go
// `int`, in the three answers this repository actually needs.
//
// ===========================================================================
// WHY THIS EXISTS
// ===========================================================================
// Every construct in this tree that reads a JSON-ish payload field into an
// `int` is the same small function -- a type switch over `any` with a
// `float64` arm and an `int64` arm -- and each one of them has to decide what
// a value too large for an `int` means. Before this package, seven of them
// had written their own answer and they gave three different ones, because
// each was written in a separate round of fixing the same CodeQL alert
// (`go/incorrect-integer-conversion`) and no round asked what else looked
// like it. See docs/internal/ops/codeql-alert-triage.md, "what recurs is not
// the bug, it is the SWEEP".
//
// A bare `int(x)` from a `float64` is not merely wrong on a 32-bit build. Go
// leaves the out-of-range conversion IMPLEMENTATION-DEFINED, and on amd64 the
// hardware answers with the integer indefinite value: measured in memql#4778,
// `int(1e30)` is -9223372036854775808. A count becomes hugely negative, which
// inverts every `> 0` guard and every ordering the field exists to express.
//
// ===========================================================================
// THE THREE ANSWERS ARE ALL CORRECT, AND THE CHOICE IS THE CALLER'S
// ===========================================================================
// Forcing one semantic on every site would silently change behaviour at four
// of the seven, so the answer is NAMED rather than assumed.
//
// Clamp* SATURATES at the platform bounds. It is the right answer when the
// field is an ORDERING or a magnitude -- a rank, a count, a retry ordinal --
// because saturation is the only one of the three that preserves the order. A
// role declaring 2^63 skills must present as the LEAST restrictive in the
// catalog, not the most (memql#4778).
//
// *OrZero returns 0. It is the right answer where callers already read 0 as
// "unset" through their own `> 0` guards, which is what integrations/dailyspace
// documented and deliberately chose.
//
// *Or returns the CALLER'S DEFAULT. It is the right answer where the site
// already has one -- an embedding dimensionality, a chunk size, a read cap --
// because saturating those produces a value the next line cannot use: MaxInt
// fed to make([]byte, n+1) panics, and fed to a SQL LIMIT removes the limit.
//
// ===========================================================================
// THE FLOAT COMPARISON CANNOT BE EXACT, AND THAT IS THE WHOLE TRICK
// ===========================================================================
// float64 has no representation of math.MaxInt (2^63-1) and rounds it UP to
// 2^63, so a value in that gap PASSES a `v > math.MaxInt` guard and then
// converts anyway. Every float entry point below therefore finishes in the
// integer domain, where the bound is exact -- which is the step that is
// actually provable, to a reader and to a static analyser alike.
//
// ===========================================================================
// THE BOUND IS THE PLATFORM'S, NOT int32
// ===========================================================================
// Three of the collapsed helpers bounded at math.MaxInt32 and said they did
// so because it is "the worst-case int width" -- a portability proxy, not a
// domain rule. On every platform this repository builds for, int is 64 bits,
// so the int32 bound does not protect anything and DOES silently truncate a
// legitimate value: a 5 GB file size read through it became 2147483647. The
// exact platform bound is correct everywhere and lossy nowhere. A field that
// genuinely must fit in int32 has a DOMAIN rule, and a domain rule belongs at
// the call site that knows why, not hidden inside a narrowing.
package num

import "math"

// ClampInt64 narrows an int64 to int, saturating at the platform int bounds.
// Exact on a 64-bit build, where int is already 64 bits; saturating on a
// 32-bit one.
func ClampInt64(v int64) int {
	if v > math.MaxInt {
		return math.MaxInt
	}
	if v < math.MinInt {
		return math.MinInt
	}
	return int(v)
}

// ClampFloat64 narrows a float64 to int, saturating at the platform int
// bounds and truncating toward zero.
//
// NaN has no ordering and so no clamp: it becomes the same zero an absent
// field does. That is deliberate rather than an oversight -- there is no
// saturation direction to pick for a value that compares false against every
// bound, and a caller that needs to TELL an absent field from a NaN one is
// asking a question this signature cannot answer and should use Float64Or.
func ClampFloat64(v float64) int {
	switch {
	case math.IsNaN(v):
		return 0
	case v >= math.MaxInt:
		return math.MaxInt
	case v <= math.MinInt:
		return math.MinInt
	}
	return ClampInt64(int64(v))
}

// Int64OrZero narrows an int64 to int, returning 0 when the value does not
// fit. For callers whose own `> 0` guards already read 0 as "unset", so an
// out-of-range value lands on the same path an absent field does.
func Int64OrZero(v int64) int {
	if v > math.MaxInt || v < math.MinInt {
		return 0
	}
	return int(v)
}

// Float64OrZero narrows a float64 to int, returning 0 when the value does not
// fit or is NaN.
func Float64OrZero(v float64) int {
	if math.IsNaN(v) || v >= math.MaxInt || v <= math.MinInt {
		return 0
	}
	return Int64OrZero(int64(v))
}

// Int64Or narrows an int64 to int, returning def when the value does not fit.
func Int64Or(v int64, def int) int {
	if v > math.MaxInt || v < math.MinInt {
		return def
	}
	return int(v)
}

// Float64Or narrows a float64 to int, returning def when the value does not
// fit or is NaN.
func Float64Or(v float64, def int) int {
	if math.IsNaN(v) || v >= math.MaxInt || v <= math.MinInt {
		return def
	}
	return Int64Or(int64(v), def)
}

// ---------------------------------------------------------------------------
// float64 -> int64
// ---------------------------------------------------------------------------
//
// The conversion above stops at `int`, and that is not where the class stops.
// float64 -> int64 IS the implementation-defined conversion -- it is where the
// integer indefinite value comes from -- so a 64-bit build does not make it
// safe, it makes it the only width that matters.

// ClampFloat64ToInt64 narrows a float64 to int64, saturating at the int64
// bounds and truncating toward zero. NaN answers 0, for the reason
// ClampFloat64 states.
func ClampFloat64ToInt64(v float64) int64 {
	switch {
	case math.IsNaN(v):
		return 0
	case v >= math.MaxInt64:
		return math.MaxInt64
	case v <= math.MinInt64:
		return math.MinInt64
	}
	return int64(v)
}

// WholeInt64 reports whether v is a whole number that an int64 can hold, and
// its value when it is.
//
// This replaces `float64(int64(v)) == v`, which seven files in this tree wrote
// as their integrality test. That expression's result is UNDEFINED for a v
// outside int64: the inner conversion is implementation-defined, so the
// comparison is asking whether an arbitrary value equals the original. On
// amd64 it happens to answer "not whole", which is the safe direction and is
// why nobody noticed -- but it is undefined rather than merely surprising, and
// math.Trunc is exact, total, and converts nothing.
func WholeInt64(v float64) (int64, bool) {
	if math.IsNaN(v) || v != math.Trunc(v) {
		return 0, false
	}
	// float64(math.MaxInt64) rounds UP to 2^63, so `>=` excludes it and
	// everything above; float64(math.MinInt64) is exactly -2^63, which int64
	// does hold, so the low bound is strict.
	if v >= math.MaxInt64 || v < math.MinInt64 {
		return 0, false
	}
	return int64(v), true
}
