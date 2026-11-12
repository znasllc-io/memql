// Package statsperiod is the one definition of a statistics period: which
// ranges a statistics read offers, where each begins and ends, how it is cut
// into buckets, and which range it is compared with
// (docs/superpowers/specs/2026-11-08-site-and-email-statistics-design.md,
// D4 and D5).
//
// The website read (component/sitetraffic) and the email read
// (component/emailstats) both take their bounds from here, so "last 30 days"
// covers the same days on both pages.
//
// # Days are cut in one zone
//
// For cuts every bound in UTC whatever zone `now` arrives in (D5): the
// statistics tables are keyed by UTC hour and UTC day, and a bound computed in
// another zone would cut a day in two. ForIn cuts them in a zone it is handed,
// a store's own, and answers every time in that zone; this package loads no
// zone, a caller passes a *time.Location.
//
// # The bucket under way is included
//
// A period ends where the bucket holding `now` ends: the next whole hour, the
// start of tomorrow in the period's zone (UTC for For), the first of next month. So "last 7 days" is today and
// the six days before it, and today's partial figures are part of it. A period
// that ended at `now` would leave the newest bucket out until it closed.
//
// # Twelve months have no previous period
//
// 24h, 7d, 30d and 90d are compared with the period of the same length just
// before them. 12m is the current month and the twelve before it, thirteen
// monthly buckets, so this month stands beside the same month last year, and
// it has NO previous period: the thirteen months before those reach past the
// 400 days the daily tables keep, so a comparison would be against figures the
// storage no longer holds (D4).
package statsperiod

import (
	"fmt"
	"strings"
	"time"
)

// Period names a statistics range, in the spelling the wire carries.
type Period string

const (
	// Last24Hours is the 24 hours ending with the hour under way, in hourly
	// buckets.
	Last24Hours Period = "24h"
	// Last7Days, Last30Days and Last90Days end with today, in daily buckets.
	Last7Days  Period = "7d"
	Last30Days Period = "30d"
	Last90Days Period = "90d"
	// Last12Months is the current month and the twelve before it, in monthly
	// buckets, with no previous period.
	Last12Months Period = "12m"
)

// The bucket sizes, as Bounds.Bucket spells them.
const (
	BucketHour  = "hour"
	BucketDay   = "day"
	BucketMonth = "month"
)

// All lists every period, shortest first.
func All() []Period {
	return []Period{Last24Hours, Last7Days, Last30Days, Last90Days, Last12Months}
}

// Bounds is where a period begins and ends, how it is bucketed, and the period
// it is compared with. Every range is half-open, [Start, End), so two adjacent
// periods add up rather than sharing their boundary.
type Bounds struct {
	Start, End time.Time
	// PrevStart and PrevEnd are the previous period, the same length and
	// immediately before; both are zero when HasPrevious is false.
	PrevStart, PrevEnd time.Time
	// Bucket is BucketHour, BucketDay or BucketMonth.
	Bucket string
	// Buckets is how many buckets [Start, End) holds.
	Buckets int
	// HasPrevious is false for 12m (see the package comment).
	HasPrevious bool
}

// Parse reads a period off the wire. With no allowed periods it accepts every
// one in All; with some, it accepts only those, so a read that cannot answer a
// period refuses it instead of answering something else.
//
// A refusal names the accepted periods. Nothing is defaulted: a caller that
// asked for no period, or a misspelled one, has a mistake to correct, and
// answering a week in its place would show them a week labelled as what they
// asked for.
func Parse(s string, allowed ...Period) (Period, error) {
	if len(allowed) == 0 {
		allowed = All()
	}
	p := Period(strings.TrimSpace(s))
	var offered []Period
	for _, a := range allowed {
		// A listed value that is not a period cannot widen the set: For has no
		// bounds for it.
		if !known(a) {
			continue
		}
		if a == p {
			return p, nil
		}
		offered = append(offered, a)
	}
	if p == "" {
		return "", fmt.Errorf("period is required: one of %s", list(offered))
	}
	return "", fmt.Errorf("period must be one of %s; got %q", list(offered), s)
}

// For answers the bounds of p as of now, cut in UTC. A period Parse would not
// return has empty bounds: no buckets and no previous period.
func For(p Period, now time.Time) Bounds {
	return ForIn(p, now, time.UTC)
}

