package sitetraffic

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"
)

// fakeTimers stands in for a keeper's timers: each timer its loop asks for is
// announced on asked, with its duration, and fires when the test sends on fire.
type fakeTimers struct {
	asked chan time.Duration
	fire  chan time.Time
}

func newFakeTimers() *fakeTimers {
	return &fakeTimers{asked: make(chan time.Duration, 16), fire: make(chan time.Time)}
}

func (f *fakeTimers) newTimer(d time.Duration) (<-chan time.Time, func() bool) {
	f.asked <- d
	return f.fire, func() bool { return true }
}

// next is the duration of the next timer the loop asks for.
func (f *fakeTimers) next(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-f.asked:
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("the loop asked for no timer")
		return 0
	}
}

// waitsAfterFailures fires the loop's timers until it has asked for n of
// them, and answers their durations in order.
func (f *fakeTimers) waitsAfterFailures(t *testing.T, n int) []time.Duration {
	t.Helper()
	waits := []time.Duration{f.next(t)}
	for len(waits) < n {
		f.fire <- time.Time{}
		waits = append(waits, f.next(t))
	}
	return waits
}

// wantRetries checks that the waits after failures start short and double,
// never past the keeper's regular wait.
func wantRetries(t *testing.T, waits []time.Duration, regular time.Duration) {
	t.Helper()
	if waits[0] > time.Minute {
		t.Fatalf("after a failure the keeper waited %s before trying again, want a short retry", waits[0])
	}
	for i := 1; i < len(waits); i++ {
		want := min(2*waits[i-1], regular)
		if waits[i] != want {
			t.Fatalf("retry %d waited %s after %s, want %s: the retries %v should double up to %s", i+1, waits[i], waits[i-1], want, waits, regular)
		}
	}
	if last := waits[len(waits)-1]; last != regular {
		t.Fatalf("the retries %v never reached the regular %s", waits, regular)
	}
}

// A ROTATION THAT FAILS IS TRIED AGAIN SOON. A keeper with no secret for the
// day counts no visitors, so waiting out the hourly re-check after a database
// blip would lose up to an hour of them; the retries start within a minute
// and double back up to the hour.
func TestAFailedRotationIsRetriedSoonAndBacksOffToTheHour(t *testing.T) {
	noon := time.Date(2036, 5, 4, 12, 0, 0, 0, time.UTC)
	v := NewVisitorSecrets(quietLogger(), func() *bun.DB { return nil }, func() time.Time { return noon })
	timers := newFakeTimers()
	v.newTimer = timers.newTimer
	v.Start(context.Background())
	defer v.Stop(context.Background())

	wantRetries(t, timers.waitsAfterFailures(t, 9), time.Hour)
}

// Midnight still comes first: a retry never waits past the rotation the day
// needs.
func TestARetryNeverWaitsPastMidnight(t *testing.T) {
	late := time.Date(2036, 5, 4, 23, 59, 50, 0, time.UTC)
	v := NewVisitorSecrets(quietLogger(), func() *bun.DB { return nil }, func() time.Time { return late })
	timers := newFakeTimers()
	v.newTimer = timers.newTimer
	v.Start(context.Background())
	defer v.Stop(context.Background())

	if got := timers.next(t); got != 10*time.Second {
		t.Fatalf("ten seconds before midnight a failed keeper waits %s, want the ten seconds to midnight", got)
	}
}

// A COUNTRY LOAD THAT FAILS IS TRIED AGAIN SOON, for the visitor secret's
// reason: a replica with no database records no country.
func TestAFailedCountryLoadIsRetriedSoonAndBacksOffToTheHour(t *testing.T) {
	c := NewCountryLoader(quietLogger(), func() *bun.DB { return nil })
	timers := newFakeTimers()
	c.newTimer = timers.newTimer
	c.Start(context.Background())
	defer c.Stop(context.Background())

	wantRetries(t, timers.waitsAfterFailures(t, 9), countryRecheckEvery)
}
