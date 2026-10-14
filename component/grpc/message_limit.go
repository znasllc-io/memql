package memql

import (
	"errors"
	"fmt"
	"math"
	"strconv"

	"google.golang.org/grpc/codes"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// messageTooLargeError reports a server message that was measured, found over
// the stream's limit, and NOT written.
//
// It exists because grpc-go does not treat an oversize send as a failed call.
// SendMsg returns ResourceExhausted and then writes that status as the RPC's
// trailer, so the stream is finished: the browser bridge's Recv returns an
// error and the bridge closes the WebSocket, which takes every request in
// flight on that tab with it. The only way to fail one request instead of the
// connection is to never hand gRPC the message, so the size is checked first
// (sendServerMessage) and the refusal comes back as this typed error for the
// caller that knows what the message was an answer to.
type messageTooLargeError struct {
	size  int
	limit int
}

func (e *messageTooLargeError) Error() string {
	return fmt.Sprintf("message of %d bytes is over the %d-byte stream limit", e.size, e.limit)
}

// sentence is what the person whose request it was is told. Sizes round UP to
// a tenth of a MiB so that an answer one byte over the limit reads as over it
// ("32.1 MiB", not "32.0 MiB against a limit of 32 MiB"), and the last clause
// is the thing they can do about it.
func (e *messageTooLargeError) sentence() string {
	return fmt.Sprintf("This answer is too large to send: it is %s MiB and one answer can carry at most %d MiB. "+
		"Ask for fewer rows or fewer fields and try again.",
		mebibytesRoundedUp(e.size), e.limit>>20)
}

func mebibytesRoundedUp(n int) string {
	return strconv.FormatFloat(math.Ceil(float64(n)/(1<<20)*10)/10, 'f', 1, 64)
}

// sendQueryResult answers one request with its result, or, when the result is
// too large to send, with an error for that request alone. A refusal that was
// delivered returns nil: the request was answered, and the stream is fine.
func (s *streamSession) sendQueryResult(requestID, correlate string, result *memqlv1.Result) error {
	err := s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_QueryResult{
			QueryResult: &memqlv1.QueryResultChunk{RequestId: requestID, Result: result, Done: true},
		},
	})
	var tooLarge *messageTooLargeError
	if !errors.As(err, &tooLarge) {
		return err
	}
	if s.logger != nil {
		s.logger.Warn("query answer is too large to send; the request was answered with an error instead",
			"requestId", requestID, "bytes", tooLarge.size, "limit", tooLarge.limit)
	}
	return s.sendQueryErrorWithMetadata(requestID, correlate, codes.ResourceExhausted, tooLarge.sentence(), map[string]string{
		"code":       "RESULT_TOO_LARGE",
		"sizeBytes":  strconv.Itoa(tooLarge.size),
		"limitBytes": strconv.Itoa(tooLarge.limit),
	})
}
