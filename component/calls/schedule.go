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

// ScheduledCallOpensEarly is how long before its start a booked call can be
// joined: the first join from then on opens its room. It is the capability's
// constant, not a policy value: it is the courtesy of arriving a little early,
// the same on every deployable, and a deployable that set it to hours would let
// a booking open a call long before the slot it holds begins.
const ScheduledCallOpensEarly = 10 * time.Minute

// MaxScheduledTitleRunes is the longest title a booking may carry, in
// characters: it is the calendar entry's summary.
const MaxScheduledTitleRunes = 80

// The states of a booked call (v1:calls:scheduledCall.status). An absent
// status reads as BookingScheduled. A booking is started when its room is
// opened, and stays started however long the call runs: a live call is never
// cut at its booked end.
const (
	BookingScheduled = "scheduled"
	BookingCancelled = "cancelled"
	BookingStarted   = "started"
	BookingEnded     = "ended"
)

// The reason codes of a booking's refusals, each stable for a client to switch
// on. A join answers not_yet, cancelled and call_ended in its JoinResult;
// booking, moving and cancelling answer theirs in a *Refusal.
const (
	// ReasonNotYet: the call opens ScheduledCallOpensEarly before its start,
	// and it is earlier than that.
	ReasonNotYet = "not_yet"
	// ReasonCancelled: the booking was cancelled, and its links open nothing.
	ReasonCancelled = "cancelled"
	// ReasonCallEnded: the call ended, or its booked end passed before anyone
	// opened it. DecideJoin answers an ended room with the same code.
	ReasonCallEnded = "call_ended"
	// ReasonStarted: the call has started, so it can no longer be cancelled or
	// moved.
	ReasonStarted = "started"
	// ReasonInPast, ReasonBeyondHorizon and ReasonTooLong are ValidateSchedule's
	// first three refusals.
	ReasonInPast        = "in_past"
	ReasonBeyondHorizon = "beyond_horizon"
	ReasonTooLong       = "too_long"
	// ReasonTitleTooLong: the title is longer than MaxScheduledTitleRunes.
	ReasonTitleTooLong = "title_too_long"
	// ReasonMinutesInvalid: the booked length is zero minutes or less.
	ReasonMinutesInvalid = "minutes_invalid"
	// ReasonOverlaps and ReasonTooManyUpcoming are ValidateSchedule's last two:
	// the booking clashes with the key's other bookings.
	ReasonOverlaps        = "overlaps"
	ReasonTooManyUpcoming = "too_many_upcoming"
)

// BookingRefusal is the *Refusal a ValidateSchedule error refuses a booking
// with: ErrInvalid for a booking no state of the key could make acceptable,
// ErrConflict for one that clashes with the key's other bookings. The cause
// stays wrapped, so errors.Is finds it too. Nil is nil, and any other error is
// answered as it is.
func BookingRefusal(err error) error {
	for _, r := range []struct {
		cause, class error
		reason       string
	}{
		{ErrInPast, ErrInvalid, ReasonInPast},
		{ErrBeyondHorizon, ErrInvalid, ReasonBeyondHorizon},
		{ErrTooLong, ErrInvalid, ReasonTooLong},
		{ErrOverlaps, ErrConflict, ReasonOverlaps},
		{ErrTooManyUpcoming, ErrConflict, ReasonTooManyUpcoming},
	} {
		if errors.Is(err, r.cause) {
			return &Refusal{Class: r.class, Reason: r.reason, Detail: r.cause.Error(), Cause: err}
		}
	}
	return err
}

// BookingJoin is the answer to a join of a booked call whose room is not open:
// the join opens it, or it is refused with a reason, and a call that opens
// later says how long until it does.
type BookingJoin struct {
	Open       bool
	Reason     string
	RetryAfter time.Duration
}

// DecideBookingJoin answers a join of a booked call whose room is not open, at
// now. A cancelled booking and an ended one are refused at once, whatever the
// time; so is one whose booked end has passed with nobody having opened it.
// Earlier than ScheduledCallOpensEarly before the start is not yet, with the
// time until the call opens. From then until the booked end, the join opens
// the room. The order is the contract: a cancelled booking is never "not yet".
func DecideBookingJoin(now time.Time, status string, start, end time.Time) BookingJoin {
	switch status {
	case BookingCancelled:
		return BookingJoin{Reason: ReasonCancelled}
	case BookingEnded:
		return BookingJoin{Reason: ReasonCallEnded}
	}
	if !now.Before(end) {
		return BookingJoin{Reason: ReasonCallEnded}
	}
	if opens := start.Add(-ScheduledCallOpensEarly); now.Before(opens) {
		return BookingJoin{Reason: ReasonNotYet, RetryAfter: opens.Sub(now)}
	}
	return BookingJoin{Open: true}
}
