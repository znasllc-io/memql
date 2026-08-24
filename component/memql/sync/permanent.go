package sync

import (
	"errors"
	"fmt"
)

// permanent.go -- the failure a retry cannot fix.
//
// # Why the drain needs to be told
//
// The drain's default is to retry with backoff and dead-letter after a
// bounded number of attempts, which is right for a timeout, a 5xx or a
// rate limit. It is wrong for a VALIDATION failure: a receiver that
// refused a payload as malformed will refuse it identically forever, so
// every attempt is spent, every backoff is waited, and the entry
// dead-letters at the ceiling having taught nobody anything -- while the
// entries behind it wait.
//
// Shopify makes this concrete and common. Its GraphQL mutations report
// validation failures as `userErrors` INSIDE a 200 response: the HTTP
// status is success, the errors array is empty, and the refusal is in
// the data. A connector that could not distinguish that from a network
// blip would turn one bad metafield value into MaxAttempts round trips.
//
// # Why a typed error rather than a result field
//
// ErrNotImplemented is already a typed error the drain switches on, and
// this is the same kind of statement about the same call. A field on
// PropagateResult would also have to be set on the ERROR path, where a
// connector returns a zero result -- so it would be a flag that has to
// be remembered exactly where the failure is.

// ErrPermanent marks a failure that will recur identically on every
// retry. The drain dead-letters it immediately rather than spending its
// attempt budget.
var ErrPermanent = errors.New("connector: permanent failure")

// Permanent wraps a failure as one no retry can fix, naming what the far
// end said so an operator reading the dead-letter learns the reason
// rather than only the verdict.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrPermanent, err)
}

// Permanentf wraps a described permanent failure.
func Permanentf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPermanent, fmt.Sprintf(format, args...))
}

// IsPermanent reports whether a failure should dead-letter immediately.
func IsPermanent(err error) bool { return errors.Is(err, ErrPermanent) }
