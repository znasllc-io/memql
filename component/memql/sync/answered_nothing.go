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
// did not announce a deletion, one such answer is not believed. A record the
// mirror holds may be briefly unreadable, and on a domain swept by what
// changed, nothing reads an unchanged record again once it is retired. A
// record the mirror does not hold yet may have been announced before it can be
// read, and one answer of nothing would spend the notice that announced it.
// The connector fails the delivery with this error instead, the retry lane
// tries it again, and the dispatcher tells the next try that the previous one
// ended this way (InboundRequest.AnsweredNothingBefore). Two such answers in a
// row are believed: a held record is retired, and one never held is not there
// to record.

// ErrAnsweredNothing is a clean answer from the origin that it has no such
// record, given once. It is worth another try. Its text is the only writer of
// the stored failure text the dispatcher recognises (AnsweredNothingBefore).
var ErrAnsweredNothing = errors.New("connector: the origin answered nothing for this record; it is asked once more before that answer is believed")

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
