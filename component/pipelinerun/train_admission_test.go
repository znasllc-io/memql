package pipelinerun

import (
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
)

func TestTrainAdmissionIdentityComesFromSavedRunAndBindsRecovery(t *testing.T) {
	dr := &runDriver{
		d:   Deps{EngineRevision: func() string { return "immutable-engine" }, Domain: func() string { return "example.test" }},
		p:   testPipeline(DeliveryWebhook),
		run: Run{SHA: shaA, Event: pipelines.EventPullRequest, HeadBranch: "train/2026-10-28-1", StartedAt: testNow},
	}
	dr.workRun.Store(&workjournal.Run{})
	dr.setFacts()
	track := &stepTrack{step: pipelines.Step{Key: "checks.unit", Kind: pipelines.StepCommand, Verification: true}}
	encoded, err := json.Marshal(dr.request(track, nil))
	if err != nil {
		t.Fatal(err)
	}
	var request pipelines.StepRequest
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatal(err)
	}
	if !IsTrainRun(dr.run) || !pipelines.IsTrainEvent(request.Event, request.HeadBranch) || request.HeadBranch != dr.run.HeadBranch {
		t.Fatal("the receiving executor lost the saved run's train identity")
	}
	if request.Environment()["MEMQL_EVENT"] != "" {
		t.Fatal("verification commands must not gain event-dependent behavior")
	}
	definition, verification := dr.definitionOf(track.step), dr.verificationOf(track.step)
	dr.run.HeadBranch = "feature/ordinary"
	if definition == "" || definition == dr.definitionOf(track.step) {
		t.Fatal("changed admission identity retained an execution recovery definition")
	}
	if verification == "" || verification != dr.verificationOf(track.step) {
		t.Fatal("admission metadata changed reusable verification of the same command and source")
	}
}
