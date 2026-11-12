package memql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// The model setup view shows a machine's owner what the installation's catalog
// research has found: the latest review's summary and when it was made, its
// candidates' public fields, and whether weekly research is on. Those records
// belong to the installation and their tier admits a cluster owner alone, so
// the handler reads them itself, after it has admitted the caller to their own
// machine, and hands on that projection and nothing else: not the research
// owner's identity, and not the review's run.
//
// The caller here owns the machine and is not a cluster owner.
func TestTheModelSetupViewShowsCatalogResearchToAMachineOwner(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	key := uniqueSuffix("model-setup-research")
	owner := "v1:identity:user:" + key
	researchOwner := "v1:identity:user:" + key + "-research-owner"
	registration := WorkerRegistrationConcept + ":" + key
	candidate := key + "-candidate"
	insert := func(conceptID, rowID string, at time.Time, payload map[string]any) {
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		row := memorynodes.MemoryNode{ID: rowID, Concept: conceptID, CreatedAt: at, CreatedBy: owner, Type: memorynodes.NodeTypeObject, Payload: raw, Schema: json.RawMessage(`{}`), Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
		_, err = db.NewInsert().Model(&row).Exec(context.Background())
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", conceptID).Where("id = ?", rowID).Where(`"createdAt" = ?`, at).Exec(context.Background())
			require.NoError(t, err)
		})
	}
	now := time.Now().UTC()
	insert(WorkerRegistrationConcept, registration, now, map[string]any{"ownerUserId": owner, "identityId": "test", "name": "Test machine", "registeredAt": now.Format(time.RFC3339Nano), "capabilities": []string{"MODEL"}, "concurrency": map[string]any{}, "labels": map[string]any{}, "hardware": map[string]any{"memoryBytes": 64 << 30, "gpu": map[string]any{"backend": "metal"}, "runtimes": []any{map[string]any{"name": "ollama"}}}, "platformInfo": map[string]any{"os": "darwin"}})

	// Three reviews, each dated past any a shared database holds. The latest
	// by reviewedAt that is not deleted is the one the view shows: an earlier
	// one is not, and neither is a later one whose newest version is deleted.
	const reviewConcept, settingsConcept = "v1:models:catalogReview", "v1:models:catalogResearchSettings"
	review := func(suffix, reviewedAt, summary string, deleted bool) {
		id := reviewConcept + ":" + key + "-" + suffix
		insert(reviewConcept, id, now, map[string]any{"runId": "v1:work:run:" + key + "-run", "reviewedAt": reviewedAt, "summary": summary,
			"findings": []any{map[string]any{"modelId": candidate, "category": "text", "runtime": "ollama", "summary": "A candidate to verify.", "sources": []any{"https://example.com/" + candidate}, "internalNote": key + "-internal"}}})
		if deleted {
			insert(reviewConcept, id, now.Add(time.Second), map[string]any{"id": id, "deleted": true})
		}
	}
	review("earlier", "2998-01-01T00:00:00Z", key+" earlier review", false)
	review("latest", "2998-06-01T00:00:00Z", key+" latest review", false)
	review("withdrawn", "2998-12-01T00:00:00Z", key+" withdrawn review", true)
	// The opt-in, as the newest version of the installation's one settings
	// row; the cleanup removes this version and leaves any other in place.
	insert(settingsConcept, settingsConcept+":weekly", now, map[string]any{"enabled": true, "ownerUserId": researchOwner})

	nodes, err := eng.evaluateFleetModelSetup(auth.ContextWithUserActor(context.Background(), owner), map[string]any{"registrationId": registration})
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	var view map[string]any
	require.NoError(t, json.Unmarshal(nodes[0].Payload, &view))

	require.Equal(t, key+" latest review", view["researchSummary"])
	require.Equal(t, "2998-06-01T00:00:00Z", view["researchAt"])
	require.Equal(t, true, view["researchEnabled"])
	var found map[string]any
	for _, entry := range view["catalog"].([]any) {
		if e, _ := entry.(map[string]any); e["modelId"] == candidate {
			found = e
		}
	}
	require.NotNil(t, found, "the latest review's candidate is not in the view's catalog")
	require.Equal(t, "text", found["category"])
	require.Equal(t, "ollama", found["runtime"])
	require.Equal(t, "https://example.com/"+candidate, found["source"])
	require.Equal(t, "A candidate to verify.", found["notes"])
	require.Equal(t, false, found["installed"])

	// What the view does not carry.
	raw := string(nodes[0].Payload)
	for what, value := range map[string]string{
		"the research owner's identity":               researchOwner,
		"the review's run":                            key + "-run",
		"a field of a finding the view does not show": key + "-internal",
	} {
		require.False(t, strings.Contains(raw, value), "the view carries %s", what)
	}
}

// A caller with no machine of that registration is refused before anything of
// the installation's is read.
func TestTheModelSetupViewAdmitsTheMachinesOwnerBeforeReadingResearch(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	key := uniqueSuffix("model-setup-research-admission")
	owner := "v1:identity:user:" + key
	registration := WorkerRegistrationConcept + ":" + key
	now := time.Now().UTC()
	raw, err := json.Marshal(map[string]any{"ownerUserId": owner, "identityId": "test", "name": "Test machine", "registeredAt": now.Format(time.RFC3339Nano), "capabilities": []string{"MODEL"}, "concurrency": map[string]any{}, "labels": map[string]any{}, "hardware": map[string]any{"memoryBytes": 64 << 30}, "platformInfo": map[string]any{"os": "darwin"}})
	require.NoError(t, err)
	row := memorynodes.MemoryNode{ID: registration, Concept: WorkerRegistrationConcept, CreatedAt: now, CreatedBy: owner, Type: memorynodes.NodeTypeObject, Payload: raw, Schema: json.RawMessage(`{}`), Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
	_, err = db.NewInsert().Model(&row).Exec(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", WorkerRegistrationConcept).Where("id = ?", registration).Exec(context.Background())
		require.NoError(t, err)
	})
	for name, ctx := range map[string]context.Context{
		"another person": auth.ContextWithUserActor(context.Background(), "v1:identity:user:"+key+"-somebody-else"),
		"nobody":         context.Background(),
	} {
		_, err := eng.evaluateFleetModelSetup(ctx, map[string]any{"registrationId": registration})
		require.ErrorContains(t, err, "no machine of yours", name)
	}
}
