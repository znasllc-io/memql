package worker

import (
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// modelcall_loopback.go provides an in-process ModelCallHandle for code
// that stands in for a machine's runtime.
//
// WHY THIS IS NOT IN A _test.go FILE. The seam it serves is
// CROSS-PACKAGE: a ModelCallFunc is installed on a *Worker by whoever
// owns the stream, and the only in-tree owner is the gRPC server here
// -- the real runtime lives in the memql-cockpit repo. So a test that
// exercises the ENGINE side of a model call (the cross-replica hop in
// integrations/agent/worker being the one that matters) has to supply
// a ModelCallFunc from another package, and cannot reach an unexported
// constructor. This is the httptest argument, arrived at for the same
// reason: the fake belongs beside the contract it fakes.
//
// It is deliberately not a general-purpose constructor. It hands back
// exactly the three things a stand-in runtime needs -- the handle to
// return, a sink for deltas, and a way to finish -- and nothing that
// would let a caller reach inside the handle's invariants.

// NewModelCallLoopback returns a handle driven directly rather than by
// a worker stream, along with the two functions a stand-in runtime
// uses: `emit` delivers one delta (subject to the same seq discipline
// a real one is), and `finish` closes the call.
//
// Cancellation is observable: the returned handle's Cancel writes into
// the context the caller passed to its runtime, so a stand-in that
// parks on ctx.Done() behaves like a real one being told to stop.
func NewModelCallLoopback(req ModelCallRequest, onCancel func(reason string)) (
	handle *ModelCallHandle,
	emit func(ModelCallDelta),
	finish func(ModelCallOutcome),
) {
	limits := req.Limits.withDefaults()
	h := &ModelCallHandle{
		requestId:    req.RequestId,
		limits:       limits,
		deltas:       make(chan ModelCallDelta, modelDeltaBuffer),
		done:         make(chan struct{}),
		clock:        time.Now,
		lastActivity: time.Now(),
	}
	h.cancelFn = func(c *memqlv1.ModelCallCancel) error {
		if onCancel != nil {
			onCancel(c.GetReason())
		}
		return nil
	}
	return h, h.deliverDelta, func(out ModelCallOutcome) { h.finish(out, nil) }
}
