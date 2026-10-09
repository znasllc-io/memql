package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

type checkpointModel struct {
	calls    int
	fail     bool
	reply    string
	messages []common.ChatMessage
}

func (p *checkpointModel) CallChatStructured(_ context.Context, messages []common.ChatMessage, _ common.StructuredSchema) (string, error) {
	p.calls++
	p.messages = messages
	if p.fail {
		return "", fmt.Errorf("summary unavailable")
	}
	if p.reply != "" {
		return p.reply, nil
	}
	return `{"facts":["The project is CNAS [0]"],"entities":["owner@example.test [1]"],"decisions":[],"constraints":[],"unfinished":[]}`, nil
}
func TestWorkCheckpointRetainsSourcesReusesExactMemoryAndIsolatesOwners(t *testing.T) {
	e, _, _ := readMergeTestEngine(t)
	owner := "v1:identity:user:" + id.NewShortId()
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	ctx = common.ContextWithRun(ctx, common.RunContext{RunId: id.NewShortId(), GoalId: "goal", OwnerUserId: owner, StepKey: "reason"})
	model := &checkpointModel{}
	e.SetAIResolver(testAIResolver{fn: func(context.Context, airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: model, Resolution: airoute.Resolution{ProviderName: "fleet:test", Model: "test"}}, nil
	}})
	messages := []common.ChatMessage{{Role: "system", Content: "Complete the work"}}
	for n := range 60 {
		messages = append(messages, common.ChatMessage{Role: "user", Content: fmt.Sprintf("Turn %d: CNAS %s", n, strings.Repeat("evidence ", 150))})
	}
	compacted, err := e.CompactWorkContext(ctx, messages, nil, 15000)
	require.NoError(t, err)
	require.Less(t, WorkContextSize(compacted, nil), 15001)
	require.Equal(t, messages[len(messages)-6:], compacted[len(compacted)-6:])
	require.Equal(t, messages[1], compacted[1], "the original request stays exact")
	require.Contains(t, compacted[2].Content, "owner@example.test")
	calls := model.calls
	require.Positive(t, calls)
	require.Contains(t, fmt.Sprint(model.messages), "<request-context>")
	require.Contains(t, fmt.Sprint(model.messages), "Turn 0: CNAS")
	require.Contains(t, fmt.Sprint(model.messages), "Turn 59: CNAS")
	again, err := e.CompactWorkContext(ctx, messages, nil, 15000)
	require.NoError(t, err)
	require.Equal(t, compacted, again)
	require.Equal(t, calls, model.calls, "exact source prefixes should reuse checkpoints")
	run, _ := common.RunFromContext(ctx)
	rows, err := e.workRows(ctx, "workObservationsForOwnerRun", run.RunId)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	require.Contains(t, fmt.Sprint(rows), "Turn 1: CNAS", "original source must remain recoverable")
	stranger := auth.ContextWithUserActor(context.Background(), "v1:identity:user:"+id.NewShortId())
	rows, err = e.workRows(stranger, "workObservationsForOwnerRun", run.RunId)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestWorkCheckpointRelevanceContextIsBoundedAndPartOfItsIdentity(t *testing.T) {
	request := "Compare field performance " + strings.Repeat("文献", 3000) + " preserve measurement uncertainty"
	messages := []common.ChatMessage{
		{Role: "system", Content: "system-only"},
		{Role: "user", Content: request},
		{Role: "tool", Content: "tool-only"},
		{Role: "user", Content: "[Memory checkpoint old]\nderived-only"},
	}
	context := workContextTask(messages)
	require.LessOrEqual(t, len(context), 4000)
	require.True(t, utf8.ValidString(context))
	require.Contains(t, context, "Compare field performance")
	require.Contains(t, context, "preserve measurement uncertainty")
	for _, excluded := range []string{"system-only", "tool-only", "derived-only"} {
		require.NotContains(t, context, excluded)
	}
	source := []byte(`[{"Role":"tool","Content":"exact evidence"}]`)
	before := workCheckpointHash(source, context)
	messages = append(messages, common.ChatMessage{Role: "user", Content: "Correction: compare regeneration costs"})
	corrected := workContextTask(messages)
	require.LessOrEqual(t, len(corrected), 4000)
	require.True(t, utf8.ValidString(corrected))
	require.Contains(t, corrected, "Compare field performance")
	require.Contains(t, corrected, "Correction: compare regeneration costs")
	require.NotEqual(t, before, workCheckpointHash(source, corrected), "a changed objective must not reuse differently focused memory")
	require.Equal(t, before, workCheckpointHash(source, context))
	require.Empty(t, workContextTask(nil))
}
func TestWorkContextSizeCountsToolSchemasAndArguments(t *testing.T) {
	messages := []common.ChatMessage{{Role: "assistant", ToolCalls: []common.ToolCall{{Name: "execute", Arguments: strings.Repeat("x", 30000)}}}}
	require.Greater(t, WorkContextSize(messages, nil), 10000)
}

func TestWorkCheckpointBoundsWholeEntriesIncludingUnicode(t *testing.T) {
	checkpoint := workCheckpoint{
		Facts:       []string{strings.Repeat("文献", 4000), "Measured result [3]"},
		Entities:    []string{"https://example.test/exact-source [4]"},
		Decisions:   []string{"Keep uncertainty explicit [5]"},
		Constraints: []string{"Do not repeat the completed upload [6]"},
		Unfinished:  []string{"Verify field evidence [7]"},
	}
	got := boundedWorkCheckpoint(checkpoint, 512)
	require.LessOrEqual(t, len(got), 512)
	var decoded workCheckpoint
	require.NoError(t, json.Unmarshal([]byte(got), &decoded))
	require.Contains(t, decoded.Unfinished, checkpoint.Unfinished[0])
	require.Contains(t, decoded.Constraints, checkpoint.Constraints[0])
	require.Contains(t, decoded.Entities, checkpoint.Entities[0])
	require.NotContains(t, got, "文", "do not publish truncated claims")
	require.Contains(t, got, "incomplete")
	checkpoint.Facts = []string{"Short evidence [3]"}
	raw, _ := json.Marshal(checkpoint)
	require.Equal(t, string(raw), boundedWorkCheckpoint(checkpoint, 6000))
}
