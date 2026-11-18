package calls

import (
	"errors"
	"time"
)

// Interval is a half-open span [Start, End).
type Interval struct{ Start, End time.Time }

var (
	ErrInPast          = errors.New("calls: schedule starts in the past")
	ErrBeyondHorizon   = errors.New("calls: schedule starts beyond the horizon")
	ErrTooLong         = errors.New("calls: schedule length is outside the allowed range")
	ErrOverlaps        = errors.New("calls: schedule overlaps an upcoming call")
	ErrTooManyUpcoming = errors.New("calls: too many upcoming scheduled calls")
)

// Overlaps reports whether two spans share time. Spans that merely touch do
// not, so back-to-back calls are allowed.
func Overlaps(a, b Interval) bool {
	return a.Start.Before(b.End) && b.Start.Before(a.End)
}

// ValidateSchedule checks a requested span against policy and the person's
// existing upcoming spans. The first failing rule wins, in the order the
// errors are declared. A span of zero or negative length reports ErrTooLong
// rather than a seventh error: both are a length outside what policy allows.
func ValidateSchedule(now time.Time, want Interval, p Policy, upcoming []Interval) error {
	if want.Start.Before(now) {
		return ErrInPast
	}
	if want.Start.After(now.Add(p.ScheduleHorizon)) {
		return ErrBeyondHorizon
	}
	length := want.End.Sub(want.Start)
	if length <= 0 || length > time.Duration(p.MaxScheduledMinutes)*time.Minute {
		return ErrTooLong
	}
	for _, u := range upcoming {
		if Overlaps(want, u) {
			return ErrOverlaps
		}
	}
	if len(upcoming) >= p.MaxUpcomingScheduled {
		return ErrTooManyUpcoming
	}
	return nil
}
