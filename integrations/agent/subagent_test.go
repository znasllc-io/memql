package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/core/common"
)

// transcriptPhrase is a sentinel that stands in for the raw human
// conversation transcript. The boundary tests assert it NEVER appears in a
// specialist's scoped window.
const transcriptPhrase = "SECRET_HUMAN_TRANSCRIPT_LINE"

// =============================================================================
// AC1: a specialist cannot produce a human-facing turn (no respondToUser).
// =============================================================================

func TestRoleAllowsTool_RespondToUserAssistantOnly(t *testing.T) {
	assert.True(t, RoleAllowsTool(RoleAssistant, RespondToUserToolName),
		"assistant must hold respondToUser")
	assert.False(t, RoleAllowsTool(RoleSpecialist, RespondToUserToolName),
		"specialist must NOT hold respondToUser")
	assert.False(t, RoleAllowsTool(RoleUnknown, RespondToUserToolName),
		"undeclared role fails closed -- no human-facing tool")

	// Every non-sentinel tool is allowed for both roles.
	for _, role := range []HarnessRole{RoleAssistant, RoleSpecialist, RoleUnknown} {
		assert.True(t, RoleAllowsTool(role, "recall"), "recall allowed for %s", role)
		assert.True(t, RoleAllowsTool(role, "workbenchHost"), "workbenchHost allowed for %s", role)
	}
}

func TestScopeToolsForRole_SpecialistExcludesRespondToUser(t *testing.T) {
	in := []string{"recall", RespondToUserToolName, "workbenchHost", "uiClick"}

	assistant := ScopeToolsForRole(RoleAssistant, in)
	assert.Equal(t, in, assistant, "assistant keeps the full set, order preserved")

	specialist := ScopeToolsForRole(RoleSpecialist, in)
	assert.NotContains(t, specialist, RespondToUserToolName,
		"specialist tool set must exclude respondToUser")
	assert.Equal(t, []string{"recall", "workbenchHost", "uiClick"}, specialist,
		"specialist keeps every other tool, order preserved")
}

func TestScopeToolDefinitionsForRole_SpecialistExcludesRespondToUser(t *testing.T) {
	defs := []common.ToolDefinition{
		{Name: "recall"},
		RespondToUserToolDefinition(),
		{Name: "workbenchHost"},
	}

	specialist := ScopeToolDefinitionsForRole(RoleSpecialist, defs)
	for _, d := range specialist {
		assert.NotEqual(t, RespondToUserToolName, d.Name,
			"specialist wire tool set must not carry respondToUser")
	}
	assert.Len(t, specialist, 2)

	assistant := ScopeToolDefinitionsForRole(RoleAssistant, defs)
	assert.Len(t, assistant, 3, "assistant keeps respondToUser definition")
}

func TestResolveHarnessRole(t *testing.T) {
	cases := map[string]HarnessRole{
		"specialist": RoleSpecialist,
		"SPECIALIST": RoleSpecialist,
		"assistant":  RoleAssistant,
		"":           RoleAssistant, // legacy / undeclared -> human-facing chat
		"garbage":    RoleAssistant,
	}
	for hint, want := range cases {
		assert.Equal(t, want, ResolveHarnessRole(hint), "hint=%q", hint)
	}
}

// =============================================================================
// AC2: specialist context = role + step input + scoped recall; the full
// transcript is absent.
// =============================================================================

