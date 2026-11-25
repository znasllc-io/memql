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
	require.Equal(t, []AccessSource{{Kind: AccessSourceApp, State: AccessOutOfUsage, Own: true, Label: "codex on Desk"}}, apps)
	require.NotContains(t, reasons, "no app on their machines is allowed and signed in")
}
