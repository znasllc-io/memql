package sitetraffic

import "time"

// The edge keepers' retries: the visitor secret (visitors.go) and the country
// database (countries.go).
//
// Both keepers check the database on a slow schedule -- the secret at each UTC
// midnight and hourly, the country database hourly -- because a check that
// succeeds has nothing more to do for an hour. A check that FAILS is another
// matter: until one succeeds, a keeper without the day's secret records no
// visitor, and a loader without a database records no country. Waiting out
// the regular hour after a database blip of a few seconds lost up to an hour
// of both, on every replica the blip reached.
//
// So after a failure the next check comes retryFirst later, then twice as long
// after each further failure, up to the keeper's regular wait; the first check
// that succeeds returns the keeper to that wait. Thirty seconds is long enough
// that a database which is down for a while is asked about a handful of times
// an hour, each a Warn in the log, and short enough that a blip costs a
// replica well under a minute of visitors and countries
// (TestAFailedRotationIsRetriedSoonAndBacksOffToTheHour,
// TestAFailedCountryLoadIsRetriedSoonAndBacksOffToTheHour). The secret's
// midnight still comes first: a retry never waits past the rotation the next
// day needs (TestARetryNeverWaitsPastMidnight).
//
// Per replica, with nothing shared: each replica's keeper retries on its own
// schedule, and a check is one or two statements on a table of a row or two,
// so even every replica retrying at once costs the database nothing.

// retryFirst is the wait after a first failed check.
const retryFirst = 30 * time.Second

// retryWait is how long a keeper waits before its next check after failures
// failed checks in a row: retryFirst after one, doubling after each further
// one, never longer than regular. No failure is the regular wait.
func retryWait(failures int, regular time.Duration) time.Duration {
	if failures <= 0 {
		return regular
	}
	wait := retryFirst
	for i := 1; i < failures && wait < regular; i++ {
		wait *= 2
	}
	return min(wait, regular)
}

// systemTimer is a keeper's real timer: the channel that fires after d, and
// its stop.
func systemTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}
