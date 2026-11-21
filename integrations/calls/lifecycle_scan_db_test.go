package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func TestLifecycleWorkflowCursorPassesHeldAndBrokenRowsBeyondEngineCap(t *testing.T) {
	cw, key, _, started := workflowFixture(t, "workflow-fair-scan", true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	run := workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: "working-session", Version: 1, IdempotencyKey: "lifecycle-workflow-start"}).Run
	require.NotNil(t, run)
	stateID := workflowStateID(cw.site, "run", started.Call.ID)
	state, err := cw.svc.store.workflowState(ctx, cw.owner, stateID)
	require.NoError(t, err)
	seed := func(id string, state workflowState) {
		data, err := json.Marshal(state)
		require.NoError(t, err)
		cw.insert(workflowConcept, id, map[string]any{"siteId": siteConcept + ":" + cw.site, "ownerUserId": cw.owner, "keyId": key.result.Session.KeyID, "kind": "run", "active": true, "stateData": string(data)})
	}
	// These valid paused runs never leave the active scan. The old first-page
	// read repeatedly revisited them and could never reach the final due run.
	held := *state
	heldRun := *state.Run
	heldRun.Status, heldRun.Hold = "paused", "human"
	held.Run = &heldRun
	for i := range 600 {
		seed(fmt.Sprintf("scan-%s-%04d", cw.suffix, i), held)
	}
	brokenID := "scan-" + cw.suffix + "-broken"
	cw.insert(workflowConcept, brokenID, map[string]any{"siteId": siteConcept + ":" + cw.site, "ownerUserId": cw.owner, "keyId": key.result.Session.KeyID, "kind": "run", "active": true, "stateData": "invalid journal"})
	roomPayload, _ := latestPayload(t, cw.db, roomConcept, roomConcept+":"+started.Call.ID)
	roomPayload["status"] = "ended"
	endedRoomID := "ended-" + cw.suffix
	cw.insert(roomConcept, endedRoomID, roomPayload)
	due := *state
	dueRun := *state.Run
	dueRun.RoomID = endedRoomID
	due.Run = &dueRun
	dueID := "zzdue-" + cw.suffix
	seed(dueID, due)

	copyService := *cw.svc // Per-request configuration copies must retain cursor progress.
	for pass := range 4 {
		err = copyService.ReconcileWorkflows(ctx)
		if err != nil {
			require.Equal(t, "workflow_state_unavailable", callrules.ReasonOf(err))
		}
		if pass < 3 {
			current, readErr := cw.svc.store.workflowState(memql.ContextWithFreshRead(ctx), cw.owner, dueID)
			require.NoError(t, readErr)
			require.Equal(t, "running", current.Run.Status)
		}
		copyService = *cw.svc
	}
	current, err := cw.svc.store.workflowState(memql.ContextWithFreshRead(ctx), cw.owner, dueID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", current.Run.Status)
	first, err := cw.svc.store.workflowState(memql.ContextWithFreshRead(ctx), cw.owner, fmt.Sprintf("scan-%s-%04d", cw.suffix, 0))
	require.NoError(t, err)
	require.Equal(t, "paused", first.Run.Status, "an unchanged first row is not a reason to restart its page")
	require.Empty(t, cw.svc.lifecycle.workflows.cursor)
	// A replica has its own scheduling hints and still safely repeats native
	// gated effects. It does not inherit or require this process's cursor.
	peer := allowanceReplica(t, cw)
	peer.commerce = cw.svc.commerce
	require.NotSame(t, cw.svc.lifecycle, peer.lifecycle)
	require.NoError(t, peer.ReconcileWorkflows(ctx))
}

