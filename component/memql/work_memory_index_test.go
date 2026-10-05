package memql

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
)

type memoryIndexTestIntegration struct{ handler builtinExecutorHandler }

func (m memoryIndexTestIntegration) IntegrationName() string { return "embedding" }
func (m memoryIndexTestIntegration) Capabilities() []IntegrationCapability {
	return []IntegrationCapability{{Name: "store", Handler: m.handler}}
}

func TestConversationSemanticEvidenceAcrossReplicasAndSourceChanges(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	a.database().DB.SetMaxOpenConns(4)
	b.database().DB.SetMaxOpenConns(4)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	ctx, cancel := context.WithTimeout(askTestActor(), 30*time.Second)
	defer cancel()
	owner, _ := auth.AccessFromContext(ctx)
	conversation := askTestConversation(t, a, ctx)
	turn := AskTurn{ID: "format", Prompt: "For reports, I prefer concise bullet points.", Answer: "Understood.", State: "done", StartedAt: time.Now().UTC()}
	require.NoError(t, a.askSave(ctx, conversation, "Preference", askTranscript{Turns: []AskTurn{turn}}))
	binding, bindErr := a.ReadEmbedderBinding(ctx)
	if bindErr != nil {
		for _, write := range []struct {
			name string
			args map[string]any
		}{
			{"platform.recordEmbedderBindingPlan", map[string]any{"bindingId": "active", "providerRef": "memory-index-test", "dimensions": 1536}},
			{"platform.activateEmbedderBinding", map[string]any{"bindingId": "active", "activatedAt": time.Now().UTC().Format(time.RFC3339Nano)}},
		} {
			call, _ := parser.RenderCall(write.name, write.args)
			_, err := a.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
			require.NoError(t, err)
		}
		binding, bindErr = a.ReadEmbedderBinding(ctx)
	}
	require.NoError(t, bindErr)
	table, err := EnsureEmbeddingVectorTable(ctx, a.database().DB, binding.ProviderRef, binding.Dimensions)
	require.NoError(t, err)
	var embedded atomic.Int32
	store := memoryIndexTestIntegration{handler: func(c context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		embedded.Add(1)
		vector := "[1," + strings.Repeat("0,", binding.Dimensions-2) + "0]"
		_, err := a.database().DB.ExecContext(c, `INSERT INTO `+table+`(id,concept,vector_field,embedding) VALUES($1,$2,'content',$3::vector) ON CONFLICT(id,vector_field) DO NOTHING`, args["nodeId"], conversationEvidenceConcept, vector)
		return nil, err
	}}
	require.NoError(t, a.integrations.Register(store))
	require.NoError(t, b.integrations.Register(store))
	// Two consumers race the same persisted source on separate DB pools.
	errors := make(chan error, 2)
	for _, engine := range []*MemQLEngine{a, b} {
		go func(e *MemQLEngine) {
			_, err := e.indexConversationMemoryBuiltin(ctx, map[string]any{"conversationId": conversation}, 0)
			errors <- err
		}(engine)
	}
	require.NoError(t, <-errors)
	require.NoError(t, <-errors)
	require.Equal(t, int32(1), embedded.Load(), "a repeat consumes the durable vector written by the other replica")
	readEvidence := func() []map[string]any {
		rows, err := a.database().DB.QueryContext(ctx, `SELECT payload FROM "MemoryNodes" WHERE concept=$1 AND payload->>'ownerUserId'=$2`, conversationEvidenceConcept, owner.UserId)
		require.NoError(t, err)
		defer rows.Close()
		var out []map[string]any
		for rows.Next() {
			var raw []byte
			require.NoError(t, rows.Scan(&raw))
			var row map[string]any
			require.NoError(t, json.Unmarshal(raw, &row))
			row["_similarity"] = .93
			out = append(out, row)
		}
		require.NoError(t, rows.Err())
		return out
	}
	oldCandidates := readEvidence()
	require.Len(t, oldCandidates, 1)
	// The vector retrieval is a fixture here; the owning replica, source
	// validation and durable writes are real. The similarity integration has
	// its own database/provider cache test.
	b.builtinExecutorHandlers["integration.similarity.similarTo"] = func(c context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		require.Equal(t, []any{conversationMemoryDomain(owner.UserId)}, args["domains"])
		var nodes []memorynodes.MemoryNode
		for _, candidate := range oldCandidates {
			raw, _ := json.Marshal(candidate)
			nodes = append(nodes, memorynodes.MemoryNode{ID: "fixture", Payload: raw})
		}
		return nodes, nil
	}
	nodes, err := b.workRecallMemoryBuiltin(ctx, map[string]any{"search": "How should you format my reports?"}, 0)
	require.NoError(t, err)
	require.Contains(t, string(nodes[0].Payload), "concise bullet points")
	// An edit/correction of the source invalidates its earlier vector even
	// before indexing has caught up. A cached neighbor is never its own proof.
	turn.Prompt = "Correction: use short paragraphs for reports."
	require.NoError(t, a.askSave(ctx, conversation, "Preference", askTranscript{Turns: []AskTurn{turn}}))
	nodes, err = b.workRecallMemoryBuiltin(ctx, map[string]any{"search": "How should you format my reports?"}, 0)
	require.NoError(t, err)
	require.NotContains(t, string(nodes[0].Payload), "concise bullet points")
	other := askTestActor()
	_, err = b.indexConversationMemoryBuiltin(other, map[string]any{"conversationId": conversation, "ownerUserId": owner.UserId}, 0)
	require.Error(t, err, "a supplied owner cannot lend a private source")
	forged := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:automation:untrusted", Synthetic: true}))
	_, err = b.indexConversationMemoryBuiltin(forged, map[string]any{"conversationId": conversation, "ownerUserId": owner.UserId}, 0)
	require.Error(t, err)
}

