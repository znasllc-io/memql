package memql

import (
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// ErrModelDeclined is a request the model, or a safety classifier in front of
// it, declined to answer.
//
// WHY IT IS AN ERROR. The vendor answers a declined request with HTTP 200 and
// stop reason "refusal", usually with no content at all. Read as an ordinary
// reply that is an empty answer recorded as served: no fallback runs, the
// decision says the call succeeded, and the person sees nothing. The models
// current on 2026-11-22 run classifiers that decline more often than the ones
// before them, and they are the first records the vendor selectors try. So a
// decline fails the call: the route's next source serves it, and when there is
// none the person is told the model declined.
//
// IT SAYS NOTHING ABOUT THE ACCOUNT THAT PAID. The next account at the same
// model would be declined the same, so it follows the route's ordinary
// fallback to the next source (VendorAccountFailure reports it as no account
// failure).
//
// What arrived before a decline on a stream was already handed to the caller;
// the stream then ends in this error, never in a clean Done.
//
// Read from the vendor's documentation, not observed against it.
// Regression tests: ai_anthropic_declined_test.go.
var ErrModelDeclined = errors.New("the model declined this request")

// anthropicDeclined turns a reply's stop reason into ErrModelDeclined when the
// request was declined, and nil otherwise.
func anthropicDeclined(stopReason string) error {
	if stopReason != string(anthropic.StopReasonRefusal) {
		return nil
	}
	return fmt.Errorf("anthropic: %w", ErrModelDeclined)
}
