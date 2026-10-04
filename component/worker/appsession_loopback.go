package worker

import (
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// appsession_loopback.go provides an in-process AppSessionHandle for code
// that stands in for a machine's app harness.
//
// WHY THIS IS NOT IN A _test.go FILE, for NewModelCallLoopback's reason: an
// AppSessionFunc is installed on a *Worker by whoever owns the stream, the
// only in-tree owner is the gRPC server here, and the real harness lives in
// the memql-cockpit repo. A test of the ENGINE side of an app session -- the
// cross-replica app-call hop in integrations/agent/worker is the one that
// needed it -- has to supply an AppSessionFunc from another package and cannot
// reach the unexported handle constructor. Before this existed, every such
// test could only watch a session be REFUSED at the start, because a refused
// start was the one outcome a stand-in could produce; a session that ran and
// answered could not be tested outside this package at all.
//
// Deliberately not a general-purpose constructor: it hands back exactly what
// a stand-in harness needs -- the handle to return, a sink for output, and a
// way to end -- and nothing that reaches inside the handle's invariants.

// NewAppSessionLoopback returns a handle driven directly rather than by a
// worker stream, with the two functions a stand-in harness uses: `emit`
// delivers one chunk (subject to the seq discipline a real one is), and
// `finish` ends the session with its outcome.
//
// Control is observable: every AppSessionControl the engine sends -- a cancel
// when the caller gives up, a credential renewal -- reaches onControl, so a
// stand-in can end the session the way a harness told to stop would.
func NewAppSessionLoopback(req AppSessionRequest, onControl func(*memqlv1.AppSessionControl)) (
	handle *AppSessionHandle,
	emit func(AppSessionChunk),
	finish func(AppSessionOutcome),
) {
	h := &AppSessionHandle{
		sessionId: req.SessionId,
		app:       req.App,
		chunks:    make(chan AppSessionChunk, appSessionChunkBuffer),
		done:      make(chan struct{}),
	}
	h.control = func(c *memqlv1.AppSessionControl) error {
		if onControl != nil {
			onControl(c)
		}
		return nil
	}
	return h, h.deliverChunk, func(out AppSessionOutcome) { h.finish(out, nil) }
}
