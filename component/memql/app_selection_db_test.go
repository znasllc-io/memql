package memql

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
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
	untilWithoutRecent, err := peer.appBlockedUntil(ctx, registration, "claude-code", nil, false, time.Time{})
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
	// Another machine's receipts never block this one.
	until, err := peer.appBlockedUntil(ctx, registration+"-other", "claude-code", nil, false, time.Time{})
	require.NoError(t, err)
	require.True(t, until.IsZero())
	// A new refusal blocks again; an unrelated failure cannot erase it.
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
		until, err := e.appBlockedUntil(ctx, black, "claude-code", nil, false, time.Time{})
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
	until, err := peer.appBlockedUntil(ctx, black, "claude-code", nil, false, time.Time{})
	require.NoError(t, err)
	require.True(t, until.IsZero(), "success did not clear the refusal")
}

// The machine's measurement lives on the row, so a selection on a replica that holds
// no stream for it answers the same. Absent and unknown stay selectable, or every
// Cockpit that has not upgraded would leave routing.
func TestAppSelectionDBReadsReadinessOffTheRow(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	peer, err := New(db)
	require.NoError(t, err)
	require.NoError(t, peer.Init(memorynodes.DefaultRegistry()))
	key := uniqueSuffix("app-readiness")
	owner := "v1:identity:user:" + key
	registration := WorkerRegistrationConcept + ":" + key
	write := func(apps []map[string]any) {
		raw, _ := json.Marshal(map[string]any{"ownerUserId": owner, "identityId": "test", "name": "Readiness test", "capabilities": []string{"HEADLESS"}, "concurrency": map[string]any{}, "registeredAt": time.Now().UTC().Format(time.RFC3339Nano), "apps": apps})
		row := memorynodes.MemoryNode{ID: registration, Concept: WorkerRegistrationConcept, CreatedAt: time.Now().UTC(), CreatedBy: owner, Type: memorynodes.NodeTypeObject, Payload: raw, Schema: json.RawMessage(`{}`), Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
		_, err := db.NewInsert().Model(&row).Exec(context.Background())
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", registration).Exec(context.Background())
		require.NoError(t, err)
	})
	app := func(id, readiness string) map[string]any {
		entry := map[string]any{"id": id, "version": "1", "signedIn": true, "allowed": true, "subscription": "present"}
		if readiness != "" {
			entry["readiness"] = readiness
			entry["readinessCheckedAt"] = time.Now().UTC().Format(time.RFC3339)
		}
		return entry
	}
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	machine := AppMachine{RegistrationId: registration, Online: true, Forwardable: true, StructuredResult: true}
	doors := []AppDoor{{AppId: "claude-code", Machines: []AppMachine{machine}}, {AppId: "codex", Machines: []AppMachine{machine}}}
	req := AppCallRequest{ActingUserId: owner, AppId: "*"}

	for _, measured := range []string{"out_of_usage", "signed_out", "unknown"} {
		write([]map[string]any{app("claude-code", measured), app("codex", "ready")})
		time.Sleep(2 * time.Millisecond)
		selected, err := peer.SelectAppForCall(ctx, req, false, nil, doors)
		require.NoError(t, err)
		require.Equal(t, "codex", selected.AppId, "%s app was selected", measured)
		filtered, err := peer.FilterAppDoors(ctx, doors)
		require.NoError(t, err)
		require.Empty(t, filtered[0].Machines, "%s door stayed open", measured)
		require.Len(t, filtered[1].Machines, 1)
		require.Error(t, peer.CheckMachineAppRouting(ctx, registration, "claude-code"))
		require.NoError(t, peer.CheckMachineAppRouting(ctx, registration, "codex"))
	}

	for _, silent := range []string{"", "ready"} {
		write([]map[string]any{app("claude-code", silent)})
		time.Sleep(2 * time.Millisecond)
		filtered, err := eng.FilterAppDoors(ctx, doors)
		require.NoError(t, err)
		require.Len(t, filtered[0].Machines, 1, "readiness %q left routing", silent)
		require.NoError(t, eng.CheckMachineAppRouting(ctx, registration, "claude-code"))
	}
}

