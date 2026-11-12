package sync

import (
	"errors"
	"fmt"
	"strings"
)

// notgranted.go -- the domain the origin will not let this deployment read.
//
// A connector declares every domain it CAN mirror; whether a particular
// deployment MAY read one is the origin's decision, made when the operator
// granted the connection its access scopes. A store connected with
// read_products and read_orders has not granted read_content, and its blogs,
// articles, pages and menus are not a failure of anything -- they are
// domains this deployment does not have. Sweeping them asks the origin a
// question it has already answered, once per tick, forever: 22 domains on
// one store were refused every ten minutes with the same ACCESS_DENIED.
//
// The runtime treats this as it treats ErrNotImplemented -- a configuration
// fact, skipped rather than failed -- with one difference: it is RECORDED on
// the domain's health row, once, so the Data origins page says why the
// domain is idle and what granting would change. A missing scope is
// something an operator can act on; a connector that does not reconcile is
// not.

// ErrNotGranted marks a domain the origin refuses this deployment on
// standing grounds (an access scope it was never granted).
var ErrNotGranted = errors.New("connector: not granted")

// NotGranted names the scopes a store lacks for a domain, so the health row
// reads "not granted: needs read_content" rather than a bare verdict.
func NotGranted(connector string, missingScopes ...string) error {
	if len(missingScopes) == 0 {
		return fmt.Errorf("%w: %s did not grant this domain", ErrNotGranted, connector)
	}
	return fmt.Errorf("%w: %s needs %s", ErrNotGranted, connector, strings.Join(missingScopes, ", "))
}

// IsNotGranted reports whether a failure is a standing refusal by the
// origin rather than something a retry could change.
func IsNotGranted(err error) bool { return errors.Is(err, ErrNotGranted) }

// ErrNotRequested marks a domain outside the store's own connection: the
// connection never asked the origin for it, so the origin's refusal is not
// something to grant. A shared app's connection asks for a fixed set, and a
// domain outside that set is neither a failure nor a missing scope -- it is
// recorded like a not-granted domain and never shown as one.
var ErrNotRequested = errors.New("connector: not requested")

// NotRequested says a connector's connection does not ask for a domain.
func NotRequested(connector string) error {
	return fmt.Errorf("%w: %s's connection does not ask for this domain", ErrNotRequested, connector)
}

// IsNotRequested reports whether a refusal is of a domain the connection
// never asked for. It is never IsNotGranted.
func IsNotRequested(err error) bool { return errors.Is(err, ErrNotRequested) }

// ErrBusy marks an origin that cannot take the request NOW because it is
// doing the same kind of work already -- Shopify runs one bulk query per shop
// at a time. Unlike a refusal it says nothing about the domain: the same
// request succeeds once the other work finishes, so the runtime records no
// error and asks again on its next tick.
var ErrBusy = errors.New("connector: busy")

// Busy says the origin cannot take the request now, and why: it is busy with
// other work of the same kind, or -- the reason says which -- the tenant asked
// about does not take the request at present. The reason is the connector's
// own sentence and is kept apart from the words around it (BusyReason), so
// whoever asked can be told it and not a guess.
func Busy(connector, reason string) error {
	return &busy{connector: connector, reason: strings.TrimSpace(reason)}
}

// busy is ErrBusy with the connector's reason.
type busy struct{ connector, reason string }

func (b *busy) Error() string {
	if b.reason == "" {
		return fmt.Sprintf("%s: %s is busy with another operation", ErrBusy.Error(), b.connector)
	}
	return fmt.Sprintf("%s: %s: %s", ErrBusy.Error(), b.connector, b.reason)
}

func (b *busy) Unwrap() error { return ErrBusy }

// IsBusy reports whether a failure is the origin being busy, which a later
// attempt can change.
func IsBusy(err error) bool { return errors.Is(err, ErrBusy) }

// BusyReason is the reason a busy answer carries, in the connector's own
// words: "" when the connector gave none, and for an error that is not a busy
// answer.
func BusyReason(err error) string {
	var b *busy
	if errors.As(err, &b) {
		return b.reason
	}
	return ""
}
