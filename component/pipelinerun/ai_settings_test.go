package pipelinerun

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
)

// The pipeline AI switches (MEMQL_PIPELINES_AI) are off unless the owner turns
// them on in Pipelines > Settings > AI. These tests hold both gates; the other
// review and maintenance tests run with both switches on (bindTestWorkflow).

func withAISettings(d *Deps, value string) {
	next := d.SystemVariable
	d.SystemVariable = func(ctx context.Context, name string) (string, error) {
		if name == pipelines.AISettingsVariable {
			return value, nil
		}
		if next == nil {
			return "", nil
		}
		return next(ctx, name)
	}
}

func TestAIReviewOffLeavesTheRunToItsOwnChecks(t *testing.T) {
	for _, value := range []string{"", `{"review":false,"diagnosis":true}`, "not json"} {
		_, dr, f := independentReviewDriver(t)
		withAISettings(&dr.d, value)
		out, err := dr.independentReview(context.Background())
		if err != nil || out != nil {
			t.Fatalf("%q: the review was not skipped: %v, %v", value, out, err)
		}
		if f.model != 0 || f.reports != 0 || f.observes != 0 {
			t.Fatalf("%q: a model call or a review report ran: %+v", value, f)
		}
	}
}

func TestAIReviewOnStillReviews(t *testing.T) {
	_, dr, f := independentReviewDriver(t)
	withAISettings(&dr.d, `{"review":true}`)
	if _, err := dr.independentReview(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.model != 1 || f.reports != 1 {
		t.Fatalf("model %d, reports %d", f.model, f.reports)
	}
}

func TestAIDiagnosisOffOpensNoGoal(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	a := workflowSource(t, `@template
automation pipelineMaintenancePolicy {
 args { event string! conclusion string! failureCode string! attempt int! repository string! sha string! }
 return {action: "analysis", template: "installedRepairAnalysis", reason: "Inspect the failed check evidence before proposing a repair."}
}`)
	dh.integ.Configure(func(d *Deps) {
		bindTestWorkflow(d, a)
		withAISettings(d, "")
		d.OpenMaintenanceGoal = func(context.Context, MaintenanceRequest) (string, error) {
			t.Fatal("an AI analysis goal was opened while AI diagnosis is off")
			return "", nil
		}
	})
	r := dh.openRun(t, prOpening())
	r.Status = StatusCompleted
	r.Conclusion = ConclusionFailure
	r.FinishedAt = time.Now()
	dh.store.addRun(r)
	if err := dh.integ.reconcileMaintenance(context.Background(), dh.integ.snapshot(), r.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := dh.store.run(r.ID)
	if got.MaintenanceState != "none" || got.MaintenanceGoalID != "" || got.MaintenanceMessage != aiDiagnosisOffMessage {
		t.Fatalf("the failure was not recorded as reported without AI: %+v", got)
	}
}

func TestAIDiagnosisOffLeavesAHumanDispositionAlone(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	a := workflowSource(t, `@template
automation pipelineMaintenancePolicy {
 args { event string! conclusion string! failureCode string! attempt int! repository string! sha string! }
 return {action: "human", template: "", reason: "A person reviews this failure."}
}`)
	dh.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a); withAISettings(d, "") })
	r := dh.openRun(t, prOpening())
	r.Status = StatusCompleted
	r.Conclusion = ConclusionFailure
	r.FinishedAt = time.Now()
	dh.store.addRun(r)
	if err := dh.integ.reconcileMaintenance(context.Background(), dh.integ.snapshot(), r.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := dh.store.run(r.ID); got.MaintenanceState != "human" {
		t.Fatalf("a non-AI disposition changed: %+v", got)
	}
}
