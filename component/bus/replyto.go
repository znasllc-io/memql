package bus

import (
	"context"
	"errors"
	"time"

	busv1 "github.com/znasllc-io/memql/component/bus/gen"
)

var (
	// ErrTimeout is returned when a request-response exchange times out.
	ErrTimeout = errors.New("bus: request timed out")

	// ErrContextCancelled is returned when the context is cancelled during await.
	ErrContextCancelled = errors.New("bus: context cancelled")
)

// Request wraps an InternalMessage with a ReplyTo channel for
// synchronous request-response exchange over async channels.
//
// Usage pattern:
//
//	req := bus.NewRequest(msg)
//	if err := ch.SendBlocking(ctx, req); err != nil { ... }
//	resp, err := req.Await(ctx, 5*time.Second)
type Request struct {
	Msg     *busv1.InternalMessage
	ReplyTo chan *busv1.InternalMessage
}

// NewRequest creates a Request with a buffered ReplyTo channel (size 1).
// The buffer size of 1 ensures the responder never blocks when sending the reply.
func NewRequest(msg *busv1.InternalMessage) Request {
	return Request{
		Msg:     msg,
		ReplyTo: make(chan *busv1.InternalMessage, 1),
	}
}

// Await waits for a reply on the ReplyTo channel. It returns the response
// or an error if the context is cancelled or the timeout expires.
//
// If timeout is 0, only the context deadline is used.
func (r Request) Await(ctx context.Context, timeout time.Duration) (*busv1.InternalMessage, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	select {
	case resp := <-r.ReplyTo:
		return resp, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrTimeout
		}
		return nil, ErrContextCancelled
	}
}

// Reply sends a response back to the caller. This is non-blocking because
// the ReplyTo channel is buffered (size 1). If Reply is called more than
// once, subsequent calls are silently dropped.
func (r Request) Reply(msg *busv1.InternalMessage) {
	select {
	case r.ReplyTo <- msg:
	default:
		// Channel already has a response or is closed; drop silently.
	}
}
