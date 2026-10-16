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

// ScheduleSlot binds a run to a UTC calendar period. A weekly slot starts on
// Monday; a missed Monday still opens that week's scan on the next invocation.
func ScheduleSlot(cadence string, now time.Time) (string, error) {
	now = now.UTC()
	switch cadence {
	case "daily":
		return now.Format("2006-01-02"), nil
	case "weekly":
		monday := now.AddDate(0, 0, -((int(now.Weekday()) + 6) % 7))
		return "week:" + monday.Format("2006-01-02"), nil
	default:
		return "", fmt.Errorf("pipelines: unsupported schedule cadence %q", cadence)
	}
}

func ScheduleCadence(slot string) (string, error) {
	cadence, day := "daily", slot
	if strings.HasPrefix(slot, "week:") {
		cadence, day = "weekly", strings.TrimPrefix(slot, "week:")
	}
	parsed, err := time.Parse("2006-01-02", day)
	if err != nil || parsed.Format("2006-01-02") != day || (cadence == "weekly" && parsed.Weekday() != time.Monday) {
		return "", fmt.Errorf("pipelines: schedule slot %q must be YYYY-MM-DD or week:YYYY-MM-DD naming a Monday", slot)
	}
	return cadence, nil
}

// Daily keys preserve their existing identity. Weekly keys deliberately omit
// the head SHA: the first opening pins that week's source, so later main pushes
// cannot turn a weekly deep scan into a daily scan. The same atomic opening gate
// deduplicates concurrent replicas; a manual retry retains the original SHA.
func ScheduledRunKey(repository, sha string, mode Mode, slot string) (string, error) {
	if mode != ModeFull {
		return "", fmt.Errorf("pipelines: scheduled run mode %q must be full", mode)
	}
	cadence, err := ScheduleCadence(slot)
	if err != nil {
		return "", err
	}
	if cadence == "weekly" {
		sha = "weekly"
	}
	return RunKey(repository, sha, mode, EventSchedule) + ":" + slot, nil
}

// ScheduledSlotFromRunKey recovers the calendar slot a retry must preserve.
func ScheduledSlotFromRunKey(key string) (string, bool) {
	base, slot, ok := strings.Cut(key, ":schedule:")
	if !ok || !strings.HasSuffix(base, ":full") || !strings.Contains(base, "@") {
		return "", false
	}
	if _, err := ScheduleCadence(slot); err != nil {
		return "", false
	}
	return slot, true
}
