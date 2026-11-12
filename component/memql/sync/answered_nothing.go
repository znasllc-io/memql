package sync

import (
	"errors"
	"fmt"
	"strings"
)

// answered_nothing.go -- a record the origin answered nothing for.
//
// A connector that mirrors the origin and reads one of its records back finds,
// now and then, a clean answer that the record is not there. When the origin
// did not announce the deletion, one such answer is not believed: a record
// briefly unreadable would be retired, and on a domain swept by what changed,
// nothing reads an unchanged record again. The connector fails the delivery
// with this error instead, the retry lane tries it again, and the dispatcher
// tells the next try that the previous one ended this way
// (InboundRequest.AnsweredNothingBefore). Two such answers in a row are a
// deletion.

// ErrAnsweredNothing is a clean answer from the origin that it has no record
// the mirror holds. It is worth another try. Its text is the only writer of
// the stored failure text the dispatcher recognises (AnsweredNothingBefore).
var ErrAnsweredNothing = errors.New("connector: the origin answered nothing for a record the mirror holds; it is asked again before the record is retired")

// AnsweredNothing is ErrAnsweredNothing for one record.
func AnsweredNothing(connector, record string) error {
	return fmt.Errorf("%w (%s %s)", ErrAnsweredNothing, connector, record)
}

// IsAnsweredNothing reports whether a failure is ErrAnsweredNothing.
func IsAnsweredNothing(err error) bool { return errors.Is(err, ErrAnsweredNothing) }

// AnsweredNothingBefore reports whether a stored failure text is that of
// ErrAnsweredNothing: the error's own text, which nothing else writes, at the
// start of what the dispatcher recorded.
func AnsweredNothingBefore(lastError string) bool {
	return strings.HasPrefix(strings.TrimSpace(lastError), ErrAnsweredNothing.Error())
}
