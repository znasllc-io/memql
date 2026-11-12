package statsperiod

import (
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zone database travels with the test, so a runner without zone files still has the zones
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

// zone loads a named zone, failing with its name when the runner cannot: a nil
// zone is UTC, so a dropped error would show up as a wrong hour instead.
func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("zone %s cannot be loaded: %v", name, err)
	}
	return loc
}

// utc is an instant given in UTC, for comparing with Equal.
func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

// The expected values are what For answered at this instant before this change, written down as literals:
// a comparison of ForIn against For would compare a function with itself.
func TestForAndForInUTCAnswerWhatForAnswered(t *testing.T) {
	at := time.Date(2025, 11, 8, 10, 17, 42, 0, time.UTC)
	when := func(s string) time.Time {
		if s == "" {
			return time.Time{}
		}
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		p                              Period
		start, end, prevStart, prevEnd string
		bucket                         string
		buckets                        int
		hasPrevious                    bool
	}{
		{Last24Hours, "2025-11-07T11:00:00Z", "2025-11-08T11:00:00Z", "2025-11-06T11:00:00Z", "2025-11-07T11:00:00Z", BucketHour, 24, true},
		{Last7Days, "2025-11-02T00:00:00Z", "2025-11-09T00:00:00Z", "2025-10-26T00:00:00Z", "2025-11-02T00:00:00Z", BucketDay, 7, true},
		{Last30Days, "2025-10-10T00:00:00Z", "2025-11-09T00:00:00Z", "2025-09-10T00:00:00Z", "2025-10-10T00:00:00Z", BucketDay, 30, true},
		{Last90Days, "2025-08-11T00:00:00Z", "2025-11-09T00:00:00Z", "2025-05-13T00:00:00Z", "2025-08-11T00:00:00Z", BucketDay, 90, true},
		{Last12Months, "2024-11-01T00:00:00Z", "2025-12-01T00:00:00Z", "", "", BucketMonth, 13, false},
	} {
		for name, got := range map[string]Bounds{"For": For(c.p, at), "ForIn(UTC)": ForIn(c.p, at, time.UTC)} {
			if !got.Start.Equal(when(c.start)) || !got.End.Equal(when(c.end)) || !got.PrevStart.Equal(when(c.prevStart)) || !got.PrevEnd.Equal(when(c.prevEnd)) ||
				got.Bucket != c.bucket || got.Buckets != c.buckets || got.HasPrevious != c.hasPrevious || len(got.Starts()) != c.buckets {
				t.Errorf("%s(%s) = %+v", name, c.p, got)
			}
		}
	}
}

func TestASpringForwardDayHasTwentyThreeHoursAndAFallBackDayTwentyFive(t *testing.T) {
	madrid := zone(t, "Europe/Madrid")
	b := ForIn(Last7Days, time.Date(2025, 3, 30, 12, 0, 0, 0, madrid), madrid)
	spring := b.Starts()
	if d := spring[6].Sub(spring[5]); d != 24*time.Hour {
		t.Errorf("the 29th is %s long", d)
	}
	if d := b.End.Sub(spring[6]); d != 23*time.Hour {
		t.Errorf("the 30th is %s long, want 23h", d)
	}
	fall := ForIn(Last7Days, time.Date(2025, 10, 26, 12, 0, 0, 0, madrid), madrid)
	if d := fall.End.Sub(fall.Starts()[6]); d != 25*time.Hour {
		t.Errorf("the 26th is %s long, want 25h", d)
	}
}

func TestTheTwentyFourHoursOfAFallBackDayHoldTheRepeatedHourTwice(t *testing.T) {
	newYork := zone(t, "America/New_York")
	starts := ForIn(Last24Hours, time.Date(2025, 11, 2, 12, 30, 0, 0, newYork), newYork).Starts()
	if len(starts) != 24 {
		t.Fatalf("%d buckets", len(starts))
	}
	ones := 0
	for i, s := range starts {
		if i > 0 && s.Sub(starts[i-1]) != time.Hour {
			t.Errorf("bucket %d starts %s after the one before", i, s.Sub(starts[i-1]))
		}
		if s.In(newYork).Day() == 2 && s.In(newYork).Hour() == 1 {
			ones++
		}
	}
	if ones != 2 {
		t.Errorf("%d buckets start at 01:00 on the 2nd, want the hour before and the hour after the clocks went back", ones)
	}
}

