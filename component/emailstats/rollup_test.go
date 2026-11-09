package emailstats

import (
	"testing"
	"time"
)

// A first walk that cannot read its first page is moved half way to the clock
// at a UTC midnight, never nearer the clock than a day, and never back: 400
// days, then 200, then 100, and at the floor it stays a day behind.
func TestALaterStartHalvesTheSpanToADayAndNeverMovesBack(t *testing.T) {
	askedAt := time.Date(2041, 6, 17, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		since time.Time
		want  time.Time
	}{
		{"four hundred days back", askedAt.Add(-retention), time.Date(2040, 11, 29, 0, 0, 0, 0, time.UTC)},
		{"two hundred days back", time.Date(2040, 11, 29, 0, 0, 0, 0, time.UTC), time.Date(2041, 3, 9, 0, 0, 0, 0, time.UTC)},
		{"two days back, which halves to a day", time.Date(2041, 6, 15, 12, 0, 0, 0, time.UTC), time.Date(2041, 6, 16, 0, 0, 0, 0, time.UTC)},
		{"thirty hours back, floored to a day", time.Date(2041, 6, 16, 6, 0, 0, 0, time.UTC), time.Date(2041, 6, 16, 6, 0, 0, 0, time.UTC)},
		{"already at the floor", time.Date(2041, 6, 16, 0, 0, 0, 0, time.UTC), time.Date(2041, 6, 16, 0, 0, 0, 0, time.UTC)},
	} {
		got := laterStart(c.since, askedAt)
		if !got.Equal(c.want) {
			t.Errorf("%s: from %v the next start is %v, want %v", c.name, c.since, got, c.want)
		}
		if got.Before(c.since) || got.After(askedAt.Add(-firstWalkFloor)) {
			t.Errorf("%s: %v is before the start it moves from, or nearer the clock than a day", c.name, got)
		}
	}
}
