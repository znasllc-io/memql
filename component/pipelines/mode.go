package pipelines

import (
	"strings"
)

// Events is the supported GitHub protocol event vocabulary. Run policy lives
// in pipelineModeForEvent, not in this protocol/model package.
func Events() []Event {
	return []Event{EventMergeGroup, EventPullRequest, EventPush, EventRelease}
}

// RunKey is the identity a run is deduplicated on: "owner/name@<sha>:<mode>:<event>".
//
// The event is part of it (a decision the pipelines-seam plan takes over the
// record's (repository, SHA, mode)): the merge queue's run and the default
// branch's push run are both full and share a SHA once merged, and without the
// event the push -- which carries the deploy and notify stages -- would
// collapse into the queue's run and never execute. A webhook and a poll for
// one head stay one run because the poll synthesizes the same event.
//
// Repository and SHA are lower-cased here, once, so two spellings of one head
// can never become two runs, whichever caller forgot to normalize.
func RunKey(repository, sha string, mode Mode, event Event) string {
	return strings.ToLower(strings.TrimSpace(repository)) + "@" +
		strings.ToLower(strings.TrimSpace(sha)) + ":" + string(mode) + ":" + string(event)
}
