package pipelinerun

import (
	"context"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

type unavailableQueueRef struct{ GitHub }

func (g unavailableQueueRef) CommitForRef(context.Context, string, string, string) (string, string, error) {
	return "", "", fmt.Errorf("provider unavailable")
}

func TestQueueReconciliationUsesTheExactRefAndNeverCancelsOnUncertainty(t *testing.T) {
	for _, state := range []string{"present", "absent", "moved", "unknown"} {
		t.Run(state, func(t *testing.T) {
			dh := newDriveHarness(t, driveManifest)
			branch := "gh-readonly-queue/main/pr-42"
			r := dh.openRun(t, Opening{Event: pipelines.EventMergeGroup, SHA: shaA, Branch: branch, Trigger: TriggerWebhook})
			switch state {
			case "present":
				dh.github.commits[repoName+"@refs/heads/"+branch] = headAnswer{SHA: shaA}
			case "moved":
				dh.github.commits[repoName+"@refs/heads/"+branch] = headAnswer{SHA: shaB}
			}
			d := dh.integ.snapshot()
			if state == "unknown" {
				d.GitHub = unavailableQueueRef{d.GitHub}
			}
			cancelled, err := dh.integ.reconcileQueueRun(context.Background(), d, r)
			want := state == "absent" || state == "moved"
			if err != nil || cancelled != want {
				t.Fatalf("state %s: cancel=%v err=%v", state, cancelled, err)
			}
			got, _ := dh.store.run(r.ID)
			if want && got.CancelledBy != queueInvalidatedBy {
				t.Fatalf("missing cancellation evidence: %+v", got)
			}
		})
	}
}

func TestRestoredQueueRefReopensOnlyItsSystemInvalidatedAttempt(t *testing.T) {
	for _, state := range []string{"restored", "absent", "moved", "unknown", "manually cancelled"} {
		t.Run(state, func(t *testing.T) {
			dh := newDriveHarness(t, driveManifest)
			branch := "gh-readonly-queue/main/pr-42"
			o := Opening{Event: pipelines.EventMergeGroup, SHA: shaA, Branch: branch, Trigger: TriggerWebhook}
			r := dh.openRun(t, o)
			d := dh.integ.snapshot()
			by := queueInvalidatedBy
			if state == "manually cancelled" {
				by = "owner"
			}
			if _, err := dh.integ.requestCancel(context.Background(), d, r.ID, by); err != nil {
				t.Fatal(err)
			}
			if state == "restored" || state == "manually cancelled" {
				dh.github.commits[repoName+"@refs/heads/"+branch] = headAnswer{SHA: shaA}
			}
			if state == "moved" {
				dh.github.commits[repoName+"@refs/heads/"+branch] = headAnswer{SHA: shaB}
			}
			if state == "unknown" {
				d.GitHub = unavailableQueueRef{d.GitHub}
			}
			result, err := dh.integ.open(context.Background(), d, dh.p, o)
			if err != nil {
				t.Fatal(err)
			}
			if result.Opened != (state == "restored") {
				t.Fatalf("%s: opened=%v", state, result.Opened)
			}
			if state == "restored" {
				if result.Run.Attempt != r.Attempt+1 || result.Run.ID == r.ID {
					t.Fatalf("restoration lost its attempt history: %+v", result.Run)
				}
				again, err := dh.peer("agent-b").open(context.Background(), d, dh.p, o)
				if err != nil || again.Opened || again.Run.ID != result.Run.ID {
					t.Fatalf("another replica duplicated restored attempt: %+v %v", again, err)
				}
			}
		})
	}
}
