package calls

import (
	"context"
	"strings"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/integrations/siteaccess"
)

// The tester code limit is per account and durable: five wrong codes
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

// clearTesterFailures forgets failures and an ended pause after a live code; it reports whether the record changed and must be saved.
func (v *commerceState) clearTesterFailures() bool {
	if len(v.TesterFailures) == 0 && v.TesterPausedUntil.IsZero() {
		return false
	}
	v.TesterFailures, v.TesterPausedUntil = nil, time.Time{}
	return true
}

// testerCodeMaxBytes bounds a typed tester code: a code is 25 symbols, and room for
// the spaces and dashes a person types keeps a paste of anything else short.
const testerCodeMaxBytes = 64

// TesterCodes decides a typed tester code for a site. integrations/siteaccess owns
// access codes and answers it for the calls-testers gate.
type TesterCodes interface {
	TesterCode(ctx context.Context, siteID, typed string, now time.Time) (siteaccess.TesterCodeVerdict, error)
}

// testerBackOut names the two results a person can always produce without a tester
// code, as on Stripe's or Didit's own page: leaving the checkout or the age check.
// They apply no credit and no age result.
func testerBackOut(kind, outcome string) bool {
	return kind == "checkout" && outcome == "cancelled" || kind == "verification" && outcome == "abandoned"
}

// testerGate decides whether a simulated result may apply. It runs only in mock
// mode, after the operation and outcome are known, and before any effect. The
// order matters: a paused account is refused before any lookup, so parallel
// requests cannot exceed the limit; a missing code, an expired one, a site that
// cannot hold codes and a failed lookup are never counted. changed reports a
// failure or a clear that must be saved, whether or not the result is refused.
func testerGate(ctx context.Context, codes TesterCodes, siteID string, v *commerceState, kind string, req callrules.AccountRequest, now time.Time) (changed bool, err error) {
	if testerBackOut(kind, req.Outcome) {
		return false, nil
	}
	if codes == nil {
		return false, testerAccessUnavailable()
	}
	if wait := v.testerPause(now); wait > 0 {
		return false, &callrules.TesterCodePaused{RetryAfter: wait}
	}
	if strings.TrimSpace(req.TesterCode) == "" {
		return false, callrules.Refuse(callrules.ErrInvalid, callrules.ReasonTesterCodeRequired, "a tester code is required")
	}
	verdict, err := codes.TesterCode(ctx, siteID, req.TesterCode, now)
	if err != nil {
		return false, err
	}
	switch verdict {
	case siteaccess.TesterCodeLive:
		return v.clearTesterFailures(), nil
	case siteaccess.TesterCodeExpired:
		return false, callrules.Refuse(callrules.ErrForbidden, callrules.ReasonTesterCodeExpired, "the tester code has expired")
	case siteaccess.TesterCodeUnavailable:
		return false, testerAccessUnavailable()
	}
	left, pause := v.recordTesterFailure(now)
	if pause > 0 {
		return true, &callrules.TesterCodePaused{RetryAfter: pause}
	}
	return true, &callrules.TesterCodeIncorrect{AttemptsLeft: left}
}

func testerAccessUnavailable() error {
	return callrules.Refuse(callrules.ErrForbidden, callrules.ReasonTesterAccessUnavailable, "simulated results are switched off for this site")
}
