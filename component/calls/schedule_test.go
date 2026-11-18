package calls

import (
	"errors"
	"testing"
	"time"
)

func TestValidateSchedule(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	p := DefaultPolicy()
	iv := func(startMin, lenMin int) Interval {
		s := now.Add(time.Duration(startMin) * time.Minute)
		return Interval{Start: s, End: s.Add(time.Duration(lenMin) * time.Minute)}
	}
	many := make([]Interval, 20)
	for i := range many {
		many[i] = iv(1000+i*100, 30)
	}
	cases := []struct {
		name     string
		want     Interval
		upcoming []Interval
		err      error
	}{
		{"ok", iv(60, 60), nil, nil},
		{"exactly 480 minutes", iv(60, 480), nil, nil},
		{"past", iv(-5, 30), nil, ErrInPast},
		{"beyond horizon", Interval{Start: now.Add(31 * 24 * time.Hour), End: now.Add(31*24*time.Hour + time.Hour)}, nil, ErrBeyondHorizon},
		{"too long", iv(60, 481), nil, ErrTooLong},
		{"overlap", iv(60, 60), []Interval{iv(90, 60)}, ErrOverlaps},
		{"touching is fine", iv(60, 60), []Interval{iv(120, 60)}, nil},
		{"21st upcoming", iv(30000, 30), many, ErrTooManyUpcoming},
		{"empty interval", Interval{Start: now.Add(time.Hour), End: now.Add(time.Hour)}, nil, ErrTooLong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateSchedule(now, c.want, p, c.upcoming); !errors.Is(err, c.err) {
				t.Fatalf("got %v want %v", err, c.err)
			}
		})
	}
}

// TestBookingRefusalNamesAReasonAndAClass: each ValidateSchedule error becomes a
// refusal with its stable reason, of the class a route maps to its reply -- invalid
// for a booking no state could make acceptable, conflict for one that clashes with the
// key's other bookings -- and the cause stays findable.
func TestBookingRefusalNamesAReasonAndAClass(t *testing.T) {
	for _, c := range []struct {
		cause, class error
		reason       string
	}{
		{ErrInPast, ErrInvalid, ReasonInPast},
		{ErrBeyondHorizon, ErrInvalid, ReasonBeyondHorizon},
		{ErrTooLong, ErrInvalid, ReasonTooLong},
		{ErrOverlaps, ErrConflict, ReasonOverlaps},
		{ErrTooManyUpcoming, ErrConflict, ReasonTooManyUpcoming},
	} {
		err := BookingRefusal(c.cause)
		if !errors.Is(err, c.class) || !errors.Is(err, c.cause) {
			t.Errorf("%v: got %v, want it to be %v and %v", c.cause, err, c.class, c.cause)
		}
		if got := ReasonOf(err); got != c.reason {
			t.Errorf("%v: reason %q, want %q", c.cause, got, c.reason)
		}
	}
	if BookingRefusal(nil) != nil {
		t.Error("no error became a refusal")
	}
	other := errors.New("the database is down")
	if got := BookingRefusal(other); got != other || ReasonOf(got) != "" {
		t.Errorf("an unrelated error became %v", got)
	}
}

// TestDecideBookingJoin: a booked call whose room is not open opens on the first join
// from ten minutes before its start until its booked end. Earlier is not yet, with the
// time until it opens; a cancelled or ended booking, and one whose end passed with
// nobody having opened it, are refused at once whatever the time.
func TestDecideBookingJoin(t *testing.T) {
	start := time.Date(2026, 12, 1, 15, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	for _, c := range []struct {
		name   string
		now    time.Time
		status string
		want   BookingJoin
	}{
		{"an hour early", start.Add(-time.Hour), BookingScheduled, BookingJoin{Reason: ReasonNotYet, RetryAfter: 50 * time.Minute}},
		{"a second too early", start.Add(-ScheduledCallOpensEarly - time.Second), BookingScheduled, BookingJoin{Reason: ReasonNotYet, RetryAfter: time.Second}},
		{"exactly ten minutes early", start.Add(-ScheduledCallOpensEarly), BookingScheduled, BookingJoin{Open: true}},
		{"at the start", start, "", BookingJoin{Open: true}},
		{"a second before the end", end.Add(-time.Second), BookingScheduled, BookingJoin{Open: true}},
		{"at the end", end, BookingScheduled, BookingJoin{Reason: ReasonCallEnded}},
		{"long after the end", end.Add(24 * time.Hour), BookingScheduled, BookingJoin{Reason: ReasonCallEnded}},
		{"cancelled, early", start.Add(-time.Hour), BookingCancelled, BookingJoin{Reason: ReasonCancelled}},
		{"cancelled, in its slot", start, BookingCancelled, BookingJoin{Reason: ReasonCancelled}},
		{"ended, in its slot", start, BookingEnded, BookingJoin{Reason: ReasonCallEnded}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := DecideBookingJoin(c.now, c.status, start, end); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}
