package memql

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

type imageResultFleet struct {
	stubFleet
	calls int
}

func (f *imageResultFleet) Call(_ context.Context, req FleetCallRequest) (FleetCallResult, error) {
	f.lastReq = req
	f.calls++
	return FleetCallResult{Images: []FleetImage{{Data: []byte("private-image-bytes"), MediaType: "image/png"}}}, nil
}

func TestStoredImageReplayAcrossEnginesKeepsBytesOutOfJournal(t *testing.T) {
	offTheSharedRateCeiling(t)
	model := onlineModel("illustrator", false)
	model.ImageGen, model.ContextWindow = true, 0
	fleet := &imageResultFleet{stubFleet: stubFleet{models: []FleetModel{model}}}
	first := engineWithFleetDefaultPrompt(t, fleet, "")
	journal := newCountingJournal()
	first.SetModelCallJournal(journal)
	run := common.RunContext{RunId: "image-live", GoalId: "goal", StepKey: "portrait", OwnerUserId: "alice", Mode: common.RunModeLive}
	req := airoute.ResolveRequest{Level: airoute.LevelFast, ExplicitProvider: "fleet:illustrator"}
	input := FleetImageRequest{Prompt: "An illustrated portrait", Width: 512, Height: 768, Count: 1, Format: "png"}
	writes := 0
	sink := func(_ context.Context, image FleetImage, resolution airoute.Resolution) (string, error) {
		writes++
		if string(image.Data) != "private-image-bytes" || resolution.Model != "illustrator" {
			t.Fatal("image or attribution lost")
		}
		return `{"artifactId":"portrait-1","markdownURI":"../portrait-1/portrait.png"}`, nil
	}
	live, err := first.CallAIImage(common.ContextWithRun(userCtx("alice"), run), req, input, "alice/image-1", sink)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.lastReq.Kind != FleetKindImage || fleet.lastReq.Image.Width != 512 || fleet.lastReq.ContextTokens != 0 {
		t.Fatalf("wrong fleet dispatch: %+v", fleet.lastReq)
	}
	second := engineWithFleetDefaultPrompt(t, fleet, "")
	second.SetModelCallJournal(journal)
	run.RunId, run.SourceRunId, run.SourceGoalId, run.Mode = "image-replay", "image-live", "goal", common.RunModeReplay
	replayed, err := second.CallAIImage(common.ContextWithRun(userCtx("alice"), run), req, input, "alice/image-1", sink)
	if err != nil || replayed.Receipt != live.Receipt || replayed.Served != "journal" || writes != 1 || fleet.calls != 1 {
		t.Fatalf("replay=%+v err=%v writes=%d calls=%d", replayed, err, writes, fleet.calls)
	}
	raw, _ := json.Marshal(journal.rows)
	if strings.Contains(string(raw), "private-image-bytes") {
		t.Fatal("image bytes entered the model journal")
	}
	if _, err = second.CallAIImage(common.ContextWithRun(userCtx("alice"), run), req, input, "alice/different-destination", sink); err == nil {
		t.Fatal("replay reused a different destination")
	}
	input.Width = 768
	if _, err = second.CallAIImage(common.ContextWithRun(userCtx("alice"), run), req, input, "alice/image-1", sink); err == nil {
		t.Fatal("replay reused different image dimensions")
	}
}

func TestStoredImageFailureIsNotASuccessReceipt(t *testing.T) {
	offTheSharedRateCeiling(t)
	model := onlineModel("illustrator", false)
	model.ImageGen = true
	fleet := &imageResultFleet{stubFleet: stubFleet{models: []FleetModel{model}}}
	e := engineWithFleetDefaultPrompt(t, fleet, "")
	journal := newCountingJournal()
	e.SetModelCallJournal(journal)
	ctx := common.ContextWithRun(userCtx("alice"), common.RunContext{RunId: "failed-image", GoalId: "goal", StepKey: "image", OwnerUserId: "alice", Mode: common.RunModeLive})
	_, err := e.CallAIImage(ctx, airoute.ResolveRequest{Level: airoute.LevelFast, ExplicitProvider: "fleet:illustrator"}, FleetImageRequest{Prompt: "A portrait", Width: 512, Height: 512, Count: 1}, "alice/image", func(context.Context, FleetImage, airoute.Resolution) (string, error) {
		return "", errors.New("storage unavailable")
	})
	if err == nil || !strings.Contains(err.Error(), "storage unavailable") {
		t.Fatalf("storage failure hidden: %v", err)
	}
	if rows := journal.rows["failed-image"]; len(rows) != 1 || rows[0].Error == "" {
		t.Fatalf("failure not recorded: %+v", rows)
	}
}

func TestStoredImageHonorsOwnerStepInstructions(t *testing.T) {
	offTheSharedRateCeiling(t)
	model := onlineModel("illustrator", false)
	model.ImageGen = true
	fleet := &imageResultFleet{stubFleet: stubFleet{models: []FleetModel{model}}}
	e := engineWithFleetDefaultPrompt(t, fleet, "")
	ctx := common.ContextWithRun(userCtx("alice"), common.RunContext{RunId: "image-override", OwnerUserId: "alice", StepKey: "portrait", Override: &common.StepOverride{Prompt: "Use a charcoal drawing style"}})
	_, err := e.CallAIImage(ctx, airoute.ResolveRequest{Level: airoute.LevelFast, ExplicitProvider: "fleet:illustrator"}, FleetImageRequest{Prompt: "An illustrated portrait", Width: 512, Height: 768, Count: 1, Format: "png"}, "image-override", func(context.Context, FleetImage, airoute.Resolution) (string, error) { return "stored-image", nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fleet.lastReq.Image.Prompt, "Use a charcoal drawing style") {
		t.Fatal("owner instructions did not reach image model")
	}
}
