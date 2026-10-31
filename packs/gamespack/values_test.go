package gamespack

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestVersionAfterIsStrictlyAfterWhatWasRead(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if got := versionAfter(time.Time{}, now); !got.Equal(now) {
		t.Fatalf("no prior version: got %v, want now", got)
	}
	if got := versionAfter(now.Add(-time.Second), now); !got.Equal(now) {
		t.Fatalf("an older prior: got %v, want now", got)
	}
	// A replica whose clock lags the one that wrote the prior version.
	ahead := now.Add(3 * time.Second)
	if got := versionAfter(ahead, now); !got.Equal(ahead.Add(time.Microsecond)) {
		t.Fatalf("a prior from the future: got %v, want one microsecond after it", got)
	}
}

func TestCanonicalComposesOnlyWhatIsBare(t *testing.T) {
	if got := canonical(conceptPlayer, "p-sam"); got != "v1:games:player:p-sam" {
		t.Fatalf("bare: %s", got)
	}
	if got := canonical(conceptPlayer, "v1:games:player:p-sam"); got != "v1:games:player:p-sam" {
		t.Fatalf("canonical: %s", got)
	}
	if got := canonical(conceptPlayer, ""); got != "" {
		t.Fatalf("empty: %q", got)
	}
}

// gamesRecall measures an episode's age from `at`, read back with timeOf from
// the one form it is stored in (episodeAtLayout: UTC, milliseconds always
// written), and from the asOf a caller sends in whichever spelling it likes.
func TestTimeOfReadsTheStoredEpisodeTimeAndTheSpellingsAsOfArrivesIn(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     time.Time
	}{
		{"the stored form, on the second", "2026-10-18T05:00:00.000Z", time.Date(2026, 10, 18, 5, 0, 0, 0, time.UTC)},
		{"the stored form, with milliseconds", "2026-10-18T06:00:00.123Z", time.Date(2026, 10, 18, 6, 0, 0, 123_000_000, time.UTC)},
		{"no fraction", "2026-10-18T07:00:00Z", time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC)},
		{"a trimmed fraction, as rfc3339 writes it", "2026-10-18T08:00:00.5Z", time.Date(2026, 10, 18, 8, 0, 0, 500_000_000, time.UTC)},
		{"an offset", "2026-10-18T10:00:00+05:30", time.Date(2026, 10, 18, 4, 30, 0, 0, time.UTC)},
	} {
		if got := timeOf(tc.in); !got.Equal(tc.want) {
			t.Errorf("%s: timeOf(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
	for _, unreadable := range []string{"", "yesterday", "2026-10-18 05:00:00"} {
		if got := timeOf(unreadable); !got.IsZero() {
			t.Errorf("timeOf(%q) = %v, want the zero time", unreadable, got)
		}
	}
}

// Whatever spelling an episode is recorded in, what is stored reads back as the
// same instant to the millisecond, so recall's age is the age of the moment the
// runtime named.
func TestTimeOfReadsWhatTheWritePathStores(t *testing.T) {
	for _, written := range []string{
		"2026-10-18T10:00:00+05:30", "2026-10-18T05:00:00Z", "2026-10-18T06:00:00.123456789123Z", "2026-10-18t07:00:00z",
	} {
		stored, ok := canonicalDateTime(written)
		if !ok {
			t.Fatalf("canonicalDateTime refused %q", written)
		}
		instant, err := time.Parse(time.RFC3339, strings.ToUpper(written))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := timeOf(stored), instant.UTC().Truncate(time.Millisecond); !got.Equal(want) {
			t.Errorf("%q is stored as %q, which timeOf reads as %v, want %v", written, stored, got, want)
		}
	}
}

// timeOf answers the zero time for what it cannot read, and the zero time is
// also the first instant of year 1; readTime says which of the two it was.
func TestReadTimeTellsTheFirstInstantOfYearOneFromWhatIsUnreadable(t *testing.T) {
	if got, ok := readTime("0001-01-01T00:00:00Z"); !ok || !got.IsZero() {
		t.Errorf("the first instant of year 1: got %v, %v, want the zero time and ok", got, ok)
	}
	if got, ok := readTime(time.Time{}); !ok || !got.IsZero() {
		t.Errorf("a zero time.Time: got %v, %v, want the zero time and ok", got, ok)
	}
	for _, unreadable := range []any{"", "yesterday", "2026-10-18 05:00:00", 12345, 12345.5, true, nil} {
		if got, ok := readTime(unreadable); ok || !got.IsZero() {
			t.Errorf("readTime(%#v) = %v, %v, want the zero time and not ok", unreadable, got, ok)
		}
	}
	want := time.Date(2026, 10, 18, 4, 30, 0, 0, time.UTC)
	for _, readable := range []any{"2026-10-18T10:00:00+05:30", " 2026-10-18T04:30:00.000Z ", want} {
		if got, ok := readTime(readable); !ok || !got.Equal(want) {
			t.Errorf("readTime(%#v) = %v, %v, want %v and ok", readable, got, ok, want)
		}
	}
}

// asFloat takes a finite number or answers its fallback, whichever way the number
// arrives. A json.Number is read by strconv.ParseFloat, which also reads the words
// NaN, Inf and Infinity without an error, so each spelling of one has to be
// refused here as the float64 branch refuses the value itself: a bound written
// for finite numbers is skipped by a value that is not one.
func TestAsFloatTakesNoNumberThatIsNotFinite(t *testing.T) {
	const fallback = 7.5
	for _, v := range []any{
		math.NaN(), math.Inf(1), math.Inf(-1),
		json.Number("NaN"), json.Number("nan"), json.Number("Inf"), json.Number("+Inf"), json.Number("-Inf"), json.Number("inf"),
		json.Number("Infinity"), json.Number("-Infinity"), json.Number("+infinity"),
		json.Number("1e999"), json.Number("-1e999"), json.Number("heavy"), json.Number(""),
		"1.5", true, nil, []any{1.0},
	} {
		if got := asFloat(v, fallback); got != fallback {
			t.Errorf("asFloat(%#v) = %v, want the fallback %v", v, got, fallback)
		}
	}
	for _, tc := range []struct {
		v    any
		want float64
	}{
		{1.5, 1.5}, {-72.0, -72}, {0.0, 0}, {3, 3}, {int64(-4), -4},
		{json.Number("1.5"), 1.5}, {json.Number("-72"), -72}, {json.Number("0"), 0}, {json.Number("1e308"), 1e308}, {json.Number("-1e-320"), -1e-320},
	} {
		if got := asFloat(tc.v, fallback); got != tc.want {
			t.Errorf("asFloat(%#v) = %v, want %v", tc.v, got, tc.want)
		}
	}
}
