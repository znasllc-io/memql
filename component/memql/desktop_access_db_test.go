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

// An app measured out of usage counts as set up (ruling R-D4), read from the
// person's own registration rows, while AppDoors still gives routing nothing.
func TestAnAppOutOfUsageOnARegistrationCountsAsSetUp(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	key := uniqueSuffix("desktop-access-apps")
	owner := "v1:identity:user:" + key
	registration := WorkerRegistrationConcept + ":" + key
	entry := func(id, readiness string) map[string]any {
		e := map[string]any{"id": id, "version": "1", "signedIn": true, "allowed": true, "subscription": "present"}
		if readiness != "" {
			e["readiness"] = readiness
			e["readinessCheckedAt"] = time.Now().UTC().Format(time.RFC3339)
		}
		return e
	}
	raw, _ := json.Marshal(map[string]any{
		"ownerUserId": owner, "identityId": "test", "name": "Desk", "capabilities": []string{"HEADLESS"},
		"concurrency": map[string]any{}, "registeredAt": time.Now().UTC().Format(time.RFC3339Nano),
		// Codex is out of usage and counts; Claude Code is measured signed out and does not.
		"apps": []map[string]any{entry("codex", "out_of_usage"), entry("claude-code", "signed_out")},
	})
	row := memorynodes.MemoryNode{ID: registration, Concept: WorkerRegistrationConcept, CreatedAt: time.Now().UTC(), CreatedBy: owner,
		Type: memorynodes.NodeTypeObject, Payload: raw, Schema: json.RawMessage(`{}`), Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
	_, err := db.NewInsert().Model(&row).Exec(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", registration).Exec(context.Background())
		require.NoError(t, err)
	})

	ctx := auth.ContextWithUserActor(context.Background(), owner)
	sources, reasons, err := eng.desktopAccessSources(ctx, owner)
	require.NoError(t, err)
	var apps []AccessSource
	for _, s := range sources {
		if s.Kind == AccessSourceApp {
			apps = append(apps, s)
		}
	}
	require.Equal(t, []AccessSource{{Kind: AccessSourceApp, App: "codex", State: AccessOutOfUsage, Own: true, Label: "codex on Desk"}}, apps)
	require.NotContains(t, reasons, "no app on their machines is allowed and signed in")
}

// The stored-role read is the real principal row, and tells "no such person"
// from a failed read (the hold never turns the second into a held page).
func TestTheStoredRoleIsReadFromThePrincipalRow(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	key := uniqueSuffix("desktop-access-role")
	insert := func(id string, payload map[string]any) {
		raw, _ := json.Marshal(payload)
		row := memorynodes.MemoryNode{ID: id, Concept: conceptIdentityUser, CreatedAt: time.Now().UTC(), CreatedBy: id,
			Type: memorynodes.NodeTypeObject, Payload: raw, Schema: json.RawMessage(`{}`), Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
		_, err := db.NewInsert().Model(&row).Exec(context.Background())
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
			require.NoError(t, err)
		})
	}
	owner := conceptIdentityUser + ":" + key + "-owner"
	admin := conceptIdentityUser + ":" + key + "-admin"
	gone := conceptIdentityUser + ":" + key + "-gone"
	insert(owner, map[string]any{"role": "owner", "active": true})
	insert(admin, map[string]any{"role": "admin", "active": true})
	insert(gone, map[string]any{"role": "owner", "active": false})

	ctx := context.Background()
	role, found, err := eng.storedUserRole(ctx, owner)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, auth.MayManageFleet(role))
	role, found, err = eng.storedUserRole(ctx, admin)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, auth.MayManageFleet(role))
	_, found, err = eng.storedUserRole(ctx, gone)
	require.NoError(t, err)
	require.False(t, found, "an inactive person holds no role")
	_, found, err = eng.storedUserRole(ctx, conceptIdentityUser+":"+key+"-nobody")
	require.NoError(t, err)
	require.False(t, found)
}
