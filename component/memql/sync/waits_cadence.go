package sync

import "errors"

// waits_cadence.go -- a failure that is asked again at its domain's own
// cadence, not on the next tick.
//
// A runner's ordinary failure is due again at the next tick: a timeout or a
// throttle may well be gone ten minutes later. Some failures are not like
// that and are not permanent either. A sweep that read rows the mirror could
// not write has to be read again -- its watermark must hold -- but a row the
// mirror refuses now will most likely be refused at the next tick too, and a
// sweep repeated every tick reads everything since the held watermark and
// spends the origin's budget doing it. Such a failure waits the domain's
// cadence, as a permanent one does, and is read again then.
//
// It is a wrapper and not a sentinel in the text, unlike ErrPermanent: the
// failure's own sentence is what a person reads on the domain's row, so the
// wrapper adds no words to it.

// waitsCadence is a failure that waits its domain's cadence. Its text is the
// failure's own.
type waitsCadence struct{ err error }

func (w waitsCadence) Error() string { return w.err.Error() }
func (w waitsCadence) Unwrap() error { return w.err }

// WaitsCadence marks a failure as one the runtime asks again at the domain's
// own cadence rather than at the next tick. It records the failure and the
// attempt and moves no watermark. The text is the failure's own, unchanged.
func WaitsCadence(err error) error {
	if err == nil {
		return nil
	}
	return waitsCadence{err: err}
}

// IsWaitingCadence reports whether a failure was marked WaitsCadence.
func IsWaitingCadence(err error) bool {
	var w waitsCadence
	return errors.As(err, &w)
}
