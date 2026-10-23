//go:build agent

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/agent"
	"github.com/znasllc-io/memql/integrations/similarity"
)

type knowledgeTurnSink struct{}

func (knowledgeTurnSink) TextDelta(string)                  {}
func (knowledgeTurnSink) ToolCall(string, string, string)   {}
func (knowledgeTurnSink) ToolResult(string, string, string) {}

// The writer publishes reviewed synthetic manuals; a different engine owns
// similarity, its real CognitionEngineAdapter, and both Replier lanes. Only the
// embedding/model providers are deterministic. No hand-built result envelope
// can hide a mismatch between the builtin, adapter, decoder and rendered prompt.
func TestWorkDBRequiredKnowledgeReachesPromptOnAnotherReplica(t *testing.T) {
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	defer gates.Close()
	key := strings.ReplaceAll(id.NewShortId(), "-", "")
	owner, goalID, runID := key+"owner", key+"goal", key+"run"
	person := auth.ContextWithClientOrigin(auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner}))
	provider := &trainingContextProvider{}
	old, had := memql.ActiveEmbedderBinding()
	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: key, Dimensions: 1024})
	t.Cleanup(func() {
		if had {
			memql.SetActiveEmbedderBinding(old)
		} else {
			memql.ClearActiveEmbedderBinding()
		}
	})
	table, err := memql.EnsureEmbeddingVectorTable(person, db.DB, key, 1024)
	require.NoError(t, err)
	t.Cleanup(func() {
		db.ExecContext(context.Background(), "DROP TABLE "+table)
		db.ExecContext(context.Background(), "DELETE FROM embedding_cache WHERE provider=?", key)
	})
	resolve := func(context.Context, string) (memql.EmbeddingAIProvider, error) { return provider, nil }
	for _, plugin := range memql.RegisteredPlugins() {
		if plugin.Name != "library" && plugin.Name != "work" && plugin.Name != "knowledge" {
			continue
		}
		native, err := plugin.Factory(memql.PluginContext{ResolvePartitionFromContext: func(context.Context) string { return "default" }, Engine: reader, Logger: reader.Logger, BunDB: func() *bun.DB { return db }, GateDB: func() *sql.DB { return gates }, ConceptDataIsStaged: reader.ConceptDataIsStaged, AdmitSourceRow: memql.AdmitSourceRow, ResolveEmbeddingProvider: resolve})
		require.NoError(t, err)
		require.NoError(t, reader.RegisterIntegration(native))
	}
	retrieve := similarity.New(reader.Logger)
	retrieve.SetDBGetter(func() *sql.DB { return db.DB })
	retrieve.SetStagedConceptPredicate(reader.ConceptDataIsStaged)
	retrieve.SetEmbeddingProvider(resolve)
	require.NoError(t, reader.RegisterIntegration(retrieve))
	registry := memql.NewProviderRegistryForTest()
	registry.RegisterWithParamsForTest("fixture", "OpenAI", "fixture", map[string]any{"contextWindow": 200000}, provider)
	rules := memql.NewRuleRegistry()
	require.NoError(t, rules.Register(&memql.RuleConfig{Name: memql.DefaultRuleName, Policy: "fixture", Locked: true, OnUnavailable: memql.OnUnavailableDegrade, SourceFile: "dsl/rules/rules.memql"}))
	require.NoError(t, rules.Finalize())
	rtr := router.New(registry, memql.NewPolicyRegistryForTest(map[string][]string{"fixture": {"fixture"}}), rules, nil, reader.Logger)
	adapter := &CognitionEngineAdapter{Engine: reader}
	replier, err := agent.NewReplier(adapter, rtr, reader.Logger)
	require.NoError(t, err)
	replier.ComputerUseStatus = func(context.Context, string) (string, string, string) { return "connected", "Synthetic worker", "" }
	write := func(name string, args map[string]any) {
		templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
	}
	write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Read a reviewed capability manual", "origin": "user"})
	write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "fixture", "templateFingerprint": "fixture", "mode": "live", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	texts := map[string]string{}
	for _, fixture := range []struct {
		name, domain, status string
		score                float64
	}{
		{"a-lower", "computer-use", "validated", 0.8},
		{"z-higher", "computer-use", "validated", 0.9},
		{"pending", "computer-use", "", 1},
		{"rejected", "computer-use", "rejected", 1},
		{"superseded", "computer-use", "validated", 1},
		{"outside-domain", key + "other", "validated", 1},
	} {
		chunkID := key + fixture.name
		texts[fixture.name] = "Synthetic manual " + key + " " + fixture.name + "."
		write("createDocumentChunk", map[string]any{"chunkId": chunkID, "domainId": fixture.domain, "text": texts[fixture.name], "source": "appStructure", "sourceRef": "fixture:" + fixture.name})
		if fixture.status != "" {
			write("setChunkValidationStatus", map[string]any{"chunkId": chunkID, "status": fixture.status})
		}
		if fixture.name == "superseded" {
			write("markChunkSuperseded", map[string]any{"chunkId": chunkID, "supersededAt": time.Now().UTC().Format(time.RFC3339Nano)})
		}
		vector := make([]float32, 1024)
		vector[0], vector[1] = float32(fixture.score), float32(math.Sqrt(1-fixture.score*fixture.score))
		encoded, err := json.Marshal(vector)
		require.NoError(t, err)
		_, err = db.ExecContext(person, "INSERT INTO "+table+" (id,concept,vector_field,embedding) VALUES (?, 'v1:knowledge:documentChunk', 'content', ?::vector)", id.BuildNodeId("v1:knowledge:documentChunk", chunkID), string(encoded))
		require.NoError(t, err)
	}
	call, err := parser.RenderCall("similarTo", map[string]any{"text": "Read the current manual", "concept": "v1:knowledge:documentChunk", "domains": []string{"computer-use"}, "limit": 5})
	require.NoError(t, err)
	raw, err := adapter.Execute(person, call)
	require.NoError(t, err)
	envelope, ok := raw.(map[string]any)
	require.True(t, ok)
	require.NotContains(t, envelope, "bundle", "the regression must exercise the real data-only builtin result")
	nodes, ok := envelope["data"].(map[string]memorynodes.MemoryNode)
	require.True(t, ok)
	require.Len(t, nodes, 2)
	for _, name := range []string{"a-lower", "z-higher"} {
		canonicalID := id.BuildNodeId("v1:knowledge:documentChunk", key+name)
		require.Equal(t, canonicalID, nodes[canonicalID].ID)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(nodes[canonicalID].Payload, &payload))
		require.Equal(t, texts[name], payload["text"])
		require.Greater(t, payload["_similarity"].(float64), 0.79)
	}
	turn := func(lane, step string) *agent.TurnResult {
		// Direct Replier calls still need the versioned running step that
		// normal runAgentTurn execution records before selecting skills.
		write("createWorkStep", map[string]any{"stepId": key + step, "runId": runID, "key": step, "seq": 0, "stepType": "automation", "status": "running", "attempt": 1, "version": 1, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
		ctx := common.ContextWithRun(person, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: owner, StepKey: step, Mode: common.RunModeLive})
		result, err := replier.Handle(ctx, &memqlv1.AgentGenerateTurnMsg{RequestId: key + step, AgentId: "synthetic", ActingAgent: &memqlv1.ActingAgentIdentity{Id: "synthetic", Role: "assistant"}, Hints: map[string]string{agent.ExecutionLaneHintKey: lane}, History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: "Read the current manual"}}}, knowledgeTurnSink{})
		require.NoError(t, err)
		return result
	}
	for _, lane := range []string{"background", "interactive"} {
		result := turn(lane, lane)
		require.Len(t, result.RetrievedChunks, 2)
		require.Equal(t, "fixture:z-higher", result.RetrievedChunks[0].SourceRef)
		require.InDelta(t, 0.9, result.RetrievedChunks[0].Similarity, 0.00001)
		system := provider.requests[len(provider.requests)-1][0].Content
		require.Contains(t, system, texts["a-lower"])
		require.Contains(t, system, texts["z-higher"])
		require.Less(t, strings.Index(system, texts["z-higher"]), strings.Index(system, texts["a-lower"]), "preserve score ranking, not the node map's ID order")
		for _, name := range []string{"pending", "rejected", "superseded", "outside-domain"} {
			require.NotContains(t, system, texts[name])
		}
	}
	// A peer review change must not fall back to the previously approved row,
	// cached prompt or graph envelope when the current answer is empty.
	for _, name := range []string{"a-lower", "z-higher"} {
		write("setChunkValidationStatus", map[string]any{"chunkId": key + name, "status": "rejected"})
	}
	require.Empty(t, turn("background", "after-review").RetrievedChunks)
	system := provider.requests[len(provider.requests)-1][0].Content
	for _, text := range texts {
		require.NotContains(t, system, text)
	}
}
