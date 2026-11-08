package statsperiod

import (
	"strings"
	"testing"
	"time"
)

// at is the instant the bounds table is read at: a Sunday morning in UTC,
// well inside an hour, a day and a month, so no case sits on a boundary.
var at = time.Date(2026, 11, 8, 10, 17, 0, 0, time.UTC)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// Every period's bounds at one instant, and the period each is compared with.
func TestBoundsForEveryPeriod(t *testing.T) {
	cases := []struct {
		period             Period
		start, end         time.Time
		prevStart, prevEnd time.Time
		bucket             string
		buckets            int
		hasPrevious        bool
	}{
		{
			period:    Last24Hours,
			start:     time.Date(2026, 11, 7, 11, 0, 0, 0, time.UTC),
			end:       time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC),
			prevStart: time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC),
			prevEnd:   time.Date(2026, 11, 7, 11, 0, 0, 0, time.UTC),
			bucket:    BucketHour, buckets: 24, hasPrevious: true,
		},
		{
			period: Last7Days, start: day(2026, 11, 2), end: day(2026, 11, 9),
			prevStart: day(2026, 10, 26), prevEnd: day(2026, 11, 2),
			bucket: BucketDay, buckets: 7, hasPrevious: true,
		},
		{
			period: Last30Days, start: day(2026, 10, 10), end: day(2026, 11, 9),
			prevStart: day(2026, 9, 10), prevEnd: day(2026, 10, 10),
			bucket: BucketDay, buckets: 30, hasPrevious: true,
		},
		{
			period: Last90Days, start: day(2026, 8, 11), end: day(2026, 11, 9),
			prevStart: day(2026, 5, 13), prevEnd: day(2026, 8, 11),
			bucket: BucketDay, buckets: 90, hasPrevious: true,
		},
		{
			// The current month and the twelve before it, and NO previous
			// period: the twelve months before these are past the daily
			// table's 400 days (spec D4).
			period: Last12Months, start: day(2025, 11, 1), end: day(2026, 12, 1),
			bucket: BucketMonth, buckets: 13, hasPrevious: false,
		},
	}
	for _, c := range cases {
		t.Run(string(c.period), func(t *testing.T) {
			b := For(c.period, at)
			if !b.Start.Equal(c.start) || !b.End.Equal(c.end) {
				t.Errorf("[%s, %s), want [%s, %s)", b.Start, b.End, c.start, c.end)
			}
			if !b.PrevStart.Equal(c.prevStart) || !b.PrevEnd.Equal(c.prevEnd) {
				t.Errorf("previous [%s, %s), want [%s, %s)", b.PrevStart, b.PrevEnd, c.prevStart, c.prevEnd)
			}
			if b.Bucket != c.bucket || b.Buckets != c.buckets {
				t.Errorf("%d %s buckets, want %d %s", b.Buckets, b.Bucket, c.buckets, c.bucket)
			}
			if b.HasPrevious != c.hasPrevious {
				t.Errorf("HasPrevious = %v, want %v", b.HasPrevious, c.hasPrevious)
			}
			if c.hasPrevious && b.PrevEnd.Sub(b.PrevStart) != b.End.Sub(b.Start) {
				t.Errorf("the previous period lasts %s, want the period's own %s", b.PrevEnd.Sub(b.PrevStart), b.End.Sub(b.Start))
			}
			if !c.hasPrevious && (!b.PrevStart.IsZero() || !b.PrevEnd.IsZero()) {
				t.Errorf("previous [%s, %s), want zero times when there is no previous period", b.PrevStart, b.PrevEnd)
			}
		})
	}
}

// The bucket under way is part of the period, whatever the instant inside it:
// a read on the hour, at midnight or on the first of a month still ends after
// the bucket that has just begun, and one a moment before a boundary ends on it.
func TestTheBucketUnderWayIsIncludedAtEveryBoundary(t *testing.T) {
	cases := []struct {
		name   string
		period Period
		now    time.Time
		end    time.Time
	}{
		{"24h on the hour", Last24Hours, time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC), time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC)},
		{"24h just before the hour", Last24Hours, time.Date(2026, 11, 8, 10, 59, 59, 999_999_999, time.UTC), time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC)},
		{"7d at midnight", Last7Days, day(2026, 11, 8), day(2026, 11, 9)},
		{"7d just before midnight", Last7Days, time.Date(2026, 11, 8, 23, 59, 59, 999_999_999, time.UTC), day(2026, 11, 9)},
		{"12m on the first", Last12Months, day(2026, 11, 1), day(2026, 12, 1)},
		{"12m on the last moment of the year", Last12Months, time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), day(2027, 1, 1)},
	}
	for _, c := range cases {
		if got := For(c.period, c.now).End; !got.Equal(c.end) {
			t.Errorf("%s: End = %s, want %s", c.name, got, c.end)
		}
	}
}

// Twelve months read in January reach back into the year before last.
func TestTwelveMonthsAcrossTheYearEnd(t *testing.T) {
	b := For(Last12Months, time.Date(2027, 1, 15, 8, 0, 0, 0, time.UTC))
	if !b.Start.Equal(day(2026, 1, 1)) || !b.End.Equal(day(2027, 2, 1)) {
		t.Errorf("[%s, %s), want [2026-01-01, 2027-02-01)", b.Start, b.End)
	}
	starts := b.Starts()
	if len(starts) != 13 || !starts[0].Equal(day(2026, 1, 1)) || !starts[12].Equal(day(2027, 1, 1)) {
		t.Errorf("starts = %v, want the thirteen firsts of January 2026 to January 2027", starts)
	}
}

