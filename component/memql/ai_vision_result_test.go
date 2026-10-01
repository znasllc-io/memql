package memql

import (
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"testing"
)

func TestVisionReferenceReplayUsesImageBytes(t *testing.T) {
	model := onlineModel("visual:8b", true)
	model.Vision = true
	fleet := &stubFleet{models: []FleetModel{model}, answer: "Blue heading, two columns"}
	e := engineWithFleetDefaultPrompt(t, fleet, "")
	journal := newCountingJournal()
	e.SetModelCallJournal(journal)
	run := common.RunContext{RunId: "reference-run", GoalId: "goal", StepKey: "compose", OwnerUserId: "alice", Mode: common.RunModeLive}
	req := airoute.ResolveRequest{Level: airoute.LevelStrong, ExplicitProvider: "fleet:visual:8b"}
	images := []common.VisionContent{{MimeType: "image/png", Data: []byte("image-one")}}
	live, err := e.CallAIVision(common.ContextWithRun(userCtx("alice"), run), req, "Describe the reference", images)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.lastReq.Kind != FleetKindVision || len(fleet.lastReq.Images) != 1 || string(fleet.lastReq.Images[0].Data) != "image-one" {
		t.Fatal("actual image bytes did not reach fleet vision")
	}
	fleet.answer = "must not be called during replay"
	run.RunId, run.SourceRunId, run.SourceGoalId, run.Mode = "reference-replay", "reference-run", "goal", common.RunModeReplay
	replay, err := e.CallAIVision(common.ContextWithRun(userCtx("alice"), run), req, "Describe the reference", images)
	if err != nil || replay.Text != live.Text || replay.Served != "journal" {
		t.Fatalf("vision replay=%+v, %v", replay, err)
	}
	images[0].Data = []byte("image-two")
	if _, err := e.CallAIVision(common.ContextWithRun(userCtx("alice"), run), req, "Describe the reference", images); err == nil {
		t.Fatal("a different image reused the old analysis")
	}
}
