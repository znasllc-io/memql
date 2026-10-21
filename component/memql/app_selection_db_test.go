package memql

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"sync"
	"testing"
	"time"
)

func TestAppSelectionDBOwnerSettingsAndReplicaRotation(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	peer, err := New(db)
	require.NoError(t, err)
	require.NoError(t, peer.Init(memorynodes.DefaultRegistry()))
	key := uniqueSuffix("app-selection")
	owner := "v1:identity:user:" + key
	registration := WorkerRegistrationConcept + ":" + key
	raw, _ := json.Marshal(map[string]any{"ownerUserId": owner, "identityId": "test", "name": "Test machine", "capabilities": []string{"HEADLESS"}, "concurrency": map[string]any{}, "registeredAt": time.Now().UTC().Format(time.RFC3339Nano), "appRouting": map[string]any{"mode": "roundRobin"}})
	row := memorynodes.MemoryNode{ID: registration, Concept: WorkerRegistrationConcept, CreatedAt: time.Now().UTC(), CreatedBy: owner, Type: memorynodes.NodeTypeObject, Payload: raw, Schema: json.RawMessage(`{}`), Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
	_, err = db.NewInsert().Model(&row).Exec(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", registration).Exec(context.Background())
		require.NoError(t, err)
	})
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	machine := AppMachine{RegistrationId: registration, Online: true, LocalStream: true, StructuredResult: true}
	doors := []AppDoor{{AppId: "claude-code", Machines: []AppMachine{machine}}, {AppId: "codex", Machines: []AppMachine{machine}}}
	req := AppCallRequest{ActingUserId: owner, AppId: "*"}
	// Concurrent replicas must reserve distinct successive choices.
	var wg sync.WaitGroup
	results := make(chan string, 2)
	failures := make(chan error, 2)
	for _, e := range []*MemQLEngine{eng, peer} {
		wg.Add(1)
		go func(e *MemQLEngine) {
			defer wg.Done()
			selection, err := e.SelectAppForCall(ctx, req, false, nil, doors)
			if err != nil {
				failures <- err
			} else {
				results <- selection.AppId
			}
		}(e)
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	apps := map[string]bool{}
	for app := range results {
		apps[app] = true
	}
	require.Len(t, apps, 2)
	args := map[string]any{"registrationId": registration, "revision": "", "mode": "codex", "usageWeight": 50, "speedWeight": 50, "reliabilityWeight": 50}
	_, err = eng.evaluateFleetSetAppRouting(auth.ContextWithUserActor(context.Background(), "stranger"), args)
	require.Error(t, err)
	_, err = eng.evaluateFleetSetAppRouting(ctx, args)
	require.NoError(t, err)
	_, err = peer.evaluateFleetSetAppRouting(ctx, args)
	require.Error(t, err, "stale settings replaced another replica's edit")
	selected, err := peer.SelectAppForCall(ctx, req, false, nil, doors)
	require.NoError(t, err)
	require.Equal(t, "codex", selected.AppId)
	_, err = peer.SelectAppForCall(ctx, req, false, []string{"codex"}, doors)
	require.Error(t, err, "Only strategy fell through")
	_, err = eng.Execute(ctx, `insert("v1:worker:registration", id="`+registration+`", payload={"appRouting":{"mode":"auto"}})`)
	require.Error(t, err, "raw write bypassed settings validation")
}
