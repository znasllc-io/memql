package calls

import (
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// The tester password limit is per account and durable: five wrong passwords
// within 15 minutes pause that account's simulated results for 15 minutes from
// the fifth. There is no installation-wide limit, so a stranger cannot lock
// every tester out.
const (
	testerFailureLimit  = 5
	testerFailureWindow = 15 * time.Minute
	testerPauseLength   = 15 * time.Minute
)

// testerPause is the time left in the account's pause, zero when none. A pause
// recorded further ahead than one pause length (a clock rollback) reads as one
// pause length, so a rollback never lengthens it beyond its rule.
func (v *commerceState) testerPause(now time.Time) time.Duration {
	until := v.TesterPausedUntil
	if until.IsZero() || !now.Before(until) {
		return 0
	}
	if limit := now.Add(testerPauseLength); until.After(limit) {
		until = limit
	}
	return until.Sub(now)
}

// recordTesterFailure keeps only failures from the current window (a failure
// time ahead of now counts as now) and adds this one. The fifth starts the
// pause and clears the list; otherwise it answers the tries that remain.
func (v *commerceState) recordTesterFailure(now time.Time) (attemptsLeft int, pause time.Duration) {
	kept := make([]time.Time, 0, testerFailureLimit)
	for _, at := range v.TesterFailures {
		if at.After(now) {
			at = now
		}
		if now.Sub(at) < testerFailureWindow {
			kept = append(kept, at)
		}
	}
	kept = append(kept, now)
	if len(kept) >= testerFailureLimit {
		v.TesterFailures = nil
		v.TesterPausedUntil = now.Add(testerPauseLength)
		return 0, testerPauseLength
	}
	v.TesterFailures = kept
	return testerFailureLimit - len(kept), 0
}

// clearTesterFailures forgets failures and an ended pause after a correct
// password; it reports whether the record changed and must be saved.
func (v *commerceState) clearTesterFailures() bool {
	if len(v.TesterFailures) == 0 && v.TesterPausedUntil.IsZero() {
		return false
	}
	v.TesterFailures, v.TesterPausedUntil = nil, time.Time{}
	return true
}

// testerGate decides whether a simulated result may apply. It runs only in mock
// mode, after the operation and outcome are known, and before any effect. The
// order matters: a paused account is refused before the password is looked at,
// so parallel requests cannot exceed the limit, and a missing password is never
// counted. changed reports a failure or a clear that must be saved, whether or
// not the result is refused.
func testerGate(c CommerceConfig, v *commerceState, kind string, req callrules.AccountRequest, now time.Time) (changed bool, err error) {
	if testerBackOut(kind, req.Outcome) {
		return false, nil
	}
	if c.TesterPassword == nil {
		return false, callrules.Refuse(callrules.ErrForbidden, callrules.ReasonTesterAccessUnavailable, "simulated results are switched off on this installation")
	}
	if wait := v.testerPause(now); wait > 0 {
		return false, &callrules.TesterPasswordPaused{RetryAfter: wait}
	}
	if canonicalTesterPassword(req.TesterPassword) == "" {
		return false, callrules.Refuse(callrules.ErrInvalid, callrules.ReasonTesterPasswordRequired, "a tester password is required")
	}
	if !c.TesterPassword.matches(req.TesterPassword) {
		left, pause := v.recordTesterFailure(now)
		if pause > 0 {
			return true, &callrules.TesterPasswordPaused{RetryAfter: pause}
		}
		return true, &callrules.TesterPasswordIncorrect{AttemptsLeft: left}
	}
	return v.clearTesterFailures(), nil
}
