package logstore

import (
	"context"
	"errors"

	"github.com/znasllc-io/memql/core/logger"
)

// ErrNoStore is returned by WriteLines when this node has no sink -- no
// database, or not yet booted. The caller answers with a sentence rather
// than a count: the OS drops and counts, and a browser cannot retry into a
// node that keeps no lines.
var ErrNoStore = errors.New("logstore: this node keeps no log lines (no database, or the store has not started)")

// WriteResult is what an explicit write answers: how many lines the queue
// took and how many it dropped, by the sink's own reasons.
type WriteResult struct {
	Accepted int
	Dropped  int
}

// WriteLines enqueues lines carrying an explicit stamp, through the same
// queue and bucket the engine's own lines use. The OS write path (design E)
// is its caller: nodeType=os, node blank, the tab session and the actor's
// user id. The queue never blocks; a line the bucket or the queue refuses
// is counted under its reason and reported as dropped here.
//
// The context is accepted for the signature's sake and not waited on -- a
// non-blocking enqueue has nothing to cancel.
func WriteLines(_ context.Context, lines []logger.Line, st Stamp) (WriteResult, error) {
	s := Current()
	if s == nil {
		return WriteResult{Dropped: len(lines)}, ErrNoStore
	}
	return s.WriteLines(lines, st), nil
}

// WriteLines is the instance form of the package function.
func (s *Sink) WriteLines(lines []logger.Line, st Stamp) WriteResult {
	var res WriteResult
	for _, l := range lines {
		if s.WriteStamped(l, st) {
			res.Accepted++
		} else {
			res.Dropped++
		}
	}
	return res
}
