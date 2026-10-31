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
	// Selection accepts unfiltered machine inventory; it must enforce current
	// owner settings itself, including when another replica read the inventory.
	for _, settings := range []map[string]any{
		{"ownerFeatures": map[string]any{"apps": false}},
		{"revokedAt": time.Now().UTC().Format(time.RFC3339Nano)},
	} {
		var payload map[string]any
		require.NoError(t, json.Unmarshal(raw, &payload))
		for key, value := range settings {
			payload[key] = value
		}
		row.Payload, err = json.Marshal(payload)
		require.NoError(t, err)
		row.CreatedAt = time.Now().UTC()
		_, err = db.NewInsert().Model(&row).Exec(context.Background())
		require.NoError(t, err)
		_, err = peer.SelectAppForCall(ctx, req, false, nil, doors)
		require.ErrorIs(t, err, ErrAppUnavailable)
	}
}

func TestAppQuotaDBSurvivesReplicasAndExplicitRecovery(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	peer, err := New(db)
	require.NoError(t, err)
	require.NoError(t, peer.Init(memorynodes.DefaultRegistry()))
	key := uniqueSuffix("app-quota")
	owner := "v1:identity:user:" + key
	registration := WorkerRegistrationConcept + ":" + key
	var ids []string
	insert := func(concept, short string, payload map[string]any) {
		full := concept + ":" + short
		ids = append(ids, full)
		raw, _ := json.Marshal(payload)
		row := memorynodes.MemoryNode{ID: full, Concept: concept, CreatedAt: time.Now().UTC(), CreatedBy: owner, Type: memorynodes.NodeTypeObject, Payload: raw, Schema: json.RawMessage(`{}`), Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
		_, err := db.NewInsert().Model(&row).Exec(context.Background())
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		for _, key := range ids {
			_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", key).Exec(context.Background())
			require.NoError(t, err)
		}
	})
	insert(WorkerRegistrationConcept, key, map[string]any{"ownerUserId": owner, "identityId": "test", "name": "Quota test", "capabilities": []string{"HEADLESS"}, "concurrency": map[string]any{}, "registeredAt": time.Now().UTC().Format(time.RFC3339Nano)})
	at := time.Now().UTC().Add(-72 * time.Hour)
	reset := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	quota := map[string]any{"ownerUserId": owner, "workerId": registration, "app": "claude-code", "status": "failed", "quotaRefused": true, "endedAt": at.Format(time.RFC3339Nano), "errorMessage": "usage limit resets at " + reset.Format(time.RFC3339), "usage": map[string]any{"known": true, "inputTokens": 100}}
	insert("v1:worker:appSession", key+"-quota", quota)
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	machine := AppMachine{RegistrationId: registration, Online: true, LocalStream: true, StructuredResult: true}
	doors := []AppDoor{{AppId: "claude-code", Machines: []AppMachine{machine}}, {AppId: "codex", Machines: []AppMachine{machine}}}
	req := AppCallRequest{ActingUserId: owner, AppId: "*"}
	untilWithoutRecent, err := peer.appBlockedUntil(ctx, registration, "claude-code", nil, nil)
	require.NoError(t, err)
	require.Equal(t, reset, untilWithoutRecent, "quota was lost without rolling usage history")
	// Receipt is older than the rolling two-hour window, with non-zero work.
	// Availability must still suppress new calls; replay remains separately gated.
	for _, e := range []*MemQLEngine{eng, peer} {
		selected, err := e.SelectAppForCall(ctx, req, false, nil, doors)
		require.NoError(t, err)
		require.Equal(t, "codex", selected.AppId)
		filtered, err := e.FilterAppDoors(ctx, doors)
		require.NoError(t, err)
		require.Empty(t, filtered[0].Machines)
	}
	// Another user's receipts and another machine's receipts never block this one.
	_, err = peer.evaluateFleetRetryAppAvailability(auth.ContextWithUserActor(context.Background(), "stranger"), map[string]any{"registrationId": registration, "app": "claude-code"})
	require.Error(t, err)
	until, err := peer.appBlockedUntil(ctx, registration+"-other", "claude-code", nil, nil)
	require.NoError(t, err)
	require.True(t, until.IsZero())
	// A deliberate credit refresh is durable across replicas and scoped to one app.
	_, err = eng.evaluateFleetRetryAppAvailability(ctx, map[string]any{"registrationId": registration, "app": "claude-code"})
	require.NoError(t, err)
	selected, err := peer.SelectAppForCall(ctx, req, false, []string{"codex"}, doors)
	require.NoError(t, err)
	require.Equal(t, "claude-code", selected.AppId)
	_, err = eng.evaluateFleetSetAppRouting(ctx, map[string]any{"registrationId": registration, "revision": "", "mode": "auto", "usageWeight": 50, "speedWeight": 50, "reliabilityWeight": 50})
	require.NoError(t, err)
	_, err = peer.SelectAppForCall(ctx, req, false, []string{"codex"}, doors)
	require.NoError(t, err, "saving strategy erased credit recovery")
	// A new refusal after refresh blocks again; an unrelated failure cannot erase it.
	quota["endedAt"] = time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)
	insert("v1:worker:appSession", key+"-new-quota", quota)
	insert("v1:worker:appSession", key+"-unrelated", map[string]any{"ownerUserId": owner, "workerId": registration, "app": "claude-code", "status": "failed", "errorMessage": "connection lost", "endedAt": time.Now().UTC().Add(2 * time.Second).Format(time.RFC3339Nano)})
	_, err = peer.SelectAppForCall(ctx, req, false, []string{"codex"}, doors)
	require.Error(t, err)
	insert("v1:worker:appSession", key+"-recovered", map[string]any{"ownerUserId": owner, "workerId": registration, "app": "claude-code", "status": "ended", "endedAt": time.Now().UTC().Add(3 * time.Second).Format(time.RFC3339Nano)})
	_, err = peer.SelectAppForCall(ctx, req, false, []string{"codex"}, doors)
	require.NoError(t, err, "success did not clear refusal")
}

