package library

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

type imageWorkRecorder interface {
	RecordWorkProgress(context.Context, memql.WorkEvent) error
}

// Bind the effect to the current owned live run on this replica. Neither tool
// arguments nor an old receipt may nominate a different owner, goal or run.
func (i *Integration) imageRun(ctx context.Context) (common.RunContext, error) {
	rc, ok := common.RunFromContext(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	if auth.OriginFromContext(ctx) != auth.OriginInternal || !ok || !rc.IsRun() || rc.GoalId == "" || rc.StepKey == "" || rc.Mode == common.RunModeReplay || ac == nil || ac.UserId == "" || memql.BareShortId(ac.UserId) != memql.BareShortId(rc.OwnerUserId) {
		return rc, fmt.Errorf("image generation requires the current owned live work run")
	}
	if err := ctx.Err(); err != nil {
		return rc, err
	}
	if err := i.revisionWriter(ctx); err != nil {
		return rc, err
	}
	run, err := i.revisionRow(memql.ContextWithFreshRead(ctx), "workRunForOwner", map[string]any{"runId": rc.RunId})
	if err != nil {
		return rc, err
	}
	if memql.BareShortId(asString(run["ownerUserId"])) != memql.BareShortId(ac.UserId) || memql.BareShortId(asString(run["goalId"])) != memql.BareShortId(rc.GoalId) || run["status"] != "running" || run["cancelRequested"] == true {
		return rc, fmt.Errorf("the image work run is no longer active or owned by this person")
	}
	goal, err := i.revisionRow(memql.ContextWithFreshRead(ctx), "workGoalForOwner", map[string]any{"goalId": rc.GoalId})
	if err != nil {
		return rc, err
	}
	if memql.BareShortId(asString(goal["ownerUserId"])) != memql.BareShortId(ac.UserId) || goal["status"] != "open" {
		return rc, fmt.Errorf("the image work goal is no longer open or owned by this person")
	}
	return rc, nil
}

// One bounded generation and immutable saved-file receipt. The DSL selects
// this operation; the existing router and model journal enforce its budget,
// modality, paid-provider restrictions and recovery across replicas.
func (i *Integration) handleGenerateImage(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	rc, err := i.imageRun(ctx)
	if err != nil {
		return nil, err
	}
	spec := revisionImageSpec{Mode: "generate", Name: asString(args["name"]), Alt: asString(args["alt"]), Prompt: asString(args["prompt"])}
	if err := spec.validate(); err != nil {
		return nil, err
	}
	width, height := intField(args, "width"), intField(args, "height")
	if width < 256 || width > 1536 || height < 256 || height > 1536 {
		return nil, fmt.Errorf("image dimensions must be between 256 and 1536 pixels")
	}
	recorder, records := i.engine.(imageWorkRecorder)
	generator, ok := i.engine.(revisionImageGenerator)
	if !ok || !records || i.imageAssets == nil || i.fileVersionGate == nil {
		return nil, fmt.Errorf("image generation and verified storage are unavailable on this node")
	}
	identity, _ := json.Marshal([]any{memql.BareShortId(rc.OwnerUserId), memql.BareShortId(rc.RunId), rc.StepKey, spec, width, height, rc.Override})
	fileID := "generated-image-" + string(id.NewUntracked().FromBytes(identity))
	release, err := i.fileVersionGate(ctx, fileID)
	if err != nil {
		return nil, err
	}
	defer release()
	receipt := func() ([]memorynodes.MemoryNode, error) {
		if _, err := i.imageRun(ctx); err != nil {
			return nil, err
		}
		stored, err := i.revisionRow(ctx, "libraryFileById", map[string]any{"fileId": fileID})
		if err != nil {
			return nil, err
		}
		if stored == nil || boolField(stored, "archived") || memql.BareShortId(asString(stored["producedByRunId"])) != memql.BareShortId(rc.RunId) || asString(stored["producedByStepKey"]) != rc.StepKey || asString(stored["name"]) != spec.Name || asString(stored["mimeType"]) != "image/png" || asString(stored["blobUrl"]) == "" || asString(stored["sha256"]) == "" {
			return nil, fmt.Errorf("the saved image receipt is unavailable or changed")
		}
		// These bytes were decoded and their immutable upload was verified.
		// Complete the file synchronously; indexing may finish on another replica.
		if stored["status"] == "stored" {
			if err := i.setFileStatus(ctx, fileID, fileStatusUpdate{status: "ready", summary: spec.Alt}); err != nil {
				return nil, err
			}
			stored, err = i.revisionRow(ctx, "libraryFileById", map[string]any{"fileId": fileID})
			if err != nil {
				return nil, err
			}
		}
		if stored["status"] != "ready" {
			return nil, fmt.Errorf("the generated image is saved but not ready")
		}
		if err := recorder.RecordWorkProgress(ctx, memql.WorkEvent{ID: fileID, Kind: "artifact", Phase: "completed", Name: spec.Name, App: "files", Arguments: map[string]any{"fileId": fileID}}); err != nil {
			return nil, err
		}
		return reviewResult(map[string]any{"outputFileId": fileID, "name": spec.Name, "mimeType": "image/png", "generated": true})
	}
	stored, err := i.revisionRow(ctx, "libraryFileById", map[string]any{"fileId": fileID})
	if err != nil {
		return nil, err
	}
	if stored != nil {
		return receipt()
	}
	_, err = generator.CallAIImage(ctx, airoute.ResolveRequest{Level: airoute.Level(asString(args["level"])), PromptName: "libraryImageGeneration"}, memql.FleetImageRequest{Prompt: spec.Prompt, Width: width, Height: height, Count: 1, Format: "png"}, fileID, func(ctx context.Context, image memql.FleetImage, resolution airoute.Resolution) (string, error) {
		if _, err := i.imageRun(ctx); err != nil {
			return "", err
		}
		mime, err := reviewAttachmentMIME(spec.Name, image.Data)
		if err != nil {
			return "", err
		}
		provenance := map[string]any{"mode": "generate", "alt": spec.Alt, "prompt": spec.Prompt, "provider": resolution.ProviderName, "model": resolution.Model, "attribution": "AI-generated image; model " + resolution.Model}
		if rc.Override != nil {
			provenance["stepOverride"] = rc.Override
		}
		if err := i.imageAssets.SaveImage(ctx, ImageAssetWrite{FileID: fileID, Name: spec.Name, MIME: mime, RunID: rc.RunId, StepKey: rc.StepKey, Data: image.Data, Provenance: provenance}); err != nil {
			return "", err
		}
		return fileID, nil
	})
	if err != nil {
		return nil, err
	}
	return receipt()
}
