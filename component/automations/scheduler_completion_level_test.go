package automations

import (
	"bytes"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

// A run that ends skipped logs its completion at Debug, as a completed run
// does: a cluster-guard duplicate is a normal outcome (2026-10-11: 71,105 of
// them in six hours on an idle instance). The capturing logger records every
// level, so the level of the line is read straight off the scheduler's log.
func TestSchedulerCompletionLogsDebugForCompletedAndSkipped(t *testing.T) {
	cases := []struct {
		name     string
		claimWon bool
		status   string
	}{
		// The cluster guard's answer decides the executor's status: won
		// completes the run, lost skips it as a duplicate.
		{name: "completed", claimWon: true, status: "completed"},
		{name: "skipped", claimWon: false, status: "skipped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bus := events.NewBus()
			defer bus.Close()
			var buf bytes.Buffer
			s := newMinimalScheduler(&buf, bus)
			s.eventExecutor = NewExecutor(ExecutorOptions{
				Logger:                  s.logger,
				EventBus:                bus,
				ChainTrackingEnabled:    true,
				ClusterGuard:            &recordingClaimer{result: tc.claimWon},
				MaxConcurrentExecutions: 1,
			})
			a := &Automation{Name: "completionLevel", Trigger: &TriggerConfig{Event: "node.created"}}
			s.automations[a.Name] = a
			if err := s.subscribeToEventTrigger(a); err != nil {
				t.Fatalf("subscribeToEventTrigger: %v", err)
			}
			bus.PublishSync(events.NewEvent("node.created", events.KindNodeCreated, map[string]any{"id": "x"}))

			line := schedulerCompletionLine(buf.String())
			if !strings.Contains(line, "status="+tc.status) {
				t.Fatalf("want a completion line with status=%s, got %q; scheduler log:\n%s", tc.status, line, buf.String())
			}
			if !strings.Contains(line, "level=DEBUG") {
				t.Errorf("a %s run's completion line must log at DEBUG, got: %s", tc.status, line)
			}
		})
	}
}

// Every status other than completed and skipped keeps Info, failures and
// cancellations included. Driving a cancelled run through the scheduler needs
// a DB-backed cancel request, so the rule is pinned for each status the
// executor sets instead.
func TestCompletionLogLevelKeepsInfoForOtherStatuses(t *testing.T) {
	for _, status := range []string{"completed", "skipped"} {
		if !completionLogsAtDebug(status) {
			t.Errorf("status %q: the completion line must log at Debug", status)
		}
	}
	for _, status := range []string{"failed", "cancelled", "waiting", "running", ""} {
		if completionLogsAtDebug(status) {
			t.Errorf("status %q: the completion line must stay at Info", status)
		}
	}
}

// schedulerCompletionLine returns the scheduler's "automation execution
// completed" line from a captured log, or "" when there is none.
func schedulerCompletionLine(log string) string {
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, `msg="automation execution completed"`) {
			return line
		}
	}
	return ""
}
