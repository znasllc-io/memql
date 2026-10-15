package memql

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// One physical model can fill three recommendation levels. The real act must
// persist one dispatch per model, while the recommendation retains each level.
func TestFleetPullRecommendedPersistsEachModelOnce(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	prefix := uniqueSuffix("recommended-dedup")
	owner := "v1:identity:user:" + prefix
	registration := "v1:worker:registration:" + prefix
	chatModel, embedModel := prefix+"-chat", prefix+"-embed"
	insert := func(concept, id string, payload map[string]any) {
		t.Helper()
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		row := memorynodes.MemoryNode{
			ID: id, CreatedAt: time.Now().UTC(), CreatedBy: owner, Concept: concept,
			Type: memorynodes.NodeTypeObject, Schema: json.RawMessage(`{}`), Payload: raw,
			Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`),
		}
		_, err = db.NewInsert().Model(&row).Exec(ctx)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", concept).Where("id = ?", id).Exec(ctx)
			require.NoError(t, err)
		})
	}
	t.Cleanup(func() {
		_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", ModelPullConcept).
			Where("payload->>'workerId' IN (?, ?)", registration, prefix).Exec(ctx)
		require.NoError(t, err)
	})
	insert("v1:worker:registration", registration, map[string]any{
		"ownerUserId": owner, "name": "Dedup machine", "connectedNodeId": "another-agent",
		"lastSeenAt": time.Now().UTC().Format(time.RFC3339), "platformInfo": map[string]any{"os": "linux"},
		"hardware": map[string]any{"diskFreeBytes": 100 << 30, "reportedAt": time.Now().UTC().Format(time.RFC3339Nano), "memoryBytes": 32 << 30, "gpu": map[string]any{"backend": "cuda", "vramBytes": 24 << 30}, "runtimes": []any{map[string]any{"name": "ollama"}}},
	})
	for _, profile := range []struct {
		model, category string
		levels          []string
	}{
		{chatModel, "text", []string{"fast", "strong", "reasoning"}},
		{embedModel, "embeddings", []string{"embeddings"}},
	} {
		insert(ModelProfileConcept, ModelProfileConcept+":"+profile.model, map[string]any{
			"modelId": profile.model, "category": profile.category, "runtime": "ollama", "active": true,
			"minMachineClass": "16", "recommendedFor": profile.levels, "offeredOn": []string{"linux"},
			"sizeBytes": 1 << 30, "memoryNeedBytes": 2 << 30, "params": 900_000_000_000, "contextWindow": 8192,
		})
	}
	actor := auth.ContextWithUserActor(ctx, owner)
	hw, os, err := eng.machineHardwareFor(actor, registration)
	require.NoError(t, err)
	catalog, err := eng.catalogProfiles(actor)
	require.NoError(t, err)
	set := RecommendedSet(MachineClass(hw), hw, os, catalog)
	require.Len(t, set, 4)
	for i, level := range []string{"fast", "strong", "reasoning", "embeddings"} {
		require.Equal(t, level, set[i].Level)
	}
	// A sibling's newer disk report must replace the cached 100 GiB reading.
	// Each artifact fits alone, but the combined set cannot consume the reserve.
	setDisk := func(bytes int64) {
		_, err := db.NewUpdate().Model((*memorynodes.MemoryNode)(nil)).
			Set("payload = jsonb_set(payload, '{hardware,diskFreeBytes}', to_jsonb(?::bigint))", bytes).
			Where("concept = ?", WorkerRegistrationConcept).Where("id = ?", registration).Exec(ctx)
		require.NoError(t, err)
	}
	setDisk(13 << 29) // 6.5 GiB: two 1 GiB artifacts plus a 5 GiB reserve do not fit.
	_, err = eng.evaluateFleetPullRecommendedExpression(actor, map[string]any{"registrationId": registration})
	require.ErrorContains(t, err, "exceed free storage")
	count, err := db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", ModelPullConcept).
		Where("payload->>'workerId' IN (?, ?)", registration, prefix).Count(ctx)
	require.NoError(t, err)
	require.Zero(t, count, "a capacity refusal must not dispatch any downloads")
	setDisk(100 << 30)
	result, err := eng.evaluateFleetPullRecommendedExpression(actor, map[string]any{"registrationId": registration})
	require.NoError(t, err)
	require.Len(t, result, 1)
	var payload struct {
		Pulls []struct {
			Model string `json:"model"`
		} `json:"pulls"`
	}
	require.NoError(t, json.Unmarshal(result[0].Payload, &payload))
	require.Len(t, payload.Pulls, 2, "three chat recommendation levels must open only one chat pull")
	require.Equal(t, chatModel, payload.Pulls[0].Model)
	require.Equal(t, embedModel, payload.Pulls[1].Model)
	var stored []memorynodes.MemoryNode
	err = db.NewSelect().Model(&stored).Where("concept = ?", ModelPullConcept).
		Where("payload->>'workerId' IN (?, ?)", registration, prefix).Scan(ctx)
	require.NoError(t, err)
	require.Len(t, stored, 2, "the claiming replica must see only two persisted dispatches")
	_, err = eng.evaluateFleetPullRecommendedExpression(actor, map[string]any{"registrationId": registration})
	require.ErrorContains(t, err, "current download", "another request cannot reserve the same disk while downloads are pending")
}