// A measurement newer than a refusal is the proof the app answers again: buying a
// reset or signing in writes no session row, and this is what clears the block
// without anybody pressing anything.
func TestAppRefusalDBClearsOnANewerMeasurement(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	key := uniqueSuffix("app-measured")
	owner := "v1:identity:user:" + key
	registration := WorkerRegistrationConcept + ":" + key
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
	refused := time.Now().UTC().Add(-time.Hour)
	reset := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
	insert("v1:worker:appSession", "v1:worker:appSession:"+key+"-refused", map[string]any{"ownerUserId": owner, "workerId": registration, "app": "codex", "status": "failed", "quotaRefused": true, "endedAt": refused.Format(time.RFC3339Nano), "errorMessage": "usage limit resets at " + reset.Format(time.RFC3339), "usage": map[string]any{"known": true}})
	registrationRow := func(readiness string, checkedAt time.Time) {
		insert(WorkerRegistrationConcept, registration, map[string]any{"ownerUserId": owner, "identityId": "test", "name": "Measured test", "capabilities": []string{"HEADLESS"}, "concurrency": map[string]any{}, "registeredAt": time.Now().UTC().Format(time.RFC3339Nano), "apps": []map[string]any{{"id": "codex", "allowed": true, "signedIn": true, "subscription": "present", "readiness": readiness, "readinessCheckedAt": checkedAt.Format(time.RFC3339)}}})
		time.Sleep(2 * time.Millisecond)
	}
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	doors := []AppDoor{{AppId: "codex", Machines: []AppMachine{{RegistrationId: registration, Online: true, Forwardable: true, StructuredResult: true}}}}
	// A check from before the refusal proves nothing about after it.
	registrationRow("ready", refused.Add(-time.Minute))
	filtered, err := eng.FilterAppDoors(ctx, doors)
	require.NoError(t, err)
	require.Empty(t, filtered[0].Machines, "an older check cleared a refusal")
	// Nor does a newer check that could not tell.
	registrationRow("unknown", refused.Add(time.Minute))
	filtered, err = eng.FilterAppDoors(ctx, doors)
	require.NoError(t, err)
	require.Empty(t, filtered[0].Machines, "a check that could not tell cleared a refusal")
	registrationRow("ready", refused.Add(time.Minute))
	filtered, err = eng.FilterAppDoors(ctx, doors)
	require.NoError(t, err)
	require.Len(t, filtered[0].Machines, 1, "the machine measured the app working and it stayed blocked until the reset")
	selected, err := eng.SelectAppForCall(ctx, AppCallRequest{ActingUserId: owner, AppId: "*"}, false, nil, doors)
	require.NoError(t, err)
	require.Equal(t, "codex", selected.AppId)
}

