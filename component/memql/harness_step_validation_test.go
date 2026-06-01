package memql

import (
	"fmt"
	"testing"
)

// TestStepTransitionAllowed covers the full v1:harness:step status
// state machine (issue #582), including the retry (failed -> ready)
// and blocked (running -> blocked, blocked -> ready) paths. Pure
// logic, no database -- mirrors humanCapExceeded's unit coverage in
// cognition_participant_validation_test.go.
func TestStepTransitionAllowed(t *testing.T) {
	cases := []struct {
		from string
		to   string
		want bool
		note string
	}{
		// Happy-path forward edges.
		{stepStatusPending, stepStatusReady, true, "dependsOn satisfied"},
		{stepStatusReady, stepStatusRunning, true, "controller claims"},
		{stepStatusRunning, stepStatusDone, true, "result recorded"},
		{stepStatusRunning, stepStatusFailed, true, "error, attempts exhausted"},
		{stepStatusRunning, stepStatusBlocked, true, "needs another step"},
		// Blocked path.
		{stepStatusBlocked, stepStatusReady, true, "blocker done"},
		// Retry path.
		{stepStatusFailed, stepStatusReady, true, "retry, attempt++"},
		// Self-transitions are always allowed (non-status field updates
		// re-version the same row).
		{stepStatusPending, stepStatusPending, true, "self pending"},
		{stepStatusRunning, stepStatusRunning, true, "self running (stamp result mid-run)"},
		{stepStatusDone, stepStatusDone, true, "self done"},
		// Empty `from` (no prior row): no constraint here -- the
		// fresh-insert pending check lives in the DB-bound guard.
		{"", stepStatusPending, true, "no prior row"},
		{"", stepStatusRunning, true, "no prior row, constraint applied elsewhere"},

		// Invalid transitions -- the heart of the acceptance criteria.
		{stepStatusDone, stepStatusRunning, false, "done -> running is rejected"},
		{stepStatusDone, stepStatusReady, false, "done is terminal"},
		{stepStatusDone, stepStatusPending, false, "done is terminal"},
		{stepStatusDone, stepStatusFailed, false, "done is terminal"},
		{stepStatusPending, stepStatusRunning, false, "must go through ready"},
		{stepStatusPending, stepStatusDone, false, "must run first"},
		{stepStatusReady, stepStatusDone, false, "must run first"},
		{stepStatusReady, stepStatusBlocked, false, "only running can block"},
		{stepStatusFailed, stepStatusRunning, false, "retry must re-enter via ready"},
		{stepStatusFailed, stepStatusDone, false, "cannot complete a failed step directly"},
		{stepStatusBlocked, stepStatusRunning, false, "unblock re-enters via ready"},
		{stepStatusBlocked, stepStatusDone, false, "blocked cannot complete directly"},
		{stepStatusRunning, stepStatusReady, false, "cannot un-claim a running step"},
		{stepStatusRunning, stepStatusPending, false, "cannot reset a running step"},

		// Unknown statuses are rejected.
		{stepStatusRunning, "bogus", false, "unknown target status"},
		{"bogus", stepStatusReady, false, "unknown source status"},
	}

	for _, c := range cases {
		t.Run(fmt.Sprintf("%s_to_%s", emptyAsDash(c.from), c.to), func(t *testing.T) {
			got := stepTransitionAllowed(c.from, c.to)
			if got != c.want {
				t.Errorf("stepTransitionAllowed(%q, %q) = %v, want %v (%s)",
					c.from, c.to, got, c.want, c.note)
			}
		})
	}
}

// TestStepTransitionFullReachability asserts the state machine has no
// edges beyond the documented ones: every (from, to) pair not in the
// allowed set is rejected (self-transitions and empty-from excepted).
func TestStepTransitionFullReachability(t *testing.T) {
	statuses := []string{
		stepStatusPending, stepStatusReady, stepStatusRunning,
		stepStatusBlocked, stepStatusDone, stepStatusFailed,
	}
	for _, from := range statuses {
		for _, to := range statuses {
			want := from == to || stepTransitions[from][to]
			got := stepTransitionAllowed(from, to)
			if got != want {
				t.Errorf("stepTransitionAllowed(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
}

// TestRetryBumpsAttempt covers the retry-counter invariant: a
// failed -> ready transition must advance attempt; every other
// transition is unconstrained.
func TestRetryBumpsAttempt(t *testing.T) {
	cases := []struct {
		from         string
		to           string
		priorAttempt int
		newAttempt   int
		want         bool
		note         string
	}{
		{stepStatusFailed, stepStatusReady, 0, 1, true, "retry bumps 0 -> 1"},
		{stepStatusFailed, stepStatusReady, 2, 3, true, "retry bumps 2 -> 3"},
		{stepStatusFailed, stepStatusReady, 1, 1, false, "retry did not bump"},
		{stepStatusFailed, stepStatusReady, 3, 2, false, "retry went backwards"},
		// Non-retry transitions never constrain attempt.
		{stepStatusReady, stepStatusRunning, 5, 5, true, "non-retry, attempt unchanged"},
		{stepStatusRunning, stepStatusDone, 1, 1, true, "non-retry, attempt unchanged"},
		{stepStatusPending, stepStatusReady, 0, 0, true, "non-retry, attempt unchanged"},
	}
	for _, c := range cases {
		t.Run(c.note, func(t *testing.T) {
			got := retryBumpsAttempt(c.from, c.to, c.priorAttempt, c.newAttempt)
			if got != c.want {
				t.Errorf("retryBumpsAttempt(%q, %q, %d, %d) = %v, want %v (%s)",
					c.from, c.to, c.priorAttempt, c.newAttempt, got, c.want, c.note)
			}
		})
	}
}

// TestValidStepStatus guards the enum/state-machine lockstep.
func TestValidStepStatus(t *testing.T) {
	valid := []string{
		stepStatusPending, stepStatusReady, stepStatusRunning,
		stepStatusBlocked, stepStatusDone, stepStatusFailed,
	}
	for _, s := range valid {
		if !validStepStatus(s) {
			t.Errorf("validStepStatus(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "open", "cancelled", "queued", "succeeded"} {
		if validStepStatus(s) {
			t.Errorf("validStepStatus(%q) = true, want false", s)
		}
	}
}

func emptyAsDash(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