// A login is one machine's, so a signed-out receipt blocks that machine's app
// and nothing else, on a replica that never held the machine's stream.
func TestAppAuthRefusalDBBlocksOneMachineAcrossReplicas(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	peer, err := New(db)
	require.NoError(t, err)
	require.NoError(t, peer.Init(memorynodes.DefaultRegistry()))
	key := uniqueSuffix("app-auth")
	owner := "v1:identity:user:" + key
	black := WorkerRegistrationConcept + ":" + key + "-black"
	gray := WorkerRegistrationConcept + ":" + key + "-gray"
	var ids []string
	insert := func(concept, id string, payload map[string]any) {
		ids = append(ids, id)
		raw, _ := json.Marshal(payload)
		row := memorynodes.MemoryNode{ID: id, Concept: concept, CreatedAt: time.Now().UTC(), CreatedBy: owner, Type: memorynodes.NodeTypeObject, Payload: raw, Schema: json.RawMessage(`{}`), Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
		_, err := db.NewInsert().Model(&row).Exec(context.Background())
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
			require.NoError(t, err)
		}
	})
	for _, registration := range []string{black, gray} {
		insert(WorkerRegistrationConcept, registration, map[string]any{"ownerUserId": owner, "identityId": "test", "name": "Auth test", "capabilities": []string{"HEADLESS"}, "concurrency": map[string]any{}, "registeredAt": time.Now().UTC().Format(time.RFC3339Nano)})
	}
	// Old enough to be outside appRoutingEvidence's page if that page were all
	// that kept it, and inside the recheck window: the receipt alone must block.
	refused := time.Now().UTC().Add(-time.Minute)
	insert("v1:worker:appSession", "v1:worker:appSession:"+key+"-refused", map[string]any{"ownerUserId": owner, "workerId": black, "app": "claude-code", "status": "failed", "authRefused": true, "endedAt": refused.Format(time.RFC3339Nano), "errorMessage": "Failed to authenticate: OAuth session expired and could not be refreshed", "usage": map[string]any{"known": true}})
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	doors := []AppDoor{{AppId: "claude-code", Machines: []AppMachine{
		{RegistrationId: black, Online: true, Forwardable: true, StructuredResult: true},
		{RegistrationId: gray, Online: true, Forwardable: true, StructuredResult: true},
	}}}
	req := AppCallRequest{ActingUserId: owner, AppId: "*"}
	for _, e := range []*MemQLEngine{eng, peer} {
		until, err := e.appBlockedUntil(ctx, black, "claude-code", nil, nil)
		require.NoError(t, err)
		require.Equal(t, refused.Add(15*time.Minute).Truncate(time.Millisecond), until.Truncate(time.Millisecond))
		selected, err := e.SelectAppForCall(ctx, req, false, nil, doors)
		require.NoError(t, err)
		require.Equal(t, gray, selected.RegistrationId, "the machine signed out of the app was selected")
		filtered, err := e.FilterAppDoors(ctx, doors)
		require.NoError(t, err)
		require.Len(t, filtered[0].Machines, 1)
		require.Equal(t, gray, filtered[0].Machines[0].RegistrationId)
	}
	// One call that moved past Gray too has nothing left for this app.
	_, err = peer.SelectAppForCall(ctx, req, false, []string{"claude-code@" + gray}, doors)
	require.ErrorIs(t, err, ErrAppUnavailable)
	// A later success on that machine is the proof the login works again.
	insert("v1:worker:appSession", "v1:worker:appSession:"+key+"-recovered", map[string]any{"ownerUserId": owner, "workerId": black, "app": "claude-code", "status": "ended", "endedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	until, err := peer.appBlockedUntil(ctx, black, "claude-code", nil, nil)
	require.NoError(t, err)
	require.True(t, until.IsZero(), "success did not clear the refusal")
}
