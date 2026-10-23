package pipelinerun

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestMaintenanceRecoversTheSameOwnerBoundAnalysisAfterLostResponse(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	a := workflowSource(t, `@template
automation pipelineMaintenancePolicy {
 args { event string! conclusion string! failureCode string! attempt int! repository string! sha string! }
 return {action: "analysis", template: "installedRepairAnalysis", reason: "Inspect the failed check evidence before proposing a repair."}
}`)
	calls := []MaintenanceRequest{}
	dh.integ.Configure(func(d *Deps) {
		bindTestWorkflow(d, a)
		d.OpenMaintenanceGoal = func(ctx context.Context, r MaintenanceRequest) (string, error) {
			owner, err := personFrom(ctx)
			if err != nil || owner != r.OwnerUserID {
				t.Fatalf("wrong analysis actor: %q %v", owner, err)
			}
			calls = append(calls, r)
			if len(calls) == 1 {
				return "", fmt.Errorf("response lost after durable goal creation")
			}
			return "stable-analysis-goal", nil
		}
	})
	r := dh.openRun(t, prOpening())
	r.Status = StatusCompleted
	r.Conclusion = ConclusionFailure
	r.FinishedAt = time.Now()
	dh.store.addRun(r)
	d := dh.integ.snapshot()
	if err := dh.integ.reconcileMaintenance(context.Background(), d, r.ID); err == nil {
		t.Fatal("lost response was hidden")
	}
	pending, _ := dh.store.run(r.ID)
	if pending.MaintenanceState != "pending" {
		t.Fatalf("not durable pending: %+v", pending)
	}
	if err := dh.integ.recoverMaintenance(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].Key != calls[1].Key || calls[0].OwnerUserID != r.OwnerUserID || calls[0].Input["sha"] != r.SHA {
		t.Fatalf("analysis identity drifted: %+v", calls)
	}
	finished, _ := dh.store.run(r.ID)
	if finished.MaintenanceState != "analysis" || finished.MaintenanceGoalID != "stable-analysis-goal" {
		t.Fatalf("missing receipt: %+v", finished)
	}
	if err := dh.integ.recoverMaintenance(context.Background(), d); err != nil || len(calls) != 2 {
		t.Fatalf("completed analysis repeated: %v", err)
	}
}

func TestMaintenanceNeverStartsAnotherAutomaticRepairForAFailedRetry(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	a := workflowSource(t, `@template
automation pipelineMaintenancePolicy {
 args { event string! conclusion string! failureCode string! attempt int! repository string! sha string! }
 return {action: "analysis", template: "repair", reason: "Inspect the failure."}
}`)
	dh.integ.Configure(func(d *Deps) {
		bindTestWorkflow(d, a)
		d.OpenMaintenanceGoal = func(context.Context, MaintenanceRequest) (string, error) {
			t.Fatal("repeat analysis admitted")
			return "", nil
		}
	})
	r := dh.openRun(t, prOpening())
	r.Status = StatusCompleted
	r.Conclusion = ConclusionFailure
	r.Attempt = 2
	r.FinishedAt = time.Now()
	dh.store.addRun(r)
	if err := dh.integ.reconcileMaintenance(context.Background(), dh.integ.snapshot(), r.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := dh.store.run(r.ID)
	if got.MaintenanceState != "human" {
		t.Fatalf("retry not escalated: %+v", got)
	}
}