// The owner: "we should keep a state and an interval checking for it until it
// becomes available, then it can be used again." A reset passing does not end a
// refusal on a machine that measures: its next check does, so nobody's real call is
// what finds out. A Cockpit that measures nothing has only the clock.
func TestAppRefusalDBOutlivesItsResetUntilTheMachineSaysSo(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	key := uniqueSuffix("app-outlives")
	owner := "v1:identity:user:" + key
	registration := WorkerRegistrationConcept + ":" + key
	var ids []string
	insert := func(concept, id string, payload map[string]any) {
		ids = append(ids, id)
		raw, _ := json.Marshal(payload)
		row := memorynodes.MemoryNode{ID: id, Concept: concept, CreatedAt: time.Now().UTC(), CreatedBy: owner, Type: memorynodes.NodeTypeObject, Payload: raw}
		_, err := db.NewInsert().Model(&row).Exec(context.Background())
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
			require.NoError(t, err)
		}
	})
	refused := time.Now().UTC().Add(-2 * time.Hour)
	reset := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	insert("v1:worker:appSession", "v1:worker:appSession:"+key+"-refused", map[string]any{"ownerUserId": owner, "workerId": registration, "app": "codex", "status": "failed", "quotaRefused": true, "endedAt": refused.Format(time.RFC3339Nano), "errorMessage": "usage limit resets at " + reset.Format(time.RFC3339), "usage": map[string]any{"known": true}})
	registrationRow := func(readiness string, checkedAt time.Time) {
		entry := map[string]any{"id": "codex", "allowed": true, "signedIn": true, "subscription": "present"}
		if readiness != "" {
			entry["readiness"] = readiness
			entry["readinessCheckedAt"] = checkedAt.Format(time.RFC3339)
		}
		insert(WorkerRegistrationConcept, registration, map[string]any{"ownerUserId": owner, "identityId": "test", "name": "Outlives test", "capabilities": []string{"HEADLESS"}, "concurrency": map[string]any{}, "registeredAt": time.Now().UTC().Format(time.RFC3339Nano), "apps": []map[string]any{entry}, "appRouting": map[string]any{"mode": "auto", "revision": "r1"}})
		time.Sleep(2 * time.Millisecond)
	}
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	doors := []AppDoor{{AppId: "codex", Machines: []AppMachine{{RegistrationId: registration, Online: true, Forwardable: true, StructuredResult: true}}}}

	// The machine measures, and has not yet said anything since the refusal.
	registrationRow("ready", refused.Add(-time.Minute))
	filtered, err := eng.FilterAppDoors(ctx, doors)
	require.NoError(t, err)
	require.Empty(t, filtered[0].Machines, "the reset passing sent a real call to find out")

	// Its next check is what ends it.
	registrationRow("ready", refused.Add(time.Minute))
	filtered, err = eng.FilterAppDoors(ctx, doors)
	require.NoError(t, err)
	require.Len(t, filtered[0].Machines, 1)

	// A Cockpit that predates the checks will never say, so the reset is all there is.
	registrationRow("", time.Time{})
	filtered, err = eng.FilterAppDoors(ctx, doors)
	require.NoError(t, err)
	require.Len(t, filtered[0].Machines, 1, "a machine that measures nothing stayed blocked past the reset")
}

// The machine holding a step's work comes first when anything there can take it over,
// on a replica that reads it off the rows, and another machine serves when nothing can.
func TestAppSelectionDBPrefersTheMachineHoldingTheWork(t *testing.T) {
	_, db, _ := sharedReadMergeEngine(t)
	peer, err := New(db)
	require.NoError(t, err)
	require.NoError(t, peer.Init(memorynodes.DefaultRegistry()))
	key := uniqueSuffix("app-prefer")
	owner := "v1:identity:user:" + key
	machineA, machineB := WorkerRegistrationConcept+":"+key+"-a", WorkerRegistrationConcept+":"+key+"-b"
	var ids []string
	write := func(id string, apps []map[string]any) {
		ids = append(ids, id)
		raw, _ := json.Marshal(map[string]any{"ownerUserId": owner, "identityId": "test", "name": "Prefer test", "capabilities": []string{"HEADLESS"}, "concurrency": map[string]any{}, "registeredAt": time.Now().UTC().Format(time.RFC3339Nano), "apps": apps, "appRouting": map[string]any{"mode": "auto", "revision": "r1"}})
		row := memorynodes.MemoryNode{ID: id, Concept: WorkerRegistrationConcept, CreatedAt: time.Now().UTC(), CreatedBy: owner, Type: memorynodes.NodeTypeObject, Payload: raw}
		_, err := db.NewInsert().Model(&row).Exec(context.Background())
		require.NoError(t, err)
		time.Sleep(2 * time.Millisecond)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
			require.NoError(t, err)
		}
	})
	ready := func(id string) map[string]any {
		return map[string]any{"id": id, "version": "1", "signedIn": true, "allowed": true, "subscription": "present", "readiness": "ready", "readinessCheckedAt": time.Now().UTC().Format(time.RFC3339)}
	}
	write(machineA, []map[string]any{ready("claude-code"), ready("codex")})
	write(machineB, []map[string]any{ready("claude-code"), ready("codex")})
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	a := AppMachine{RegistrationId: machineA, Online: true, LocalStream: true, StructuredResult: true}
	b := AppMachine{RegistrationId: machineB, Online: true, LocalStream: true, StructuredResult: true}
	doors := []AppDoor{{AppId: "claude-code", Machines: []AppMachine{b, a}}, {AppId: "codex", Machines: []AppMachine{b, a}}}

	// Codex ran out on the machine holding the work, and Claude Code there is free.
	selected, err := peer.SelectAppForCall(ctx, AppCallRequest{ActingUserId: owner, AppId: "*", PreferRegistrationId: machineB}, true, []string{"codex"}, doors)
	require.NoError(t, err)
	require.Equal(t, AppSelection{"claude-code", machineB}, selected)
	selected, err = peer.SelectAppForCall(ctx, AppCallRequest{ActingUserId: owner, AppId: "*", PreferRegistrationId: machineA}, true, []string{"codex"}, doors)
	require.NoError(t, err)
	require.Equal(t, AppSelection{"claude-code", machineA}, selected)

	// Nothing on that machine can: another one serves rather than failing the step.
	write(machineA, []map[string]any{{"id": "claude-code", "version": "1", "signedIn": false, "allowed": true, "subscription": "present", "readiness": "signed_out"}, ready("codex")})
	selected, err = peer.SelectAppForCall(ctx, AppCallRequest{ActingUserId: owner, AppId: "*", PreferRegistrationId: machineA}, true, []string{"codex"}, doors)
	require.NoError(t, err)
	require.Equal(t, AppSelection{"claude-code", machineB}, selected)
}