func TestBuildScopedSpecialistContext_ContainsRolePromptInputAndRecall(t *testing.T) {
	step := SpecialistStep{
		StepID:      "v1:harness:step:s1",
		PlanID:      "v1:harness:plan:p1",
		RolePrompt:  "You are the Research specialist.",
		Title:       "Find prior art",
		Input:       map[string]any{"query": "graph databases", "limit": 5},
		RecallTopic: "graph databases prior art",
	}
	recall := func(_ context.Context, topic string, k int) ([]map[string]any, error) {
		assert.Equal(t, "graph databases prior art", topic)
		assert.Equal(t, defaultSpecialistRecallK, k)
		return []map[string]any{
			{"content": "Last time pgvector cosine worked best."},
			{"content": "TimescaleDB hypertable prunes by createdAt."},
		}, nil
	}

	msgs, err := BuildScopedSpecialistContext(context.Background(), step, ScopedContextOptions{Recall: recall})
	require.NoError(t, err)
	require.Len(t, msgs, 3, "role + recall + input")

	// [0] role prompt
	assert.Equal(t, "system", msgs[0].Role)
	assert.Equal(t, "You are the Research specialist.", msgs[0].Content)

	// [1] scoped recall block
	assert.Equal(t, "system", msgs[1].Role)
	assert.Contains(t, msgs[1].Content, "RELEVANT MEMORY")
	assert.Contains(t, msgs[1].Content, "pgvector cosine")

	// [2] step input
	assert.Equal(t, "user", msgs[2].Role)
	assert.Contains(t, msgs[2].Content, "Find prior art")
	assert.Contains(t, msgs[2].Content, "query: graph databases")
	assert.Contains(t, msgs[2].Content, "limit: 5")
}

func TestBuildScopedSpecialistContext_TranscriptIsAbsent(t *testing.T) {
	// The recall hook would be the ONLY place a leak could occur; even if it
	// (wrongly) returned the transcript, the assertion below would catch it.
	// Critically, the builder has no parameter that could carry the
	// transcript -- the boundary is structural, not just behavioral.
	step := SpecialistStep{
		StepID:     "v1:harness:step:s2",
		RolePrompt: "You are a specialist. " + "Do not see the human chat.",
		Title:      "Compute totals",
		Input:      map[string]any{"a": 1, "b": 2},
	}
	// Recall returns memories that do NOT contain the transcript.
	recall := func(_ context.Context, _ string, _ int) ([]map[string]any, error) {
		return []map[string]any{{"content": "Totals are usually summed."}}, nil
	}

	msgs, err := BuildScopedSpecialistContext(context.Background(), step, ScopedContextOptions{Recall: recall})
	require.NoError(t, err)
	for _, m := range msgs {
		assert.NotContains(t, m.Content, transcriptPhrase,
			"specialist window must never contain the human transcript")
	}
}

func TestBuildScopedSpecialistContext_NoRecallHookNoRecallSection(t *testing.T) {
	step := SpecialistStep{
		StepID:     "v1:harness:step:s3",
		RolePrompt: "Specialist role.",
		Title:      "Task",
		Input:      map[string]any{"x": "y"},
	}
	// Nil recall hook: context is role + input only. A missing memory
	// substrate must NOT fall back to the transcript.
	msgs, err := BuildScopedSpecialistContext(context.Background(), step, ScopedContextOptions{})
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "system", msgs[0].Role)
	assert.Equal(t, "user", msgs[1].Role)
}

func TestBuildScopedSpecialistContext_RecallTopicFallsBackToTitle(t *testing.T) {
	step := SpecialistStep{
		StepID:     "v1:harness:step:s4",
		RolePrompt: "Specialist.",
		Title:      "Title topic",
		Input:      map[string]any{"k": "v"},
	}
	var gotTopic string
	recall := func(_ context.Context, topic string, _ int) ([]map[string]any, error) {
		gotTopic = topic
		return nil, nil
	}
	_, err := BuildScopedSpecialistContext(context.Background(), step, ScopedContextOptions{Recall: recall})
	require.NoError(t, err)
	assert.Equal(t, "Title topic", gotTopic, "empty RecallTopic falls back to Title")
}

