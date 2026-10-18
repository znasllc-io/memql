package memql

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

func TestFleetCapabilityDiscoveryDoesNotBorrowOfflineModality(t *testing.T) {
	models := []FleetModel{{ModelId: "same-name", ImageGen: true, Vision: true, Machines: []FleetMachine{
		{RegistrationId: "asleep", Capabilities: FleetModelCapabilities{ImageGen: true}},
		{RegistrationId: "awake", Online: true, Capabilities: FleetModelCapabilities{Vision: true}},
		{RegistrationId: "busy", Online: true, ActiveCount: 1, MaxConcurrent: 1, Capabilities: FleetModelCapabilities{ImageGen: true}},
	}}}
	counts := fleetCapabilityCounts(models)
	require.Equal(t, map[string]any{"advertised": 2, "online": 1, "idle": 0}, counts["imageGen"])
	require.Equal(t, map[string]any{"advertised": 1, "online": 1, "idle": 1}, counts["vision"])
	models[0].Machines[0].Online = true
	require.Equal(t, map[string]any{"advertised": 2, "online": 2, "idle": 1}, fleetCapabilityCounts(models)["imageGen"])
}

type capabilityCatalog struct {
	owners []string
	err    error
}

func (c *capabilityCatalog) Catalog(_ context.Context, owner string) ([]FleetModel, error) {
	c.owners = append(c.owners, owner)
	return nil, c.err
}
func TestFleetCapabilityDiscoveryIsCallerScopedAndUnknownOnReadFailure(t *testing.T) {
	catalog := &capabilityCatalog{}
	registry := newProviderRegistry()
	registry.SetFleetCatalog(catalog)
	e := &MemQLEngine{providers: registry}
	require.Equal(t, "unavailable", e.workFleetCapabilities(context.Background())["status"])
	require.Empty(t, catalog.owners, "must never fall back to shared anonymous discovery")
	require.Equal(t, "observed", e.workFleetCapabilities(userCtx("alice"))["status"])
	require.Equal(t, []string{"alice"}, catalog.owners)
	catalog.err = errors.New("catalog unavailable")
	facts := e.workFleetCapabilities(userCtx("alice"))
	require.Equal(t, "unavailable", facts["status"])
	require.NotContains(t, facts, "modalities", "read failure is not zero capabilities")
}

func TestCapabilityPrefetchEvidenceSurvivesAnotherReplicaWithoutReplacingContinuation(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := contextTestRun(t, a)
	run, _ := common.RunFromContext(ctx)
	original := []common.ChatMessage{{Role: "user", Content: "Keep my original task and progress"}}
	require.NoError(t, a.SaveWorkContinuation(ctx, original))
	rows, err := a.workCapabilitiesBuiltin(ctx, map[string]any{"search": "library.libraryGenerateImage"}, 0)
	require.NoError(t, err)
	require.Contains(t, string(rows[0].Payload), "libraryGenerateImage", "the shipped image operation must be discoverable")
	checkpoints, err := b.workRows(ContextWithFreshRead(ctx), "workSourceCheckpointsForOwnerRun", run.RunId)
	require.NoError(t, err)
	budget := replyReviewMaxBytes
	sources, omitted := workCheckpointSources(checkpoints, run.RunId, &budget)
	require.False(t, omitted)
	require.Len(t, sources, 1)
	encoded, err := json.Marshal(sources)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "libraryGenerateImage")
	require.Contains(t, string(encoded), "checkedAt")
	require.Contains(t, string(encoded), "unavailable", "catalog failure must remain unknown to the reviewing replica")
	require.NotContains(t, string(encoded), "Keep my original task")
	restored, err := b.RestoreWorkContinuation(ctx, original)
	require.NoError(t, err)
	require.Equal(t, original, restored, "prefetch must never become resumable assistant history")
	model := &replyReviewModel{}
	b.SetAIResolver(testAIResolver{fn: func(context.Context, airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: model, Resolution: airoute.Resolution{ProviderName: "fleet:review", Model: "test"}}, nil
	}})
	_, err = b.workReviewReplyBuiltin(ctx, map[string]any{"draft": map[string]any{"reply": "An image generator is online."}}, 0)
	require.NoError(t, err)
	require.Equal(t, 1, model.calls, "automatic capability discovery must pass through final evidence review")
	raw, _ := json.Marshal(model.messages)
	require.Contains(t, string(raw), "libraryGenerateImage")
	require.Contains(t, string(raw), "checkedAt")
	foreign := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "someone-else"), run)
	require.Error(t, a.recordWorkCapabilityEvidence(foreign, "image", nil, nil, 0))
}