func TestTwentyFourHoursInAHalfHourZoneStartOnItsOwnHour(t *testing.T) {
	kolkata := zone(t, "Asia/Kolkata")
	b := ForIn(Last24Hours, time.Date(2025, 11, 8, 10, 17, 0, 0, kolkata), kolkata)
	if want := time.Date(2025, 11, 8, 5, 30, 0, 0, time.UTC); !b.End.Equal(want) {
		t.Errorf("End = %s, want 11:00 +05:30", b.End)
	}
}

func TestADayWhoseMidnightTheClocksSkipStartsAtTheChange(t *testing.T) {
	santiago := zone(t, "America/Santiago")
	b := ForIn(Last7Days, time.Date(2023, 9, 3, 12, 0, 0, 0, santiago), santiago)
	day := b.Starts()[6]
	if want := time.Date(2023, 9, 3, 4, 0, 0, 0, time.UTC); !day.Equal(want) {
		t.Errorf("the 3rd starts %s, want 01:00 -03", day.UTC())
	}
	if d := b.End.Sub(day); d != 23*time.Hour {
		t.Errorf("the 3rd is %s long, want 23h", d)
	}
	if got := day.In(santiago).Day(); got != 3 {
		t.Errorf("the bucket's first instant is on day %d", got)
	}
	if got := StartOfDay(2023, 9, 3, santiago); !got.Equal(day) {
		t.Errorf("StartOfDay = %s", got)
	}
	// The day before ends where this one starts, so no instant is in two days
	// and none is in neither.
	if prev := b.Starts()[5]; !b.Next(prev).Equal(day) {
		t.Errorf("the 2nd's Next = %s, want %s", b.Next(prev).UTC(), day.UTC())
	}
}

func TestAStoreDayIsNotAUTCDay(t *testing.T) {
	la := zone(t, "America/Los_Angeles")
	at := time.Date(2025, 11, 7, 19, 0, 0, 0, time.UTC) // 2025-11-08T03:00Z, still the 7th in Los Angeles
	b := ForIn(Last7Days, at, la)
	if want := utc(2025, 11, 8, 8, 0); !b.End.Equal(want) {
		t.Errorf("End = %s, want the Los Angeles midnight 2025-11-08T08:00Z", b.End.UTC())
	}
	if want := utc(2025, 11, 1, 7, 0); !b.Start.Equal(want) {
		t.Errorf("Start = %s, want 2025-11-01T07:00Z", b.Start.UTC())
	}
}

func TestTwelveMonthsStartOnLocalFirsts(t *testing.T) {
	madrid := zone(t, "Europe/Madrid")
	b := ForIn(Last12Months, time.Date(2025, 11, 15, 12, 0, 0, 0, madrid), madrid)
	if want := utc(2024, 10, 31, 23, 0); !b.Start.Equal(want) {
		t.Errorf("Start = %s, want 2024-10-31T23:00Z", b.Start.UTC())
	}
	if want := utc(2025, 11, 30, 23, 0); !b.End.Equal(want) {
		t.Errorf("End = %s, want 2025-11-30T23:00Z", b.End.UTC())
	}
	if b.HasPrevious || !b.PrevStart.IsZero() || !b.PrevEnd.IsZero() {
		t.Errorf("12m has no previous period: %+v", b)
	}
	starts := b.Starts()
	if len(starts) != 13 || b.Buckets != 13 || b.Bucket != BucketMonth {
		t.Fatalf("%d starts, %d buckets of %s", len(starts), b.Buckets, b.Bucket)
	}
	for _, s := range starts {
		if l := s.In(madrid); l.Day() != 1 || l.Hour() != 0 || l.Minute() != 0 {
			t.Errorf("a month starts %s in Madrid, want a local first at midnight", l)
		}
	}
}

