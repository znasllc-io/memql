package pipelines

import (
	"fmt"
	"strings"
	"time"
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

// ScheduledRunKey adds the UTC day to a scheduled run's identity. A daily
// scan must run again when the branch head is unchanged, while duplicate
// scheduler deliveries on the same day must collapse to one run.
func ScheduledRunKey(repository, sha string, mode Mode, day string) (string, error) {
	if mode != ModeFull {
		return "", fmt.Errorf("pipelines: scheduled run mode %q must be full", mode)
	}
	parsed, err := time.Parse("2006-01-02", day)
	if err != nil || parsed.Format("2006-01-02") != day {
		return "", fmt.Errorf("pipelines: scheduled run day %q must be YYYY-MM-DD", day)
	}
	return RunKey(repository, sha, mode, EventSchedule) + ":" + day, nil
}

// ScheduledDayFromRunKey recovers the day a manual re-run must preserve.
func ScheduledDayFromRunKey(key string) (string, bool) {
	base, day, ok := strings.Cut(key, ":schedule:")
	if !ok || strings.Contains(day, ":") || !strings.HasSuffix(base, ":full") || !strings.Contains(base, "@") {
		return "", false
	}
	parsed, err := time.Parse("2006-01-02", day)
	if err != nil || parsed.Format("2006-01-02") != day {
		return "", false
	}
	return day, true
}
