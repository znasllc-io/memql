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
// # Days are UTC
//
// Every bound is a UTC instant whatever zone `now` arrives in (D5). The
// statistics tables are keyed by UTC hour and UTC day, and a bound computed in
// another zone would cut a day in two.
//
// # The bucket under way is included
//
// A period ends where the bucket holding `now` ends: the next whole hour, the
// next UTC midnight, the first of next month. So "last 7 days" is today and
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

// For answers the bounds of p as of now. A period Parse would not return has
// empty bounds: no buckets and no previous period.
func For(p Period, now time.Time) Bounds {
	now = now.UTC()
	switch p {
	case Last24Hours:
		end := now.Truncate(time.Hour).Add(time.Hour)
		start := end.Add(-24 * time.Hour)
		return Bounds{
			Start: start, End: end,
			PrevStart: start.Add(-24 * time.Hour), PrevEnd: start,
			Bucket: BucketHour, Buckets: 24, HasPrevious: true,
		}
	case Last7Days, Last30Days, Last90Days:
		n := days(p)
		end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
		start := end.AddDate(0, 0, -n)
		return Bounds{
			Start: start, End: end,
			PrevStart: start.AddDate(0, 0, -n), PrevEnd: start,
			Bucket: BucketDay, Buckets: n, HasPrevious: true,
		}
	case Last12Months:
		month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return Bounds{
			Start: month.AddDate(0, -12, 0), End: month.AddDate(0, 1, 0),
			Bucket: BucketMonth, Buckets: 13,
		}
	}
	return Bounds{}
}

// Next is the start of the bucket after the one starting at t.
func (b Bounds) Next(t time.Time) time.Time {
	switch b.Bucket {
	case BucketHour:
		return t.Add(time.Hour)
	case BucketDay:
		return t.AddDate(0, 0, 1)
	case BucketMonth:
		return t.AddDate(0, 1, 0)
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