func TestWorkViewerContextFreshAndMinimal(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := askTestActor()
	owner, _ := auth.AccessFromContext(ctx)
	write := func(name string, at time.Time) {
		raw, _ := json.Marshal(map[string]any{"displayName": name, "firstName": name, "primaryEmail": "never-forward@example.test", "phone": "private-phone", "primaryRole": "Engineer", "role": "writer", "active": true})
		_, err := a.database().DB.ExecContext(ctx, `INSERT INTO "MemoryNodes"(id,concept,"createdAt","createdBy",schema,payload) VALUES($1,'v1:identity:user',$2,'viewer-test','{}',$3)`, owner.UserId, at, string(raw))
		require.NoError(t, err)
	}
	write("First", time.Now().Add(-time.Minute))
	nodes, err := b.workViewerContextBuiltin(ctx, nil, 0)
	require.NoError(t, err)
	require.Contains(t, string(nodes[0].Payload), `"displayName":"First"`)
	require.NotContains(t, string(nodes[0].Payload), "never-forward")
	require.NotContains(t, string(nodes[0].Payload), "private-phone")
	write("José", time.Now())
	nodes, err = b.workViewerContextBuiltin(ctx, nil, 0)
	require.NoError(t, err)
	require.Contains(t, string(nodes[0].Payload), `"displayName":"José"`)
	require.NotContains(t, string(nodes[0].Payload), `"displayName":"First"`)
	other, err := b.workViewerContextBuiltin(askTestActor(), nil, 0)
	require.NoError(t, err)
	require.NotContains(t, string(other[0].Payload), "José")
}

func TestWorkViewerOrganizationScope(t *testing.T) {
	a, _, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	suffix := uniqueSuffix("viewer")
	account, member, group := "acct-"+suffix, "member-"+suffix, "g-"+suffix
	seedPrincipal(t, a, member, auth.RoleWriter)
	seedAccountOwnedBy(t, a, "owner-"+suffix, auth.RoleOwner, account, "Visible organization "+suffix)
	seedGroup(t, a, group, "Organization members", "custom", account)
	seedMembership(t, a, group, member, "active")
	ctx := rankActorCtx(member, auth.RoleWriter)
	nodes, err := b.workViewerContextBuiltin(ctx, nil, 0)
	require.NoError(t, err)
	require.Contains(t, string(nodes[0].Payload), "Visible organization "+suffix)
	outsider, err := b.workViewerContextBuiltin(rankActorCtx("outside-"+suffix, auth.RoleWriter), nil, 0)
	require.NoError(t, err)
	require.NotContains(t, string(outsider[0].Payload), "Visible organization "+suffix)
	seedMembership(t, a, group, member, "removed")
	nodes, err = b.workViewerContextBuiltin(ctx, nil, 0)
	require.NoError(t, err)
	require.NotContains(t, string(nodes[0].Payload), "Visible organization "+suffix)
}
