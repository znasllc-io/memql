package sitetraffic

import (
	"testing"
	"time"
)

// The window a run looks for open days in is the request log's configured
// retention, counted back from today: a run never asks the log where its oldest
// row is (that row sits in a compressed chunk, so the asking would decompress
// it every five minutes), and never reaches past the 400 days the daily table
// keeps, should the retention ever be configured longer than that.
func TestTheOpenWindowIsTheRequestLogsRetention(t *testing.T) {
	today := time.Date(2036, 6, 15, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		retentionDays int
		want          time.Time
	}{
		{30, time.Date(2036, 5, 16, 0, 0, 0, 0, time.UTC)},
		{1, time.Date(2036, 6, 14, 0, 0, 0, 0, time.UTC)},
		// 2036 is a leap year, so 365 days back from 2036-06-15 is 2035-06-16.
		{365, time.Date(2035, 6, 16, 0, 0, 0, 0, time.UTC)},
		// Capped at the daily table's 400 days: 2035-05-12.
		{500, time.Date(2035, 5, 12, 0, 0, 0, 0, time.UTC)},
	} {
		if got := openFrom(today, c.retentionDays); !got.Equal(c.want) {
			t.Errorf("openFrom(%s, %d) = %s, want %s", today.Format(time.DateOnly), c.retentionDays, got.Format(time.DateOnly), c.want.Format(time.DateOnly))
		}
	}
}
