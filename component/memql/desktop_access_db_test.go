package memql

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
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

// insertAccessRow writes one row for a desktop-access db test and removes it
// when the test ends.
func insertAccessRow(t *testing.T, db *bun.DB, concept, id string, payload map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	row := memorynodes.MemoryNode{ID: id, Concept: concept, CreatedAt: time.Now().UTC(), CreatedBy: id,
		Type: memorynodes.NodeTypeObject, Payload: raw, Schema: json.RawMessage(`{}`), Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
	_, err := db.NewInsert().Model(&row).Exec(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
		require.NoError(t, err)
	})
}

// The two forms of "owner or developer of another account" the cluster can
// express today (ruling R-D1, amended). Neither is the cluster role, so both
// are held, through the REAL stored-role read.
func TestAnAccountScopedRoleIsHeld(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	rec := installRecorder(t)
	key := uniqueSuffix("desktop-access-scoped")

	t.Run("a stored role the catalog scopes to an account", func(t *testing.T) {
		// The installed catalog says "developer" is scoped to one account, the
		// way a role row with an accountId builds it (buildRbacCatalog).
		// MayManageFleet then refuses it although the slug is developer's.
		prior := auth.InstalledCapabilityCatalog()
		auth.SetCapabilityCatalog(buildRbacCatalog(roleNodes(
			roleRow{id: "r-dev", slug: "developer", rank: 300, active: true, account: "acct-" + key},
			roleRow{id: "r-own", slug: "owner", rank: 400, active: true},
		), nil))
		t.Cleanup(func() { auth.SetCapabilityCatalog(prior) })

		user := conceptIdentityUser + ":" + key + "-scoped"
		insertAccessRow(t, db, conceptIdentityUser, user, map[string]any{"role": "developer", "active": true})
		role, found, err := eng.storedUserRole(context.Background(), user)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "acct-"+key, auth.RoleAccountScope(role), "the fixture must really scope the role")
		got, err := eng.desktopAccess(auth.ContextWithUserActor(context.Background(), user), user, "", "")
		require.NoError(t, err)
		require.False(t, got.Admitted || got.Exempt, "answer = %+v", got)
		require.Regexp(t, accessReferencePattern, got.Reference)
	})

	t.Run("a plain role and membership of an account group", func(t *testing.T) {
		// groupMembership has no owning-capacity field: being a member of the
		// account's group is all the cluster records, and it exempts nobody.
		user := conceptIdentityUser + ":" + key + "-member"
		group := conceptIdentityGroup + ":acct-" + key
		insertAccessRow(t, db, conceptIdentityUser, user, map[string]any{"role": "writer", "active": true})
		insertAccessRow(t, db, conceptIdentityGroup, group, map[string]any{"kind": "account", "accountId": "acct-" + key, "status": "active", "name": "Acme"})
		insertAccessRow(t, db, conceptIdentityGroupMembership, conceptIdentityGroupMembership+":"+key, map[string]any{"groupId": group, "userId": user, "status": "active", "origin": "added"})
		got, err := eng.desktopAccess(auth.ContextWithUserActor(context.Background(), user), user, "", "")
		require.NoError(t, err)
		require.False(t, got.Admitted || got.Exempt, "answer = %+v", got)
		require.Regexp(t, accessReferencePattern, got.Reference)
	})
	require.GreaterOrEqual(t, len(rec.lines), 2)
}

// A failed read is an error, not "no such person": a cancelled context makes
// the real read fail.
func TestAFailedStoredRoleReadIsAnError(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, found, err := eng.storedUserRole(ctx, conceptIdentityUser+":anyone")
	require.Error(t, err)
	require.False(t, found)
	_, err = eng.desktopAccess(auth.ContextWithUserActor(ctx, "anyone"), "anyone", "", "")
	require.Error(t, err)
}