// The journal is what a takeover presents as fact. Its rows are written on the replica
// holding the session and, for MCP calls, on the MCP node, so the read must answer on
// any replica at once, and only for their owner.
func TestAppWorkJournalDBReadsWhatAnotherReplicaWrote(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	peer, err := New(db)
	require.NoError(t, err)
	require.NoError(t, peer.Init(memorynodes.DefaultRegistry()))
	key := uniqueSuffix("app-journal")
	owner := "v1:identity:user:" + key
	runId := "v1:work:run:" + key
	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
			require.NoError(t, err)
		}
	})
	record := func(writer *MemQLEngine, name, tool, actionId string, seq int) {
		id := "v1:work:observation:" + key + "-" + name
		ids = append(ids, id)
		call, err := parser.RenderCall("createWorkObservation", map[string]any{
			"observationId": key + "-" + name, "runId": runId, "stepKey": "session", "kind": "tool_result",
			"content": "The app ran " + tool + " successfully.",
			"data":    map[string]any{"tool": tool, "appActionId": actionId, "seq": seq, "isError": false, "args": `{"path":"a"}`},
		})
		require.NoError(t, err)
		_, err = writer.Execute(auth.ContextWithUserActor(auth.ContextWithInternalOrigin(context.Background()), owner), "mutation "+call)
		require.NoError(t, err)
	}
	// An empty read first, so a cache could only answer wrongly.
	journal, err := ReadAppWorkJournal(context.Background(), peer, owner, runId)
	require.NoError(t, err)
	require.Empty(t, journal)

	record(eng, "one", "exec", "toolu_1", 1)
	record(peer, "two", "mcp", "mcp-createThing-2", 2)
	record(eng, "three", "fs_write", "toolu_2", 3)
	journal, err = ReadAppWorkJournal(context.Background(), peer, owner, runId)
	require.NoError(t, err)
	require.Len(t, journal, 3)
	require.Equal(t, "exec", journal[0].Tool)
	require.Equal(t, "fs_write", journal[2].Tool)
	require.True(t, AppJournalComplete(journal, 2))
	require.False(t, AppJournalComplete(journal, 3), "the MCP node's step hid a lost action")

	stranger, err := ReadAppWorkJournal(context.Background(), peer, "v1:identity:user:"+key+"-other", runId)
	require.NoError(t, err)
	require.Empty(t, stranger, "another person read the journal")
}