func TestLifecycleResourcesCompleteAccountingAndFairExpiryBeyondEngineCap(t *testing.T) {
	cw, key, started, provider := resourceFixture(t, "resources-fair-scan")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, key, "lifecycle-resource-register")
	_, err := cw.svc.Resource(ctx, cw.ref, key.result.Session.Token, callrules.ResourceRequest{Action: "start", RoomID: started.Call.ID, ResourceID: source.ID, ExpectedRevision: source.Revision, RequestID: "lifecycle-resource-start"})
	require.NoError(t, err)
	state, err := cw.svc.store.resourceState(ctx, cw.owner, cw.site, key.result.Session.KeyID)
	require.NoError(t, err)
	broken := state.State
	broken.Leases = append([]resourceLease(nil), state.State.Leases...)
	broken.Leases[0].RoomID = "unavailable-room-" + cw.suffix
	data, err := json.Marshal(broken)
	require.NoError(t, err)
	for i := range 999 {
		keyID := fmt.Sprintf("scan-%s-%04d", cw.suffix, i)
		cw.insert(resourceConcept, resourceStateID(cw.site, keyID), map[string]any{"siteId": siteConcept + ":" + cw.site, "ownerUserId": cw.owner, "keyId": keyID, "active": true, "stateData": string(data)})
		if i == 599 {
			inventory, err := cw.svc.store.resourceStates(ctx)
			require.NoError(t, err)
			require.Len(t, inventory, 601, "accounting must cross the engine's 500-row cap")
		}
	}
	_, err = cw.svc.store.resourceStates(ctx)
	require.Equal(t, "resource_capacity", callrules.ReasonOf(err), "a 1000-row accounting bound refuses rather than undercounts")
	cw.clock.Advance(callrules.ResourceLeaseLifetime)
	copyService := *cw.svc
	for range 6 {
		err = copyService.ReconcileResources(ctx)
		if err != nil {
			require.Equal(t, "resource_room_unavailable", callrules.ReasonOf(err))
		}
		copyService = *cw.svc
	}
	require.Empty(t, provider.values, "even inventory overflow and broken rows cannot block an expired provider lease")
	require.Equal(t, 1, provider.deleted)
	state, err = cw.svc.store.resourceState(memql.ContextWithFreshRead(ctx), cw.owner, cw.site, key.result.Session.KeyID)
	require.NoError(t, err)
	require.False(t, resourceStateActive(state))
}

func TestLifecycleLegacyResourceQueuePassesUnrepairableRows(t *testing.T) {
	cw, key, _, _ := resourceFixture(t, "resource-legacy-fair")
	if cw == nil {
		return
	}
	ctx := context.Background()
	keys := make([]string, 0, 102)
	for i := range 102 {
		keyID := fmt.Sprintf("legacy-%s-%04d", cw.suffix, i)
		keys = append(keys, keyID)
		data, err := json.Marshal(resourceState{OwnerUserID: cw.owner})
		require.NoError(t, err)
		cw.insert(resourceConcept, resourceStateID(cw.site, keyID), map[string]any{"siteId": siteConcept + ":" + cw.site, "ownerUserId": cw.owner, "keyId": keyID, "stateData": string(data)})
	}
	rows, err := cw.svc.store.resourceStatePage(ctx, "callsResourceLegacyStates")
	require.NoError(t, err)
	require.Len(t, rows, 100)
	// Make the entire first page unrepairable. The last two still normalize on
	// the next pass; a shrinking queue is not an excuse to forget its cursor.
	for _, row := range rows {
		payload, version := latestPayload(t, cw.db, resourceConcept, resourceConcept+":"+row.ID)
		payload["stateData"] = "invalid journal"
		version.Payload, err = json.Marshal(payload)
		require.NoError(t, err)
		version.CreatedAt = version.CreatedAt.Add(time.Microsecond)
		_, err = cw.db.NewInsert().Model(&version).Exec(ctx)
		require.NoError(t, err)
	}
	require.Error(t, cw.svc.ReconcileResources(ctx))
	require.NoError(t, cw.svc.ReconcileResources(ctx))
	normalized := 0
	for _, id := range keys {
		row, err := cw.svc.store.resourceState(memql.ContextWithFreshRead(ctx), cw.owner, cw.site, id)
		if err == nil && row.ProjectionKnown {
			normalized++
		}
	}
	require.Equal(t, 2, normalized)
	require.Empty(t, cw.svc.lifecycle.legacyResources.cursor)
	_ = key
}