func TestANilZoneIsUTC(t *testing.T) {
	at := time.Date(2025, 11, 8, 10, 17, 42, 0, time.UTC)
	for _, p := range All() {
		got, want := ForIn(p, at, nil), ForIn(p, at, time.UTC)
		same := got.Start.Equal(want.Start) && got.End.Equal(want.End) && got.PrevStart.Equal(want.PrevStart) && got.PrevEnd.Equal(want.PrevEnd) &&
			got.Bucket == want.Bucket && got.Buckets == want.Buckets && got.HasPrevious == want.HasPrevious
		if !same || got.Start.Location() != want.Start.Location() || got.End.Location() != want.End.Location() {
			t.Errorf("%s: nil zone gave %+v, UTC gave %+v", p, got, want)
		}
	}
	if got := StartOfDay(2025, 11, 8, nil); !got.Equal(utc(2025, 11, 8, 0, 0)) {
		t.Errorf("StartOfDay(nil) = %s", got)
	}
}

// Next on a day steps a calendar day through the zone, so across a change it is
// 23 or 25 hours; on a month it lands on the next local first.
func TestNextStepsCalendarDaysAndMonthsInTheBucketsOwnZone(t *testing.T) {
	newYork := zone(t, "America/New_York")
	day := Bounds{Bucket: BucketDay}
	for _, c := range []struct {
		from time.Time
		want time.Duration
	}{
		{time.Date(2025, 3, 8, 0, 0, 0, 0, newYork), 24 * time.Hour},
		{time.Date(2025, 3, 9, 0, 0, 0, 0, newYork), 23 * time.Hour}, // spring forward
		{time.Date(2025, 11, 1, 0, 0, 0, 0, newYork), 24 * time.Hour},
		{time.Date(2025, 11, 2, 0, 0, 0, 0, newYork), 25 * time.Hour}, // fall back
	} {
		if got := day.Next(c.from).Sub(c.from); got != c.want {
			t.Errorf("Next(%s) is %s on, want %s", c.from, got, c.want)
		}
	}
	month := Bounds{Bucket: BucketMonth}
	from := time.Date(2025, 10, 1, 0, 0, 0, 0, newYork)
	want := time.Date(2025, 11, 1, 0, 0, 0, 0, newYork)
	if got := month.Next(from); !got.Equal(want) || got.Location() != newYork {
		t.Errorf("Next(Oct 1) = %s, want %s in New York", got, want)
	}
	if got := month.Next(time.Date(2025, 12, 1, 0, 0, 0, 0, newYork)); !got.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, newYork)) {
		t.Errorf("Next(Dec 1) = %s", got)
	}
}

// The previous period is as long as the current one in buckets, and every one of
// its starts is a local midnight, or the change where midnight is skipped.
func TestPreviousStartsInAZoneAcrossAChangeMatchStartsInCount(t *testing.T) {
	for _, c := range []struct {
		zone string
		at   time.Time
	}{
		{"Europe/Madrid", time.Date(2025, 3, 31, 12, 0, 0, 0, time.UTC)},
		{"America/New_York", time.Date(2025, 11, 5, 12, 0, 0, 0, time.UTC)},
		{"America/Santiago", time.Date(2023, 9, 8, 12, 0, 0, 0, time.UTC)},
	} {
		loc := zone(t, c.zone)
		for _, p := range []Period{Last7Days, Last30Days, Last90Days} {
			b := ForIn(p, c.at, loc)
			prev := b.PreviousStarts()
			if len(prev) != len(b.Starts()) || len(prev) != b.Buckets {
				t.Errorf("%s %s: %d previous starts, %d starts", c.zone, p, len(prev), len(b.Starts()))
			}
			for _, s := range prev {
				if !s.Equal(StartOfDay(s.In(loc).Year(), s.In(loc).Month(), s.In(loc).Day(), loc)) {
					t.Errorf("%s %s: previous start %s is not the start of its day", c.zone, p, s.In(loc))
				}
			}
			if last := prev[len(prev)-1]; !b.Next(last).Equal(b.Start) {
				t.Errorf("%s %s: the previous period ends at %s, the period starts at %s", c.zone, p, b.Next(last), b.Start)
			}
		}
	}
}

