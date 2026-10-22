//go:build agent

package app

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

func TestHumanResumeReceiptRequiresTheAuthorizedGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason, request string
		version, attempt, next        int
		want                          bool
	}{
		{"paused invocation", "waiting", "", "", 1, 1, 0, true},
		{"failed receipt before retry", "failed", "rerun", "request", 1, 1, 2, true},
		{"still unwinding", "running", "rerun", "request", 1, 1, 2, false},
		{"already completed", "done", "rerun", "request", 1, 1, 2, false},
		{"retry already failed", "failed", "rerun", "request", 2, 2, 2, false},
		{"attempt already ran", "failed", "rerun", "request", 1, 2, 2, false},
		{"no version authorization", "failed", "rerun", "request", 1, 1, 0, false},
		{"no request identity", "failed", "rerun", "", 1, 1, 2, false},
		{"another recovery kind", "failed", "replan", "request", 1, 1, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &automations.RunJournal{FailedStep: "deliver", StepStates: map[string]automations.StepState{"deliver": {Status: tc.status, Version: tc.version, Attempt: tc.attempt}}, Rerun: &automations.RerunSpec{Reason: tc.reason, RequestId: tc.request, StepKey: "deliver", Versions: map[string]int{"deliver": tc.next}}}
			if got := humanResumeReceiptReady(j); got != tc.want {
				t.Fatalf("ready=%v, want %v", got, tc.want)
			}
		})
	}
}
