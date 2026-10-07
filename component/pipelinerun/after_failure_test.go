package pipelinerun

import (
	"context"
	"slices"
	"testing"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/pipelines"
)

const afterFailureManifest = driveManifest + `
    - name: analysis
      runAfterFailure: true
      steps:
        - name: scan
          run: security-scanner
    - name: publish
      steps:
        - name: release
          run: publish
`

func TestAfterFailureAnalysisPreservesTheFailureAndBlocksPublication(t *testing.T) {
	dh := newDriveHarness(t, afterFailureManifest)
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		res := passed(req)
		if req.StepKey == "checks.vet" {
			res.Status, res.ExitCode = pipelines.OutcomeFailed, 1
		}
		return res, nil
	}
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	if keys := dh.exec.sentKeys(); !slices.Equal(keys, []string{"checks.vet", "analysis.scan"}) {
		t.Fatalf("analysis must run, but publication must stay blocked: %v", keys)
	}
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionFailure {
		t.Fatalf("passing analysis erased a failed check: %s", got.Conclusion)
	}
	r := dh.work.receiptsOf("publish.release")
	if len(r) != 1 || argString(r[0].Args, "errorCode") != pipelines.CodeStageBlocked {
		t.Fatalf("publication was not blocked: %+v", r)
	}
}

func TestAfterFailureStageDoesNotBypassCancellation(t *testing.T) {
	dh := newDriveHarness(t, afterFailureManifest)
	release := make(chan struct{})
	defer close(release)
	blockTests(dh, release)
	run := dh.openRun(t, prOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "tests.unit", "tests.lint")
	if err := dh.integ.RequestCancel(context.Background(), run.ID, "v1:identity:user:"+ownerID); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, dh.integ)
	if slices.Contains(dh.exec.sentKeys(), "analysis.scan") {
		t.Fatal("analysis started after cancellation")
	}
	if got, _ := dh.store.run(run.ID); got.Conclusion != ConclusionCancelled {
		t.Fatalf("cancelled run concluded %s", got.Conclusion)
	}
}
