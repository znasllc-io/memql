package pipelines

import (
	"sort"
	"strings"
)

// modes is the event-to-mode table (design record D1 and D5, plus `release`
// from the documentation program's D15). A pull request runs what it affects;
// everything that lands or ships runs the whole suite once, on the exact tree.
//
// A re-requested check run has no row: it is not an event of its own but a
// new attempt of the run it names, in that run's mode and event.
var modes = map[Event]Mode{
	EventPullRequest: ModeAffected,
	EventMergeGroup:  ModeFull,
	EventPush:        ModeFull,
	EventRelease:     ModeFull,
}

// ModeFor is the event-to-mode table, and false for an event it has no row
// for.
func ModeFor(e Event) (Mode, bool) {
	m, ok := modes[e]
	return m, ok
}

// Events is every event in the table, sorted.
func Events() []Event {
	out := make([]Event, 0, len(modes))
	for e := range modes {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
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

// Version is MEMQL_VERSION: the release's tag for a release, else the head
// SHA. A release whose tag is not known yet still names its commit.
func Version(event Event, sha, releaseTag string) string {
	if event == EventRelease && releaseTag != "" {
		return releaseTag
	}
	return sha
}
