package pipelinerun

import (
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
