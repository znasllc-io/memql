package memql

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestModelSetupDBAdmissionAndCrossReplicaRevision(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	peer, err := New(db)
	require.NoError(t, err)
	require.NoError(t, peer.Init(memorynodes.DefaultRegistry()))
	key := uniqueSuffix("model-setup")
	owner := "v1:identity:user:" + key
	registration := WorkerRegistrationConcept + ":" + key
	model := key + "-chat"
	insert := func(conceptID, rowID string, payload map[string]any) {
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		row := memorynodes.MemoryNode{ID: rowID, Concept: conceptID, CreatedAt: time.Now().UTC(), CreatedBy: owner, Type: memorynodes.NodeTypeObject, Payload: raw, Schema: json.RawMessage(`{}`), Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
		_, err = db.NewInsert().Model(&row).Exec(context.Background())
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", conceptID).Where("id = ?", rowID).Exec(context.Background())
			require.NoError(t, err)
		})
	}
	insert(WorkerRegistrationConcept, registration, map[string]any{"ownerUserId": owner, "identityId": "test", "name": "Test machine", "registeredAt": time.Now().UTC().Format(time.RFC3339Nano), "capabilities": []string{"MODEL"}, "concurrency": map[string]any{}, "labels": map[string]any{"model:" + model: "structured=1,tools=1"}, "hardware": map[string]any{"memoryBytes": 64 << 30, "gpu": map[string]any{"backend": "metal"}, "runtimes": []any{map[string]any{"name": "ollama"}}}, "platformInfo": map[string]any{"os": "darwin"}})
	insert(ModelProfileConcept, ModelProfileConcept+":"+model, map[string]any{"modelId": model, "category": "text", "runtime": "ollama", "memoryNeedBytes": 8 << 30, "flags": []string{"structured", "tools"}, "minMachineClass": "16", "active": true})
	actor := auth.ContextWithUserActor(context.Background(), owner)
	_, err = eng.evaluateFleetSetModelSetup(auth.ContextWithUserActor(context.Background(), "someone-else"), map[string]any{"registrationId": registration, "revision": "", "choices": map[string]any{"tools": model}})
	require.ErrorContains(t, err, "no machine of yours")
	_, err = eng.evaluateFleetSetModelSetup(actor, map[string]any{"registrationId": registration, "revision": "", "choices": map[string]any{"vision": model}})
	require.ErrorContains(t, err, "support for vision")
	// Two independent engines share no local setup state or lock. The database
	// fence/revision admits one update, even when two BFF replicas race.
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, engine := range []*MemQLEngine{eng, peer} {
		wg.Add(1)
		go func(e *MemQLEngine) {
			defer wg.Done()
			<-start
			_, err := e.evaluateFleetSetModelSetup(actor, map[string]any{"registrationId": registration, "revision": "", "choices": map[string]any{"tools": model, "structured": model}})
			errs <- err
		}(engine)
	}
	close(start)
	wg.Wait()
	close(errs)
	success, refused := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			refused++
			t.Logf("refused: %v", err)
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, refused)
	row, err := peer.modelSetupRow(actor, registration)
	require.NoError(t, err)
	setup := ModelSetupFromRow(row["modelSetup"])
	require.NotEmpty(t, setup.Revision)
	require.Equal(t, model, setup.Choices["tools"])
	_, err = eng.evaluateFleetSetModelSetup(actor, map[string]any{"registrationId": registration, "revision": "", "choices": map[string]any{}})
	require.Error(t, err)
	// A raw write cannot bypass the memory and capability checks.
	err = validateWorkerConsentServerOnly(actor, row, map[string]any{"ownerUserId": owner, "modelSetup": map[string]any{"revision": "forged", "choices": map[string]any{"tools": "unknown"}}})
	require.ErrorContains(t, err, "modelSetup")
}
