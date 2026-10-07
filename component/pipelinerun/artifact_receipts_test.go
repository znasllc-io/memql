package pipelinerun

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
)

func TestArtifactIntentReferencesSurviveTheWorkJournalReceipt(t *testing.T) {
	id := strings.Repeat("a", 64)
	driver := &runDriver{}
	result := pipelines.StepResult{Status: pipelines.OutcomeSucceeded, Where: pipelines.Where{Surface: "cluster"}, ArtifactIntentIDs: []string{id}}
	receipt := driver.receiptFor(result, time.Second, true)
	result.ArtifactIntentIDs[0] = "changed after handoff"
	metadata := receipt.journal().Result["metadata"].(map[string]any)
	if receipt.status != WorkStepDone || !reflect.DeepEqual(metadata["artifactIntentIds"], []string{id}) {
		t.Fatalf("lost immutable receipt references: %+v", receipt)
	}
}

func TestInvalidOrMissingArtifactIntentsCannotProduceSuccessfulReceipts(t *testing.T) {
	good := strings.Repeat("a", 64)
	for name, ids := range map[string][]string{
		"absent": nil, "invalid": {"not-a-receipt"}, "uppercase": {strings.Repeat("A", 64)},
		"duplicate": {good, good}, "too many": make([]string, 1025),
	} {
		t.Run(name, func(t *testing.T) {
			driver := &runDriver{}
			r := driver.receiptFor(pipelines.StepResult{Status: pipelines.OutcomeSucceeded, Where: pipelines.Where{Surface: "cluster"}, ArtifactIntentIDs: ids}, time.Second, true)
			if r.status != WorkStepFailed || r.code != pipelines.CodeArtifactUnavailable || r.result["status"] != string(pipelines.OutcomeFailed) {
				t.Fatalf("invalid evidence produced a success: %+v", r)
			}
		})
	}
}

// Exercise compilation, placement, executor handoff and the real work journal:
// a fleet runner's presentation fields cannot bypass the receipt requirement.
func TestNativeArtifactStepCannotSucceedWithOnlyEditableLibraryIDs(t *testing.T) {
	for _, surface := range []string{"fleet", "", "cluster"} {
		for _, verified := range []bool{false, true} {
			t.Run(fmt.Sprintf("surface=%s/verified=%t", surface, verified), func(t *testing.T) {
				manifest := `formatVersion: 1
name: shop
pipeline:
  platform: linux/arm64
  stages:
    - name: build
      steps:
        - name: native
          execution: native
          placement: fleet
          run: make artifact
          artifacts: [dist/release.zip]
`
				dh := newDriveHarness(t, manifest)
				dh.p.Compute = pipelines.ComputeClusterAndFleet
				dh.store.addPipeline(dh.p)
				dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
					if req.Step.Placement != pipelines.PlacementFleet || len(req.Step.Artifacts) != 1 {
						t.Errorf("test did not reach a declared fleet artifact: %+v", req.Step)
					}
					res := passed(req)
					res.Where.Surface = surface
					if !verified {
						res.ArtifactIntentIDs = nil
					}
					return res, nil
				}
				run := dh.openRun(t, pushOpening())
				deliver(t, dh.integ, run)
				got, _ := dh.store.run(run.ID)
				want := ConclusionFailure
				if verified {
					want = ConclusionSuccess
				}
				if got.Conclusion != want || len(dh.exec.sent()) != 1 {
					t.Fatalf("native artifact receipt gate: conclusion=%s want=%s requests=%d", got.Conclusion, want, len(dh.exec.sent()))
				}
				if !verified {
					receipts := dh.work.receiptsOf("build.native")
					if len(receipts) != 1 || argString(receipts[0].Args, "errorCode") != pipelines.CodeArtifactUnavailable {
						t.Fatalf("wrong native refusal: %+v", receipts)
					}
				}
			})
		}
	}
}
