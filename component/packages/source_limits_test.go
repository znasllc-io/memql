package packages

import (
	"strconv"
	"testing"
)

// DefaultLimits' stated contract, which had no test.
//
// The paragraph above DefaultLimits says a set-but-unparseable or non-positive
// value falls back to the DEFAULT rather than to "no limit", because an
// unbounded expansion is the one outcome a misconfigured cap must never
// produce. That is the property the whole extraction path's safety rests on,
// and nothing asserted it -- DefaultLimits appeared in this package only as a
// test fixture.
//
// It is also what decides the narrowing answer for MaxFileCount. `int(...)` on
// an int64 is implementation-defined out of range (CodeQL
// go/incorrect-integer-conversion, alert #1082); `num.ClampInt64` would
// saturate, which on a 32-bit build turns a too-large cap into math.MaxInt32 --
// "no limit" wearing a number, and the exact outcome this paragraph forbids.
// `num.Int64Or` falls back to the default instead, which is what a
// misconfigured cap gets everywhere else here.
func TestDefaultLimitsFallBackToTheDefaultAndNeverToNoLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  bool
		raw  string
	}{
		{"unset", false, ""},
		{"empty", true, ""},
		{"whitespace", true, "   "},
		{"unparseable", true, "lots"},
		{"zero", true, "0"},
		{"negative", true, "-1"},
		{"float", true, "10.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(MaxFileCountEnv, tc.raw)
				t.Setenv(MaxSourceBytesEnv, tc.raw)
				t.Setenv(MaxFileBytesEnv, tc.raw)
			}
			got := DefaultLimits()
			if got.MaxFileCount != DefaultMaxFileCount {
				t.Errorf("MaxFileCount = %d, want the default %d -- a misconfigured cap must never widen one",
					got.MaxFileCount, DefaultMaxFileCount)
			}
			if got.MaxSourceBytes != DefaultMaxSourceBytes {
				t.Errorf("MaxSourceBytes = %d, want the default %d", got.MaxSourceBytes, DefaultMaxSourceBytes)
			}
			if got.MaxFileBytes != DefaultMaxFileBytes {
				t.Errorf("MaxFileBytes = %d, want the default %d", got.MaxFileBytes, DefaultMaxFileBytes)
			}
		})
	}
}

// The negative control. Without it every assertion above would pass against a
// DefaultLimits that ignored the environment entirely and always returned the
// defaults -- which is a different function with the same test.
func TestDefaultLimitsHonoursAValidValue(t *testing.T) {
	t.Setenv(MaxFileCountEnv, "7")
	t.Setenv(MaxSourceBytesEnv, "1234")
	t.Setenv(MaxFileBytesEnv, "567")
	got := DefaultLimits()
	if got.MaxFileCount != 7 {
		t.Errorf("MaxFileCount = %d, want 7 -- the env var is read at all", got.MaxFileCount)
	}
	if got.MaxSourceBytes != 1234 {
		t.Errorf("MaxSourceBytes = %d, want 1234", got.MaxSourceBytes)
	}
	if got.MaxFileBytes != 567 {
		t.Errorf("MaxFileBytes = %d, want 567", got.MaxFileBytes)
	}
}

// A count larger than an int can hold is a misconfigured cap, not a bigger
// one, and it takes the same answer as "lots" does.
//
// On a 64-bit build every int64 fits an int, so this passes trivially here and
// is a real assertion only on a 32-bit one. It is written anyway because the
// value it pins is a CHOICE -- Int64Or over ClampInt64 -- and the next person
// to touch this line should find the choice asserted rather than inferred from
// a comment.
func TestAFileCountThatCannotFitTakesTheDefault(t *testing.T) {
	t.Setenv(MaxFileCountEnv, strconv.FormatInt(1<<62, 10))
	got := DefaultLimits().MaxFileCount
	if strconv.IntSize == 64 {
		if int64(got) != 1<<62 {
			t.Errorf("MaxFileCount = %d; on a 64-bit build 1<<62 fits an int and must pass through", got)
		}
		return
	}
	if got != DefaultMaxFileCount {
		t.Errorf("MaxFileCount = %d, want the default %d -- a value that will not fit must not saturate into an effectively unlimited cap",
			got, DefaultMaxFileCount)
	}
}
