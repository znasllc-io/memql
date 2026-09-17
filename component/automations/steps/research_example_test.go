package steps

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
)

// Execute the published source through the real statement runner, replacing
// only engine calls. No database, embeddings or model inference is performed.
func researchRegion(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("../../../examples/research-desk/research/brief.memql")
	require.NoError(t, err)
	_, rest, ok := strings.Cut(string(data), "// showcase:"+name+":start\n")
	require.True(t, ok)
	src, _, ok := strings.Cut(rest, "// showcase:"+name+":end")
	require.True(t, ok)
	return src
}

func TestResearchExampleRetrievalAndAgentReply(t *testing.T) {
	sources := []any{map[string]any{"title": "Failover", "snippet": "Restore the replica.", "artifactId": "a1"}}
	funcs := &recordingFunctions{results: map[string]any{
		"librarySimilarArtifacts": rows(map[string]any{"id": "hit1", "payload": sources[0]}),
		"runAgentTurn":            rows(map[string]any{"id": "turn1", "payload": map[string]any{"reply": "A draft citing a1"}}),
	}}
	runner := automations.NewLogicRunner(&memql.MemQLEngine{}, v1Registry(funcs, &recordingEvents{}), nil)
	got, err := runner.RunLogicBody(context.Background(), "relevantResearchFiles", compiledLogicForSteps(t, researchRegion(t, "search")), map[string]any{"question": "How do we fail over?"})
	require.NoError(t, err)
	require.Equal(t, sources, got)
	require.Equal(t, "How do we fail over?", funcs.named("librarySimilarArtifacts")[0].args["text"])
	reply, err := runner.RunLogicBody(context.Background(), "draftResearchAnswer", compiledLogicForSteps(t, researchRegion(t, "ai")), map[string]any{"agentId": "agent1", "question": "How do we fail over?", "sources": got})
	require.NoError(t, err)
	require.Equal(t, "A draft citing a1", reply)
	call := funcs.named("runAgentTurn")[0]
	require.Equal(t, "agent1", call.args["agentId"])
	for _, part := range []string{"How do we fail over?", "Restore the replica.", "a1"} {
		require.Contains(t, call.args["prompt"], part)
	}
}

func TestResearchExampleAutomationBranches(t *testing.T) {
	for _, hasSources := range []bool{false, true} {
		name := "no sources skips inference"
		sources := []any{}
		if hasSources {
			name = "sources produce a saved draft"
			sources = append(sources, map[string]any{"artifactId": "a1", "snippet": "Restore the replica."})
		}
		t.Run(name, func(t *testing.T) {
			funcs := &recordingFunctions{results: map[string]any{"relevantResearchFiles": sources, "draftResearchAnswer": "A draft citing a1"}}
			runV1Automation(t, researchRegion(t, "automation"), funcs, map[string]any{"id": "brief1", "question": "How do we fail over?", "agentId": "agent1"})
			saved := funcs.named("saveResearchBrief")
			require.Len(t, saved, 1)
			require.Equal(t, "brief1", saved[0].args["briefId"])
			require.Equal(t, "How do we fail over?", saved[0].args["question"])
			if hasSources {
				require.Len(t, funcs.named("draftResearchAnswer"), 1)
				require.Equal(t, "draft", saved[0].args["status"])
				require.Equal(t, "A draft citing a1", saved[0].args["answer"])
			} else {
				require.Empty(t, funcs.named("draftResearchAnswer"))
				require.Equal(t, "no_sources", saved[0].args["status"])
			}
			require.Equal(t, sources, saved[0].args["sources"])
		})
	}
}
