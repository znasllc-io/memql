package library

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

type standaloneImageEngine struct {
	*memql.MemQLEngine
	calls       int
	beforeStore func()
	fail        error
	invalid     bool
}

func (e *standaloneImageEngine) CallAIImage(ctx context.Context, request airoute.ResolveRequest, input memql.FleetImageRequest, key string, save func(context.Context, memql.FleetImage, airoute.Resolution) (string, error)) (memql.StoredImageAIResult, error) {
	e.calls++
	if request.PromptName != "libraryImageGeneration" || request.Level != airoute.LevelStrong || input.Count != 1 || input.Format != "png" || input.Width != 512 || input.Height != 512 || key == "" {
		return memql.StoredImageAIResult{}, errors.New("wrong image contract")
	}
	if e.fail != nil {
		return memql.StoredImageAIResult{}, e.fail
	}
	if e.beforeStore != nil {
		e.beforeStore()
	}
	data := attachmentPNG()
	if e.invalid {
		data = []byte("not an image")
	}
	receipt, err := save(ctx, memql.FleetImage{Data: data, MediaType: "image/png"}, airoute.Resolution{ProviderName: "fleet:fixture", Model: "image-model"})
	return memql.StoredImageAIResult{Receipt: receipt}, err
}

func TestGenerateImageOwnedRunRecoveryAcrossReplicas(t *testing.T) {
	f := newRevisionDB(t)
	goal, run := id.NewShortId(), id.NewShortId()
	internal := auth.ContextWithInternalOrigin(f.ctx)
	f.query(f.engine, internal, "mutation", "createWorkGoal", map[string]any{"goalId": goal, "statement": "Generate the requested bird image", "origin": "user"})
	f.query(f.engine, internal, "mutation", "createWorkRun", map[string]any{"runId": run, "goalId": goal, "automationName": "imageFixture", "templateFingerprint": "imageFixture", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	// Native journals advance accepted goals to active before executing work.
	f.query(f.engine, internal, "mutation", "updateWorkGoal", map[string]any{"goalId": goal, "status": "active"})
	rc := common.RunContext{RunId: run, GoalId: goal, OwnerUserId: f.owner, StepKey: "generate", Mode: common.RunModeLive}
	ctx := common.ContextWithRun(internal, rc)
	first, second := &standaloneImageEngine{MemQLEngine: f.engine}, &standaloneImageEngine{MemQLEngine: f.other}
	f.first.engine, f.second.engine = first, second
	storage := &preparedImageStore{f: f, loseReply: true}
	f.first.SetImageAssets(storage)
	f.second.SetImageAssets(storage)
	args := map[string]any{"name": "bird.png", "alt": "Illustration of a bird", "prompt": "A bird illustration", "width": 512, "height": 512, "level": "strong"}
	_, err := f.first.handleGenerateImage(ctx, args, 0)
	require.ErrorContains(t, err, "reply lost")
	nodes, err := f.second.handleGenerateImage(ctx, args, 0)
	require.NoError(t, err)
	require.Equal(t, 1, first.calls)
	require.Zero(t, second.calls)
	require.Equal(t, 1, storage.writes)
	var receipt map[string]any
	require.NoError(t, json.Unmarshal(nodes[0].Payload, &receipt))
	fileID := asString(receipt["outputFileId"])
	require.NotEmpty(t, fileID)
	row, err := f.first.revisionRow(ctx, "libraryFileById", map[string]any{"fileId": fileID})
	require.NoError(t, err)
	require.Equal(t, "ready", row["status"])
	require.Equal(t, "image-model", revisionMap(row["imageProvenance"])["model"])
	require.Equal(t, memql.BareShortId(run), memql.BareShortId(asString(row["producedByRunId"])))

	t.Run("closed goal refuses the saved receipt", func(t *testing.T) {
		f.query(f.engine, internal, "mutation", "updateWorkGoal", map[string]any{"goalId": goal, "status": "closed"})
		_, err := f.second.handleGenerateImage(ctx, args, 0)
		require.Error(t, err)
		require.Zero(t, second.calls)
		f.query(f.engine, internal, "mutation", "updateWorkGoal", map[string]any{"goalId": goal, "status": "open"})
		_, err = f.second.handleGenerateImage(ctx, args, 0)
		require.NoError(t, err, "accepted goals remain eligible as well as active goals")
		require.Zero(t, second.calls)
	})

	t.Run("authority before effects", func(t *testing.T) {
		wrong := rc
		wrong.OwnerUserId = "someone-else"
		replay := rc
		replay.Mode = common.RunModeReplay
		for _, rejected := range []context.Context{context.Background(), auth.ContextWithClientOrigin(ctx), common.ContextWithRun(internal, wrong), common.ContextWithRun(internal, replay)} {
			_, err := f.second.handleGenerateImage(rejected, args, 0)
			require.Error(t, err)
		}
		require.Zero(t, second.calls)
	})
	t.Run("invalid request before inference", func(t *testing.T) {
		for _, bad := range []map[string]any{{"name": "../bird.png"}, {"name": "bird.jpg"}, {"width": 2000}, {"prompt": ""}} {
			changed := map[string]any{}
			for k, v := range args {
				changed[k] = v
			}
			for k, v := range bad {
				changed[k] = v
			}
			_, err := f.second.handleGenerateImage(ctx, changed, 0)
			require.Error(t, err)
		}
		require.Zero(t, second.calls)
	})
	t.Run("provider refusal and invalid bytes never save", func(t *testing.T) {
		changed := map[string]any{}
		for k, v := range args {
			changed[k] = v
		}
		changed["prompt"] = "Another image"
		second.fail = memql.ErrFleetUnavailable
		_, err := f.second.handleGenerateImage(ctx, changed, 0)
		require.ErrorIs(t, err, memql.ErrFleetUnavailable)
		second.fail = nil
		second.invalid = true
		_, err = f.second.handleGenerateImage(ctx, changed, 0)
		require.Error(t, err)
		second.invalid = false
		require.Equal(t, 1, storage.writes)
	})
	t.Run("cancellation on another replica before storage", func(t *testing.T) {
		changed := map[string]any{}
		for k, v := range args {
			changed[k] = v
		}
		changed["prompt"] = "Cancelled image"
		second.beforeStore = func() {
			f.query(f.engine, internal, "mutation", "updateWorkRun", map[string]any{"runId": run, "cancelRequested": true})
		}
		_, err := f.second.handleGenerateImage(ctx, changed, 0)
		require.Error(t, err)
		require.Equal(t, 1, storage.writes)
		second.beforeStore = nil
		_, err = f.first.handleGenerateImage(ctx, args, 0)
		require.Error(t, err, "old receipt cannot bypass current cancellation")
	})
}