// Days are UTC whatever zone the instant arrives in (spec D5): the same
// instant read in another zone gives the same bounds.
func TestBoundsAreUTCWhateverTheZoneOfNow(t *testing.T) {
	zone := time.FixedZone("+05:30", 5*3600+1800)
	// 2026-11-08T23:47+05:30 is 18:17Z on the 8th, not a moment on the 9th.
	local := time.Date(2026, 11, 8, 23, 47, 0, 0, zone)
	for _, p := range All() {
		got, want := For(p, local), For(p, local.UTC())
		if !got.Start.Equal(want.Start) || !got.End.Equal(want.End) || got.Start.Location() != time.UTC {
			t.Errorf("%s: [%s, %s), want the UTC bounds [%s, %s)", p, got.Start, got.End, want.Start, want.End)
		}
	}
	if end := For(Last7Days, local).End; !end.Equal(day(2026, 11, 9)) {
		t.Errorf("7d End = %s, want the UTC midnight after 18:17Z, 2026-11-09", end)
	}
}

// One start per bucket, a bucket apart, and the previous period's aligned with
// them by offset so its line can be drawn under the current one.
func TestStartsAreOneBucketApartAndTheirPreviousIsAligned(t *testing.T) {
	for _, p := range All() {
		b := For(p, at)
		starts := b.Starts()
		if len(starts) != b.Buckets {
			t.Fatalf("%s: %d starts, want %d", p, len(starts), b.Buckets)
		}
		if !starts[0].Equal(b.Start) {
			t.Errorf("%s: first start %s, want Start %s", p, starts[0], b.Start)
		}
		for i := 1; i < len(starts); i++ {
			if !starts[i].After(starts[i-1]) {
				t.Errorf("%s: start %d (%s) is not after start %d (%s)", p, i, starts[i], i-1, starts[i-1])
			}
		}
		if last := b.Next(starts[len(starts)-1]); !last.Equal(b.End) {
			t.Errorf("%s: the last bucket ends at %s, want End %s", p, last, b.End)
		}
		prev := b.PreviousStarts()
		if !b.HasPrevious {
			if prev != nil {
				t.Errorf("%s: %d previous starts, want none without a previous period", p, len(prev))
			}
			continue
		}
		if len(prev) != len(starts) {
			t.Fatalf("%s: %d previous starts, want %d, one per bucket", p, len(prev), len(starts))
		}
		offset := b.Start.Sub(b.PrevStart)
		for i := range starts {
			if !prev[i].Add(offset).Equal(starts[i]) {
				t.Errorf("%s: previous start %d is %s, want %s, the same offset into its period", p, i, prev[i], starts[i].Add(-offset))
			}
		}
	}
}

func TestParseAcceptsEveryPeriodAndRefusesTheRest(t *testing.T) {
	for _, p := range All() {
		got, err := Parse(" " + string(p) + " ")
		if err != nil || got != p {
			t.Errorf("Parse(%q) = %q, %v; want %q", p, got, err, p)
		}
	}
	for _, bad := range []string{"", "1y", "24H", "7days", "12M"} {
		_, err := Parse(bad)
		if err == nil {
			t.Errorf("Parse(%q) accepted a period nobody offers", bad)
			continue
		}
		// The refusal names what would have been accepted, so the caller can
		// correct the call from the sentence alone.
		for _, p := range All() {
			if !strings.Contains(err.Error(), string(p)) {
				t.Errorf("Parse(%q): %q does not name %s", bad, err, p)
			}
		}
	}
}

// A read offers the periods it can answer and no others: email has no hourly
// figures, so its read refuses 24h by naming only the four it offers.
func TestParseKeepsToTheAllowedPeriods(t *testing.T) {
	email := []Period{Last7Days, Last30Days, Last90Days, Last12Months}
	if got, err := Parse("30d", email...); err != nil || got != Last30Days {
		t.Errorf("Parse(30d, email) = %q, %v; want 30d", got, err)
	}
	_, err := Parse("24h", email...)
	if err == nil {
		t.Fatal("Parse(24h, email) accepted a period email does not offer")
	}
	// The sentence echoes what it was given after "; got"; what comes before
	// it is the offer, and the offer must not include 24h.
	offer := err.Error()
	if i := strings.Index(offer, "; got"); i >= 0 {
		offer = offer[:i]
	}
	for _, p := range email {
		if !strings.Contains(offer, string(p)) {
			t.Errorf("the refusal %q does not offer %s", err, p)
		}
	}
	if strings.Contains(offer, "24h") {
		t.Errorf("the refusal %q offers 24h, which this read does not answer", err)
	}
	// An allowed list naming a value that is not a period cannot widen the
	// set: For has no bounds for it.
	if _, err := Parse("1y", Period("1y")); err == nil {
		t.Error("Parse accepted 1y because the caller listed it, but there is no such period")
	}
}

// A period Parse would never return has no bounds, rather than somebody
// else's: zero buckets, and no previous period.
func TestForAnUnknownPeriodIsEmpty(t *testing.T) {
	b := For(Period("1y"), at)
	if b.Buckets != 0 || !b.Start.IsZero() || !b.End.IsZero() || b.HasPrevious || b.Starts() != nil {
		t.Errorf("For(1y) = %+v, want empty bounds", b)
	}
}