// Every time in an answer is in the zone asked for; for UTC that is what For
// always answered.
func TestEveryTimeInAnAnswerIsInTheZoneAskedFor(t *testing.T) {
	tokyo := zone(t, "Asia/Tokyo")
	at := time.Date(2025, 11, 8, 10, 17, 0, 0, time.UTC)
	for _, loc := range []*time.Location{time.UTC, tokyo} {
		for _, p := range All() {
			b := ForIn(p, at, loc)
			all := []time.Time{b.Start, b.End}
			all = append(all, b.Starts()...)
			if b.HasPrevious {
				all = append(all, b.PrevStart, b.PrevEnd)
				all = append(all, b.PreviousStarts()...)
			}
			for _, v := range all {
				if v.Location() != loc {
					t.Errorf("%s in %s: %s reports %s", p, loc, v, v.Location())
				}
			}
		}
	}
	for _, p := range All() {
		b := For(p, at)
		if b.Start.Location() != time.UTC || b.End.Location() != time.UTC {
			t.Errorf("For(%s) is not in UTC", p)
		}
	}
}

// Lord Howe changes its clocks by 30 minutes; 24 hours are still 24 buckets one
// elapsed hour long.
func TestAHalfHourChangeKeepsTwentyFourOneHourBuckets(t *testing.T) {
	howe := zone(t, "Australia/Lord_Howe")
	for _, at := range []time.Time{
		time.Date(2025, 4, 6, 12, 20, 0, 0, howe),  // clocks went back 30 minutes at 02:00
		time.Date(2025, 10, 5, 12, 20, 0, 0, howe), // clocks went forward 30 minutes at 02:00
	} {
		b := ForIn(Last24Hours, at, howe)
		starts := b.Starts()
		if len(starts) != 24 {
			t.Fatalf("%s: %d buckets", at, len(starts))
		}
		for i := 1; i < len(starts); i++ {
			if d := starts[i].Sub(starts[i-1]); d != time.Hour {
				t.Errorf("%s: bucket %d starts %s after the one before", at, i, d)
			}
		}
		if d := b.End.Sub(starts[23]); d != time.Hour {
			t.Errorf("%s: the last bucket is %s long", at, d)
		}
		if want := time.Date(2025, at.Month(), at.Day(), 13, 0, 0, 0, howe); !b.End.Equal(want) {
			t.Errorf("%s: End = %s, want %s", at, b.End, want)
		}
	}
}

