package sitetraffic

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"
)

// switchableDB is a provider that answers no database until it is switched
// on, so a keeper's first checks fail and a later one succeeds.
func switchableDB(db *bun.DB) (func() *bun.DB, func()) {
	var on atomic.Bool
	return func() *bun.DB {
			if on.Load() {
				return db
			}
			return nil
		}, func() {
			on.Store(true)
		}
}

// A KEEPER THAT RECOVERS RETURNS TO ITS REGULAR WAIT: the retries are for a
// failing database, and once a rotation succeeds the keeper checks hourly
// again rather than every few seconds.
func TestAKeeperThatRecoversReturnsToItsRegularWait(t *testing.T) {
	db := edgeTestDB(t)
	noon := time.Date(2042, 3, 7, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() {
		_, _ = db.NewRaw("DELETE FROM edge_visitor_secret WHERE day >= ?", utcMidnight(noon)).Exec(context.Background())
	})
	provider, switchOn := switchableDB(db)
	v := NewVisitorSecrets(quietLogger(), provider, func() time.Time { return noon })
	timers := newFakeTimers()
	v.newTimer = timers.newTimer
	v.Start(context.Background())
	defer v.Stop(context.Background())

	if got := timers.next(t); got != retryFirst {
		t.Fatalf("after a failed rotation the keeper waits %s, want %s", got, retryFirst)
	}
	switchOn()
	timers.fire <- time.Time{}
	if got := timers.next(t); got != time.Hour {
		t.Fatalf("after a rotation that succeeded the keeper waits %s, want the hourly check", got)
	}
	if v.Visitor("site", "203.0.113.0/24", "UA", noon) == "" {
		t.Fatal("the rotation that succeeded left no visitor secret held")
	}
}

// The same for the country loader.
func TestACountryLoaderThatRecoversReturnsToItsRegularWait(t *testing.T) {
	db := edgeTestDB(t)
	provider, switchOn := switchableDB(db)
	c := NewCountryLoader(quietLogger(), provider)
	timers := newFakeTimers()
	c.newTimer = timers.newTimer
	c.Start(context.Background())
	defer c.Stop(context.Background())

	if got := timers.next(t); got != retryFirst {
		t.Fatalf("after a failed load the loader waits %s, want %s", got, retryFirst)
	}
	switchOn()
	timers.fire <- time.Time{}
	if got := timers.next(t); got != countryRecheckEvery {
		t.Fatalf("after a load that succeeded the loader waits %s, want the hourly check", got)
	}
}
