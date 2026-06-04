package agent

import (
	"strings"
	"sync"
)

// pause.go -- cooperative preemption for the background execution lane
// (memql#906, epic #902).
//
// A running background task can be "passed" (preempted) so another task can
// take its slot. The model call itself can't be interrupted mid-token, so
// preemption is cooperative: it happens at a CHECKPOINT -- the boundary
// between tool rounds in runNonStreamingToolLoop. This file is the agent-
// node-local signal the loop consults at each checkpoint.
//
// Ownership split with epic #902 (Session B):
//   - Session A (here): the requestId-keyed pause registry + the loop's
//     checkpoint check + persisting taskState on a paused exit.
//   - Session B: the planner-side "pass" action (which marks the Plan
//     paused -- freeing its slot via #905's handleSlotFreed) and the
//     cross-node envelope that, on the agent node, calls RequestPause with
//     the in-flight turn's requestId.
//
// The registry is process-local to the agent node by design: the in-flight
// tool loop runs on the agent node, so the signal it consults must live
// there. The cross-node hop (planner -> agent) is the envelope Session B
// wires; its agent-side landing calls RequestPause.

var (
	pauseMu       sync.Mutex
	pauseRequests = make(map[string]struct{})
)

// RequestPause flags the in-flight background turn identified by requestId
// to stop at its next checkpoint. Idempotent; a no-op for an empty id or a
// requestId with no live turn (the flag simply sits until ClearPause, or is
// observed if that id's turn starts later -- callers should only pause live
// turns). Safe for concurrent use.
//
// This is the entry point the agent-node handler for the planner's
// cross-node "stop at checkpoint" envelope calls (memql#906, wired by
// Session B).
func RequestPause(requestId string) {
	requestId = strings.TrimSpace(requestId)
	if requestId == "" {
		return
	}
	pauseMu.Lock()
	defer pauseMu.Unlock()
	pauseRequests[requestId] = struct{}{}
}

// PauseRequested reports whether a pause has been requested for requestId.
// runNonStreamingToolLoop calls this at each checkpoint boundary.
func PauseRequested(requestId string) bool {
	requestId = strings.TrimSpace(requestId)
	if requestId == "" {
		return false
	}
	pauseMu.Lock()
	defer pauseMu.Unlock()
	_, ok := pauseRequests[requestId]
	return ok
}

// ClearPause removes any pause flag for requestId. The loop calls this when
// a turn ends (success, failure, or pause) so a stale flag can never affect
// a later turn that happens to reuse the id. Also callable by the planner
// side if a queued "pass" is rescinded before the checkpoint observes it.
func ClearPause(requestId string) {
	requestId = strings.TrimSpace(requestId)
	if requestId == "" {
		return
	}
	pauseMu.Lock()
	defer pauseMu.Unlock()
	delete(pauseRequests, requestId)
}