// ForIn answers the bounds of p as of now with every day, month and hour cut in
// loc, a store's own zone. A nil loc is UTC. Every time in the answer is in loc.
//
// A day starts at the first instant whose date in loc is that day, so where the
// clocks move a day is 23 or 25 hours long and a bucket is as long as its day.
// The 24 hours are the one exception to "calendar": they are 24 hours of elapsed
// time, 24 buckets of one hour each, so on a day the clocks go back two of them
// carry the same local hour.
func ForIn(p Period, now time.Time, loc *time.Location) Bounds {
	if loc == nil {
		loc = time.UTC
	}
	now = now.In(loc)
	switch p {
	case Last24Hours:
		// The hour under way starts at now less its LOCAL minutes: Truncate cuts
		// on UTC hours, which is half an hour off in a +05:30 zone.
		hour := now.Add(-(time.Duration(now.Minute())*time.Minute + time.Duration(now.Second())*time.Second + time.Duration(now.Nanosecond())))
		end := hour.Add(time.Hour)
		start := end.Add(-24 * time.Hour)
		return Bounds{
			Start: start, End: end,
			PrevStart: start.Add(-24 * time.Hour), PrevEnd: start,
			Bucket: BucketHour, Buckets: 24, HasPrevious: true,
		}
	case Last7Days, Last30Days, Last90Days:
		n := days(p)
		y, m, d := now.Date()
		end := StartOfDay(y, m, d+1, loc)
		start := StartOfDay(y, m, d+1-n, loc)
		return Bounds{
			Start: start, End: end,
			PrevStart: StartOfDay(y, m, d+1-2*n, loc), PrevEnd: start,
			Bucket: BucketDay, Buckets: n, HasPrevious: true,
		}
	case Last12Months:
		y, m, _ := now.Date()
		return Bounds{
			Start: StartOfDay(y, m-12, 1, loc), End: StartOfDay(y, m+1, 1, loc),
			Bucket: BucketMonth, Buckets: 13,
		}
	}
	return Bounds{}
}

// StartOfDay is the first instant whose date in loc is y-m-d; a nil loc is UTC.
// An out-of-range day or month normalises as time.Date does.
//
// time.Date(y, m, d, 0, 0, 0, 0, loc) is that instant except where the clocks
// skip midnight: there it answers an instant on the day before or an hour into
// the day, and the day starts at the change itself. A date the zone never had
// (Pacific/Apia, 2011-12-30) starts where the next date that exists starts, so
// the missing day has no bucket.
//
// Two assumptions are not handled. Where a zone's clocks go back across
// midnight, so that a date begins twice, the day is taken from the midnight the
// standard library answers; no zone does this at present. And where a clock goes
// back within the first minutes of a day, the period's end can fall before `now`
// for that stretch.
func StartOfDay(y int, m time.Month, d int, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	t := time.Date(y, m, d, 0, 0, 0, 0, loc)
	wy, wm, wd := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Date()
	gy, gm, gd := t.Date()
	if gy == wy && gm == wm && gd == wd && t.Hour() == 0 && t.Minute() == 0 {
		return t
	}
	// Midnight does not exist here: t sits at one side of the change.
	from, until := t.ZoneBounds()
	candidate := from
	if gy*10000+int(gm)*100+gd < wy*10000+int(wm)*100+wd {
		candidate = until
	}
	// The change is the day's first instant, or, for a date the zone skipped
	// altogether, the first instant after it.
	if cy, cm, cd := candidate.Date(); !candidate.IsZero() && cy*10000+int(cm)*100+cd >= wy*10000+int(wm)*100+wd {
		return candidate.In(loc)
	}
	return t
}

// Next is the start of the bucket after the one starting at t, in t's own
// location: a day or a month is stepped through StartOfDay, so it may be 23 or
// 25 hours on.
func (b Bounds) Next(t time.Time) time.Time {
	switch b.Bucket {
	case BucketHour:
		return t.Add(time.Hour)
	case BucketDay:
		y, m, d := t.Date()
		return StartOfDay(y, m, d+1, t.Location())
	case BucketMonth:
		y, m, _ := t.Date()
		return StartOfDay(y, m+1, 1, t.Location())
	}
	// No bucket size: there is no next bucket, and End stops any walk.
	return b.End
}

// Starts lists the start of every bucket of the period, oldest first.
func (b Bounds) Starts() []time.Time {
	return b.starts(b.Start, b.End)
}

// PreviousStarts lists the previous period's bucket starts, aligned by offset
// with Starts, so the previous line can be drawn under the current one; nil
// when there is no previous period.
func (b Bounds) PreviousStarts() []time.Time {
	if !b.HasPrevious {
		return nil
	}
	return b.starts(b.PrevStart, b.PrevEnd)
}

func (b Bounds) starts(from, to time.Time) []time.Time {
	if b.Buckets <= 0 {
		return nil
	}
	out := make([]time.Time, 0, b.Buckets)
	for t := from; t.Before(to) && len(out) < b.Buckets; t = b.Next(t) {
		out = append(out, t)
	}
	return out
}

func days(p Period) int {
	switch p {
	case Last7Days:
		return 7
	case Last30Days:
		return 30
	case Last90Days:
		return 90
	}
	return 0
}

func known(p Period) bool {
	for _, k := range All() {
		if p == k {
			return true
		}
	}
	return false
}

// list spells periods for a sentence: "7d, 30d, 90d or 12m".
func list(ps []Period) string {
	words := make([]string, len(ps))
	for i, p := range ps {
		words[i] = string(p)
	}
	switch len(words) {
	case 0:
		return "(none)"
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}