// rfc parses an RFC 3339 instant worked out by hand; the expected values below
// are literals, never answers of the package's own functions.
func rfc(t *testing.T, v string) time.Time {
	t.Helper()
	got, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// rfcs parses a list of instants.
func rfcs(t *testing.T, vs ...string) []time.Time {
	t.Helper()
	out := make([]time.Time, len(vs))
	for i, v := range vs {
		out[i] = rfc(t, v)
	}
	return out
}

// everyDay lists n instants a uniform 24 hours apart from first, for stretches
// where the zone's offset does not move; the arithmetic is on UTC instants, so it
// does not go through the package.
func everyDay(first time.Time, n int) []time.Time {
	out := make([]time.Time, n)
	for i := range out {
		out[i] = first.Add(time.Duration(i) * 24 * time.Hour)
	}
	return out
}

// checkPrevious asserts what any previous period must hold: as many starts as
// the current one, strictly increasing, the first at PrevStart, and one step
// from the last landing on PrevEnd, which is Start.
func checkPrevious(t *testing.T, label string, b Bounds) {
	t.Helper()
	prev := b.PreviousStarts()
	if len(prev) != len(b.Starts()) || len(prev) != b.Buckets {
		t.Errorf("%s: %d previous starts, %d starts, %d buckets", label, len(prev), len(b.Starts()), b.Buckets)
		return
	}
	for i := 1; i < len(prev); i++ {
		if !prev[i].After(prev[i-1]) {
			t.Errorf("%s: previous start %d (%s) is not after %d (%s)", label, i, prev[i], i-1, prev[i-1])
		}
	}
	if !prev[0].Equal(b.PrevStart) {
		t.Errorf("%s: first previous start %s, PrevStart %s", label, prev[0], b.PrevStart)
	}
	if got := b.Next(prev[len(prev)-1]); !got.Equal(b.PrevEnd) {
		t.Errorf("%s: stepping the last previous start lands on %s, PrevEnd is %s", label, got, b.PrevEnd)
	}
	if !b.PrevEnd.Equal(b.Start) {
		t.Errorf("%s: PrevEnd %s is not Start %s", label, b.PrevEnd, b.Start)
	}
}

// The previous 24 hours are 24 elapsed hours, also the day after the clocks
// moved: a calendar day back from Start would make them 23 or 25.
func TestThePreviousTwentyFourHoursAreTwentyFourElapsedHoursAfterAChange(t *testing.T) {
	for _, c := range []struct {
		zone                  string
		now                   time.Time // the instant, in UTC, the day after the change
		prevStart, start, end string
	}{
		// Madrid 2025-03-30 02:00 CET -> 03:00 CEST (01:00Z). At 12:17 CEST on the 31st
		// (10:17Z) the hour under way ends 13:00 CEST = 11:00Z; Start is 24h before,
		// 2025-03-30T11:00Z (13:00 CEST); the 24h before that is 2025-03-29T11:00Z
		// (12:00 CET). A calendar day back from Start would be 12:00Z.
		{"Europe/Madrid", time.Date(2025, 3, 31, 10, 17, 0, 0, time.UTC), "2025-03-29T11:00:00Z", "2025-03-30T11:00:00Z", "2025-03-31T11:00:00Z"},
		// Madrid 2025-10-26 03:00 CEST -> 02:00 CET (01:00Z). At 12:17 CET on the 27th
		// (11:17Z) the hour ends 13:00 CET = 12:00Z; Start 2025-10-26T12:00Z; the 24h
		// before that start at 2025-10-25T12:00Z (14:00 CEST). A calendar day back
		// would be 11:00Z.
		{"Europe/Madrid", time.Date(2025, 10, 27, 11, 17, 0, 0, time.UTC), "2025-10-25T12:00:00Z", "2025-10-26T12:00:00Z", "2025-10-27T12:00:00Z"},
		// New York 2025-11-02 02:00 EDT -> 01:00 EST (06:00Z). At 12:17 EST on the 3rd
		// (17:17Z) the hour ends 13:00 EST = 18:00Z; Start 2025-11-02T18:00Z; the 24h
		// before start at 2025-11-01T18:00Z (14:00 EDT). A calendar day back would be 17:00Z.
		{"America/New_York", time.Date(2025, 11, 3, 17, 17, 0, 0, time.UTC), "2025-11-01T18:00:00Z", "2025-11-02T18:00:00Z", "2025-11-03T18:00:00Z"},
	} {
		loc := zone(t, c.zone)
		b := ForIn(Last24Hours, c.now.In(loc), loc)
		label := c.zone + " " + c.start
		for name, pair := range map[string][2]time.Time{
			"PrevStart": {b.PrevStart, rfc(t, c.prevStart)}, "Start": {b.Start, rfc(t, c.start)},
			"PrevEnd": {b.PrevEnd, rfc(t, c.start)}, "End": {b.End, rfc(t, c.end)},
		} {
			if !pair[0].Equal(pair[1]) {
				t.Errorf("%s: %s = %s, want %s", label, name, pair[0].UTC(), pair[1].UTC())
			}
		}
		if d := b.PrevEnd.Sub(b.PrevStart); d != 24*time.Hour {
			t.Errorf("%s: the previous period is %s long, want 24h", label, d)
		}
		prev := b.PreviousStarts()
		for i := 1; i < len(prev); i++ {
			if d := prev[i].Sub(prev[i-1]); d != time.Hour {
				t.Errorf("%s: previous bucket %d starts %s after the one before", label, i, d)
			}
		}
		checkPrevious(t, label, b)
	}
}

// A previous period of days starts on a local midnight, or on the change where
// the clocks skip it; a start taken as Start less n calendar days would land an
// hour late there.
func TestAPreviousPeriodOfDaysStartsOnLocalMidnightsOrTheChange(t *testing.T) {
	for _, c := range []struct {
		name      string
		zone      string
		p         Period
		now       time.Time // local clock
		prevStart string
		starts    func(t *testing.T) []time.Time // previous starts, as literals
	}{
		// Santiago skipped 2023-09-03 00:00 (-04 -> -03, the change is 04:00Z). At the
		// 9th, 7d is the 3rd to the 9th, Start is the change; the days before are
		// midnights at -04 from 2023-08-27T04:00Z, 24h apart through 09-02T04:00Z.
		// Start less 7 calendar days would be 05:00Z on 08-27.
		{"Santiago 7d, Start on the skipped midnight", "America/Santiago", Last7Days, time.Date(2023, 9, 9, 12, 0, 0, 0, time.UTC), "2023-08-27T04:00:00Z",
			func(t *testing.T) []time.Time {
				return rfcs(t, "2023-08-27T04:00:00Z", "2023-08-28T04:00:00Z", "2023-08-29T04:00:00Z", "2023-08-30T04:00:00Z", "2023-08-31T04:00:00Z", "2023-09-01T04:00:00Z", "2023-09-02T04:00:00Z")
			}},
		// At the 12th, 7d is the 6th to the 12th (midnights at -03, 03:00Z) and the
		// previous period is 08-30 to 09-05, holding the skipped 3rd, which starts at
		// the change 2023-09-03T04:00Z; the 4th and 5th start at 03:00Z.
		{"Santiago 7d, the skipped day inside", "America/Santiago", Last7Days, time.Date(2023, 9, 12, 12, 0, 0, 0, time.UTC), "2023-08-30T04:00:00Z",
			func(t *testing.T) []time.Time {
				return rfcs(t, "2023-08-30T04:00:00Z", "2023-08-31T04:00:00Z", "2023-09-01T04:00:00Z", "2023-09-02T04:00:00Z", "2023-09-03T04:00:00Z", "2023-09-04T03:00:00Z", "2023-09-05T03:00:00Z")
			}},
		// At 2023-10-02, 30d is 09-03 to 10-02: Start is the change; the previous
		// 30 days are 08-04 to 09-02, all midnights at -04, 04:00Z, 24h apart.
		{"Santiago 30d", "America/Santiago", Last30Days, time.Date(2023, 10, 2, 12, 0, 0, 0, time.UTC), "2023-08-04T04:00:00Z",
			func(t *testing.T) []time.Time { return everyDay(rfc(t, "2023-08-04T04:00:00Z"), 30) }},
		// Havana skips 2025-03-09 00:00 (-05 -> -04, the change is 05:00Z). At the
		// 15th, 7d is 03-09 to 03-15 and Start is the change; the previous days are
		// midnights at -05, 05:00Z from 03-02 to 03-08. Start less 7 calendar days
		// would be 06:00Z on 03-02.
		{"Havana 7d", "America/Havana", Last7Days, time.Date(2025, 3, 15, 12, 0, 0, 0, time.UTC), "2025-03-02T05:00:00Z",
			func(t *testing.T) []time.Time { return everyDay(rfc(t, "2025-03-02T05:00:00Z"), 7) }},
		// At 2025-04-07, 30d is 03-09 to 04-07; the previous 30 days are 02-07 to
		// 03-08, midnights at -05 from 2025-02-07T05:00Z.
		{"Havana 30d", "America/Havana", Last30Days, time.Date(2025, 4, 7, 12, 0, 0, 0, time.UTC), "2025-02-07T05:00:00Z",
			func(t *testing.T) []time.Time { return everyDay(rfc(t, "2025-02-07T05:00:00Z"), 30) }},
	} {
		loc := zone(t, c.zone)
		// the clock is given as wall time in the zone: 12:00 local on the stated day.
		now := time.Date(c.now.Year(), c.now.Month(), c.now.Day(), 12, 0, 0, 0, loc)
		b := ForIn(c.p, now, loc)
		if want := rfc(t, c.prevStart); !b.PrevStart.Equal(want) {
			t.Errorf("%s: PrevStart = %s, want %s", c.name, b.PrevStart.UTC(), want.UTC())
		}
		want, got := c.starts(t), b.PreviousStarts()
		if len(got) != len(want) {
			t.Errorf("%s: %d previous starts, want %d", c.name, len(got), len(want))
			continue
		}
		for i := range want {
			if !got[i].Equal(want[i]) {
				t.Errorf("%s: previous start %d = %s, want %s", c.name, i, got[i].UTC(), want[i].UTC())
			}
		}
		checkPrevious(t, c.name, b)
	}
}

// A date the zone never had starts where the next date that exists starts; the
// missing day has no bucket and the starts keep going forward.
func TestASkippedCalendarDayStartsWhereTheNextDayDoes(t *testing.T) {
	apia := zone(t, "Pacific/Apia")
	// Apia went from 2011-12-29 24:00 (-10) to 2011-12-31 00:00 (+14) at 10:00Z on
	// 2011-12-30, so the 30th never existed.
	if a, b := StartOfDay(2011, 12, 30, apia), StartOfDay(2011, 12, 31, apia); !a.Equal(b) || !a.Equal(rfc(t, "2011-12-30T10:00:00Z")) {
		t.Errorf("StartOfDay(30th) = %s, the 31st = %s, want both 2011-12-30T10:00Z", a.UTC(), b.UTC())
	}
	now := time.Date(2012, 1, 1, 12, 0, 0, 0, apia)
	b := ForIn(Last7Days, now, apia)
	starts := b.Starts()
	// The window is the 26th to the 1st; the 26th..29th start at -10 midnights
	// (2011-12-26T10:00Z.. 12-29T10:00Z), then the 31st and 1st start at +14
	// midnights (12-30T10:00Z, 12-31T10:00Z).
	want := rfcs(t, "2011-12-26T10:00:00Z", "2011-12-27T10:00:00Z", "2011-12-28T10:00:00Z", "2011-12-29T10:00:00Z", "2011-12-30T10:00:00Z", "2011-12-31T10:00:00Z")
	if len(starts) != len(want) {
		t.Fatalf("%d starts %v, want %d", len(starts), starts, len(want))
	}
	for i := range want {
		if !starts[i].Equal(want[i]) {
			t.Errorf("start %d = %s, want %s", i, starts[i].UTC(), want[i].UTC())
		}
		if i > 0 && !starts[i].After(starts[i-1]) {
			t.Errorf("start %d (%s) does not come after the one before", i, starts[i])
		}
		if l := starts[i].In(apia); l.Hour() != 0 || l.Minute() != 0 {
			t.Errorf("start %d is %s, not a local midnight", i, l)
		}
	}
	if want := rfc(t, "2012-01-01T10:00:00Z"); !b.End.Equal(want) || !b.End.After(now) {
		t.Errorf("End = %s, want 2012-01-01T10:00Z, after now", b.End.UTC())
	}
	if !b.Next(starts[len(starts)-1]).Equal(b.End) {
		t.Errorf("stepping the last start does not reach End")
	}
}

// Asuncion skipped 2023-10-01 00:00 (-04 -> -03, the change is 04:00Z): a month
// step that keeps the clock time and moves the month would land an hour early.
func TestAMonthStepLandsOnTheLocalFirstWhenThatMidnightIsSkipped(t *testing.T) {
	asuncion := zone(t, "America/Asuncion")
	b := ForIn(Last12Months, time.Date(2024, 1, 15, 12, 0, 0, 0, asuncion), asuncion)
	// Jan..Mar 2023 are at -03 (03:00Z), Apr..Sep at -04 (04:00Z), the October first
	// starts at the change (04:00Z), Nov 2023..Jan 2024 at -03 (03:00Z).
	want := rfcs(t,
		"2023-01-01T03:00:00Z", "2023-02-01T03:00:00Z", "2023-03-01T03:00:00Z", "2023-04-01T04:00:00Z", "2023-05-01T04:00:00Z", "2023-06-01T04:00:00Z", "2023-07-01T04:00:00Z",
		"2023-08-01T04:00:00Z", "2023-09-01T04:00:00Z", "2023-10-01T04:00:00Z", "2023-11-01T03:00:00Z", "2023-12-01T03:00:00Z", "2024-01-01T03:00:00Z")
	got := b.Starts()
	if len(got) != len(want) {
		t.Fatalf("%d starts, want %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("month %d starts %s, want %s", i, got[i].UTC(), want[i].UTC())
		}
	}
	if end := rfc(t, "2024-02-01T03:00:00Z"); !b.End.Equal(end) {
		t.Errorf("End = %s, want 2024-02-01T03:00Z", b.End.UTC())
	}
}
