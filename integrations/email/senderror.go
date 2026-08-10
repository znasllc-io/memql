package email

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// senderror.go -- a typed send failure, so a caller can tell the three
// cases apart (memql#3348).
//
// # Why an untyped error was not enough
//
// Before this, every Graph failure came back as one `fmt.Errorf("graph:
// sendMail %d: %s", ...)`. A caller could retry it or not, and had no way
// to distinguish:
//
//   - PERMANENT -- a malformed recipient, a refused mailbox. Retrying is
//     pure cost and, on a bulk send, retries the same address on every
//     subsequent batch forever.
//   - THROTTLED -- HTTP 429 or 503 with a `Retry-After`. The provider has
//     said, in a header, exactly how long to wait. Ignoring it is the
//     failure mode this issue names: the send loop keeps hammering, the
//     provider keeps refusing, and the retries eventually land as
//     DUPLICATES in the recipient's mailbox because some of the refused
//     attempts were in fact accepted.
//   - RETRYABLE -- a 5xx or a transport error. Back off and try again.
//
// The distinction has to be made where the response is read, because that
// is the only place the status code and the `Retry-After` header exist.
// Anywhere further out it has already been flattened to a string, and
// re-deriving it by matching on message text is the drift this type exists
// to prevent.

// SendError is a classified delivery failure from a Sender.
type SendError struct {
	// StatusCode is the provider's HTTP status, or 0 for a transport
	// error that never reached one.
	StatusCode int
	// RetryAfter is how long the provider asked us to wait. Zero when it
	// did not say, INCLUDING on a throttle -- a 429 without the header is
	// still a throttle, and the caller supplies its own default rather
	// than reading zero as "retry immediately".
	RetryAfter time.Duration
	// Throttled reports that the provider refused because of rate
	// limiting specifically, rather than because of the message.
	Throttled bool
	// Permanent reports that retrying this exact message cannot succeed.
	Permanent bool
	// Detail is the provider's own message, truncated by the caller
	// before it is persisted.
	Detail string
}

func (e *SendError) Error() string {
	switch {
	case e.Throttled && e.RetryAfter > 0:
		return fmt.Sprintf("email: provider throttled (status %d, retry after %s): %s", e.StatusCode, e.RetryAfter, e.Detail)
	case e.Throttled:
		return fmt.Sprintf("email: provider throttled (status %d, no Retry-After): %s", e.StatusCode, e.Detail)
	case e.Permanent:
		return fmt.Sprintf("email: permanent failure (status %d): %s", e.StatusCode, e.Detail)
	default:
		return fmt.Sprintf("email: delivery failed (status %d): %s", e.StatusCode, e.Detail)
	}
}

// IsPermanent reports whether err is a SendError the caller must not
// retry. A non-SendError is NEVER permanent: an unclassified failure is
// treated as retryable, because giving up on an error nobody classified
// discards mail, while retrying one costs an attempt.
func IsPermanent(err error) bool {
	var se *SendError
	return errors.As(err, &se) && se.Permanent
}

// IsThrottled reports whether err is a provider rate-limit refusal, and
// returns the requested wait when the provider supplied one.
func IsThrottled(err error) (time.Duration, bool) {
	var se *SendError
	if errors.As(err, &se) && se.Throttled {
		return se.RetryAfter, true
	}
	return 0, false
}

// classifyHTTPSend turns a provider HTTP response into a SendError.
//
// 429 and 503 are the throttle pair: 429 is the documented Microsoft Graph
// throttling response, and 503 with a Retry-After is how a service says
// "shedding load, come back". A 503 WITHOUT Retry-After is a plain
// retryable failure rather than a throttle, because there is no signal to
// pace against and treating it as one would stall the whole send on an
// arbitrary default.
//
// 408 is retryable (a timeout is not the message's fault). Every other 4xx
// is permanent: the request was understood and refused, and re-sending the
// same bytes gets the same refusal. That includes 401 -- the Graph sender
// retries a stale token internally before it ever reaches here, so a 401
// arriving at this point means the credential itself is wrong, and
// retrying it per-recipient across a whole audience is a lockout risk
// rather than a recovery.
func classifyHTTPSend(status int, retryAfter string, detail string) *SendError {
	se := &SendError{StatusCode: status, Detail: detail}
	wait := parseRetryAfter(retryAfter)
	switch {
	case status == http.StatusTooManyRequests:
		se.Throttled = true
		se.RetryAfter = wait
	case status == http.StatusServiceUnavailable && wait > 0:
		se.Throttled = true
		se.RetryAfter = wait
	case status == http.StatusRequestTimeout, status >= 500:
		// retryable; zero value
	case status >= 400:
		se.Permanent = true
	}
	return se
}

// parseRetryAfter reads the RFC 7231 header in both of its forms:
// delta-seconds, and an HTTP-date. A negative or unparseable value is
// zero, which the caller reads as "the provider did not say".
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}