func TestBuildScopedSpecialistContext_RequiresRolePrompt(t *testing.T) {
	_, err := BuildScopedSpecialistContext(context.Background(), SpecialistStep{StepID: "s"}, ScopedContextOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no role prompt")
}

// =============================================================================
// AC3: two specialists run in parallel without cross-contamination.
// =============================================================================

func TestBuildScopedSpecialistContext_ParallelNoCrossContamination(t *testing.T) {
	stepA := SpecialistStep{
		StepID:      "v1:harness:step:A",
		RolePrompt:  "Specialist A role.",
		Title:       "task A",
		Input:       map[string]any{"only": "A-input"},
		RecallTopic: "topic-A",
	}
	stepB := SpecialistStep{
		StepID:      "v1:harness:step:B",
		RolePrompt:  "Specialist B role.",
		Title:       "task B",
		Input:       map[string]any{"only": "B-input"},
		RecallTopic: "topic-B",
	}
	// Each specialist's recall returns ONLY its own memory, keyed on topic.
	recall := func(_ context.Context, topic string, _ int) ([]map[string]any, error) {
		return []map[string]any{{"content": "memory-for-" + topic}}, nil
	}

	const iters = 50
	var wg sync.WaitGroup
	errs := make(chan error, iters*2)
	for i := 0; i < iters; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			msgs, err := BuildScopedSpecialistContext(context.Background(), stepA, ScopedContextOptions{Recall: recall})
			if err != nil {
				errs <- err
				return
			}
			joined := joinContents(msgs)
			if !strings.Contains(joined, "A-input") || strings.Contains(joined, "B-input") {
				errs <- assertErr("specialist A window cross-contaminated: " + joined)
			}
			if !strings.Contains(joined, "memory-for-topic-A") || strings.Contains(joined, "memory-for-topic-B") {
				errs <- assertErr("specialist A recall cross-contaminated: " + joined)
			}
		}()
		go func() {
			defer wg.Done()
			msgs, err := BuildScopedSpecialistContext(context.Background(), stepB, ScopedContextOptions{Recall: recall})
			if err != nil {
				errs <- err
				return
			}
			joined := joinContents(msgs)
			if !strings.Contains(joined, "B-input") || strings.Contains(joined, "A-input") {
				errs <- assertErr("specialist B window cross-contaminated: " + joined)
			}
			if !strings.Contains(joined, "memory-for-topic-B") || strings.Contains(joined, "memory-for-topic-A") {
				errs <- assertErr("specialist B recall cross-contaminated: " + joined)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// =============================================================================
// AC4: the assistant aggregates specialist observations into one answer.
// =============================================================================

func TestAggregateSpecialistObservations(t *testing.T) {
	obs := []SpecialistObservation{
		{StepID: "v1:harness:step:B", Kind: "tool_result", Content: "B did the search"},
		{StepID: "v1:harness:step:A", Kind: "note", Content: "A found prior art"},
		{StepID: "v1:harness:step:A", Kind: "decision", Content: "A chose option 2"},
		{StepID: "v1:harness:step:C", Kind: "note", Content: "   "}, // skipped (empty)
	}
	out := AggregateSpecialistObservations(obs)

	assert.Contains(t, out, "SPECIALIST RESULTS")
	assert.Contains(t, out, "A found prior art")
	assert.Contains(t, out, "A chose option 2")
	assert.Contains(t, out, "B did the search")
	assert.NotContains(t, out, "step:C", "content-less observation's step is dropped")

	// Steps are grouped + sorted: A before B.
	assert.Less(t, strings.Index(out, "Step v1:harness:step:A"),
		strings.Index(out, "Step v1:harness:step:B"))
	// Both of A's observations sit under the single A group.
	aIdx := strings.Index(out, "Step v1:harness:step:A")
	bIdx := strings.Index(out, "Step v1:harness:step:B")
	assert.Less(t, strings.Index(out, "A found prior art"), bIdx)
	assert.Less(t, strings.Index(out, "A chose option 2"), bIdx)
	assert.Greater(t, strings.Index(out, "A found prior art"), aIdx)
}

func TestAggregateSpecialistObservations_Empty(t *testing.T) {
	assert.Equal(t, "", AggregateSpecialistObservations(nil))
	assert.Equal(t, "", AggregateSpecialistObservations([]SpecialistObservation{{Content: "  "}}))
}

// --- helpers ----------------------------------------------------------------

func joinContents(msgs []common.ChatMessage) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, m.Content)
	}
	return strings.Join(parts, "\n")
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
