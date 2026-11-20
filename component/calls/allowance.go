package calls

import (
	"errors"
	"time"
)

// KeyAllowance describes time still covered by one pass. Days are UTC calendar
// days, not the browser's timezone. AvailableUntil is the latest uninterrupted
// room interval that may begin at the evaluation time; it can cross midnight
// only when the first day's allowance lasts until the next day's reset.
type KeyAllowance struct {
	RemainingSeconds      int64     `json:"remainingSeconds"`
	DailyRemainingSeconds int64     `json:"dailyRemainingSeconds"`
	DailyResetsAt         time.Time `json:"dailyResetsAt"`
	AvailableUntil        time.Time `json:"availableUntil"`
}

// FundedRoomTime is authoritative operational room time, already scoped to the
// pass being evaluated. Start is absent until the first media admission, so a
// waiting-only room consumes no time. End is absent while the room remains open. Deadline is
// the immutable ceiling admitted when that room opened: delayed cleanup never
// charges time beyond the allowance the service actually authorized.
type FundedRoomTime struct {
	Start, End, Deadline time.Time
}

var ErrAllowanceUnavailable = errors.New("calls: the pass allowance cannot be verified")

// PassAllowance counts elapsed room time once per room after its first media
// admission, including host-left grace. It does not infer attendance or charges.
// All arithmetic uses durations until the response is rounded down to whole
// remaining seconds; repeated reads and short calls do not accumulate rounding.
func PassAllowance(pass AccountPass, rooms []FundedRoomTime, at time.Time) (KeyAllowance, error) {
	day := time.Date(at.UTC().Year(), at.UTC().Month(), at.UTC().Day(), 0, 0, 0, 0, time.UTC)
	out := KeyAllowance{DailyResetsAt: day.AddDate(0, 0, 1), AvailableUntil: at}
	// A malformed stored entitlement fails closed instead of becoming an
	// overflowing duration or a pass without an expiry. The bounds here protect
	// arithmetic; the operator's catalog selects smaller product limits.
	const maximumSeconds = int64(366 * 24 * 60 * 60)
	if pass.ID == "" || pass.ActivatedAt.IsZero() || !pass.ExpiresAt.After(pass.ActivatedAt) ||
		pass.TotalSeconds <= 0 || pass.TotalSeconds > maximumSeconds || pass.DailySeconds <= 0 || pass.DailySeconds > 86400 || pass.MaxPeople <= 0 {
		return out, ErrAllowanceUnavailable
	}
	if at.Before(pass.ActivatedAt) || !at.Before(pass.ExpiresAt) {
		return out, nil
	}
	totalLeft := time.Duration(pass.TotalSeconds) * time.Second
	dailyLeft := time.Duration(pass.DailySeconds) * time.Second
	for _, room := range rooms {
		if room.Start.IsZero() {
			continue
		}
		if room.Deadline.IsZero() || !room.Deadline.After(room.Start) ||
			room.Start.Before(pass.ActivatedAt) || room.Deadline.After(pass.ExpiresAt) || (!room.End.IsZero() && room.End.Before(room.Start)) {
			return out, ErrAllowanceUnavailable
		}
		end := at
		if !room.End.IsZero() && room.End.Before(end) {
			end = room.End
		}
		if room.Deadline.Before(end) {
			end = room.Deadline
		}
		if !end.After(room.Start) {
			continue
		}
		totalLeft = max(0, totalLeft-end.Sub(room.Start))
		if start := maxTime(room.Start, day); end.After(start) {
			dailyLeft = max(0, dailyLeft-end.Sub(start))
		}
	}
	out.RemainingSeconds = int64(totalLeft / time.Second)
	out.DailyRemainingSeconds = int64(dailyLeft / time.Second)
	if totalLeft <= 0 || dailyLeft <= 0 {
		return out, nil
	}
	until := at
	left := totalLeft
	dayLeft := dailyLeft
	boundary := out.DailyResetsAt
	// The remaining pass has a finite duration. This walks at most 366 days,
	// and normally one: a three-hour daily cap cannot bridge a whole UTC day.
	for left > 0 && until.Before(pass.ExpiresAt) {
		span := min(left, dayLeft, pass.ExpiresAt.Sub(until), boundary.Sub(until))
		until = until.Add(span)
		left -= span
		if !until.Equal(boundary) || left <= 0 || !until.Before(pass.ExpiresAt) {
			break
		}
		boundary = boundary.AddDate(0, 0, 1)
		dayLeft = time.Duration(pass.DailySeconds) * time.Second
	}
	out.AvailableUntil = until
	return out, nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
