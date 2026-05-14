package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/visionarys-io/memql/component/memql"
	"github.com/visionarys-io/memql/core/common"
	"github.com/visionarys-io/memql/integrations"
)

// fakeStreamProvider satisfies common.ChatStreamWithToolsProvider. Each call
// consumes one entry from `turns`; the test can assert against recorded
// `calls` to inspect what messages were sent.
type fakeStreamProvider struct {
	mu    sync.Mutex
	turns [][]common.StreamToolChunk
	calls [][]common.ChatMessage
	err   error
}

func (f *fakeStreamProvider) CallChatStreamWithTools(
	ctx context.Context,
	messages []common.ChatMessage,
	tools []common.ToolDefinition,
) (<-chan common.StreamToolChunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	cp := make([]common.ChatMessage, len(messages))
	copy(cp, messages)
	f.calls = append(f.calls, cp)

	if f.err != nil {
		return nil, f.err
	}
	idx := len(f.calls) - 1
	if idx >= len(f.turns) {
		return nil, fmt.Errorf("fakeStreamProvider: no turn at index %d", idx)
	}

	chunks := f.turns[idx]
	out := make(chan common.StreamToolChunk, len(chunks)+1)
	for _, ch := range chunks {
		out <- ch
	}
	close(out)
	return out, nil
}

// fakeEngine is a minimal memql-engine stub satisfying cognition.MemQLEngine.
// Only Execute and ExecuteToolByName have behaviour; other methods return
// zero values.
type fakeEngine struct {
	mu             sync.Mutex
	executeQueries []string
	toolResults    map[string]string
	toolErrors     map[string]error
	toolCalls      []fakeToolInvocation
}

type fakeToolInvocation struct {
	Name string
	Args map[string]any
}

func (e *fakeEngine) Execute(ctx context.Context, query string) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.executeQueries = append(e.executeQueries, query)
	return nil, nil
}

func (e *fakeEngine) ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolCalls = append(e.toolCalls, fakeToolInvocation{Name: name, Args: args})
	if err, ok := e.toolErrors[name]; ok {
		return "", err
	}
	if result, ok := e.toolResults[name]; ok {
		return result, nil
	}
	return "", fmt.Errorf("fakeEngine: no fixture for tool %q", name)
}

func (e *fakeEngine) InvokeSI(ctx context.Context, templateId string, data map[string]any) (any, error) {
	return nil, nil
}
func (e *fakeEngine) InvokeSIStructured(
	ctx context.Context,
	templateId string,
	data map[string]any,
	schemaName string,
	schema json.RawMessage,
	strict bool,
) (string, error) {
	return "", nil
}
func (e *fakeEngine) RenderPrompt(templateId string, data map[string]any) (string, error) {
	return "", nil
}
func (e *fakeEngine) RegisterIntegration(p memql.IntegrationProvider) error { return nil }
func (e *fakeEngine) ChatStreamProvider() common.ChatStreamProvider         { return nil }
func (e *fakeEngine) ChatStreamProviderByName(string) common.ChatStreamProvider {
	return nil
}
func (e *fakeEngine) ChatStreamWithToolsProviderByName(string) common.ChatStreamWithToolsProvider {
	return nil
}
func (e *fakeEngine) ToolDefinitionsForNames([]string) []common.ToolDefinition { return nil }

// newTestCognition returns a minimal CognitionIntegration sufficient for the
// streaming tool loop. Logger sinks to io.Discard.
func newTestCognition(engine *fakeEngine) *CognitionIntegration {
	return &CognitionIntegration{
		Integration: &integrations.Integration{
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		engine: engine,
	}
}

// textChunk is a stream chunk carrying text.
func textChunk(s string) common.StreamToolChunk {
	return common.StreamToolChunk{Content: s}
}

// toolCallChunk is a stream chunk carrying a tool-call delta. The test helper
// emits the entire ID/Name/Arguments on a single delta for simplicity.
func toolCallChunk(index int, id, name, args string) common.StreamToolChunk {
	return common.StreamToolChunk{
		ToolCalls: []common.ToolCallDelta{{
			Index:     index,
			ID:        id,
			Name:      name,
			Arguments: args,
		}},
	}
}

func TestStreamingToolLoop_NoToolCalls(t *testing.T) {
	engine := &fakeEngine{}
	c := newTestCognition(engine)

	// One turn, text-only.
	stream := &fakeStreamProvider{
		turns: [][]common.StreamToolChunk{
			{
				textChunk("Hello, "),
				textChunk("world. How can I help?"),
			},
		},
	}

	result, err := c.runStreamingToolLoop(
		context.Background(), stream,
		[]common.ChatMessage{{Role: "system", Content: "sys"}},
		nil,
		"space-1", "participant-1", "reply-test",
		"test",
	)

	if err != nil {
		t.Fatalf("runStreamingToolLoop: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("runStreamingToolLoop: nil result")
	}
	if got, want := result.Text, "Hello, world. How can I help?"; got != want {
		t.Errorf("Text = %q, want %q", got, want)
	}
	if len(result.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %d entries, want 0", len(result.ToolCalls))
	}
	if len(stream.calls) != 1 {
		t.Errorf("stream called %d times, want 1", len(stream.calls))
	}
	if len(engine.toolCalls) != 0 {
		t.Errorf("engine.toolCalls = %d, want 0", len(engine.toolCalls))
	}
	// Exactly one "done" emit should be recorded.
	if dones := countDoneEmits(engine.executeQueries); dones != 1 {
		t.Errorf("done-emits = %d, want 1", dones)
	}
}

func TestStreamingToolLoop_ToolCallContinuation(t *testing.T) {
	engine := &fakeEngine{
		toolResults: map[string]string{
			"searchUsers": `{"users":[{"id":"u1","name":"Vale"}]}`,
		},
	}
	c := newTestCognition(engine)

	stream := &fakeStreamProvider{
		turns: [][]common.StreamToolChunk{
			// Turn 1: acknowledgment text + a tool call.
			{
				textChunk("Let me look into that."),
				toolCallChunk(0, "call_abc", "searchUsers", `{"query":"Vale"}`),
			},
			// Turn 2: continuation using the tool result.
			{
				textChunk(" Found Vale — "),
				textChunk("they're on your team."),
			},
		},
	}

	initialMessages := []common.ChatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "Who is @Vale?"},
	}

	result, err := c.runStreamingToolLoop(
		context.Background(), stream,
		initialMessages, nil,
		"space-1", "participant-1", "reply-test", "test",
	)

	if err != nil {
		t.Fatalf("runStreamingToolLoop: unexpected error: %v", err)
	}

	// Text from both turns should be concatenated in the final result.
	wantSubstr := []string{
		"Let me look into that.",
		"Found Vale",
		"they're on your team.",
	}
	for _, s := range wantSubstr {
		if !strings.Contains(result.Text, s) {
			t.Errorf("result.Text = %q, missing %q", result.Text, s)
		}
	}

	// Tool should have been executed exactly once with the decoded args.
	if len(engine.toolCalls) != 1 {
		t.Fatalf("engine.toolCalls = %d, want 1", len(engine.toolCalls))
	}
	call := engine.toolCalls[0]
	if call.Name != "searchUsers" {
		t.Errorf("tool name = %q, want searchUsers", call.Name)
	}
	if got, want := call.Args["query"], "Vale"; got != want {
		t.Errorf("tool args[query] = %v, want %v", got, want)
	}

	// Provider should have been called twice.
	if len(stream.calls) != 2 {
		t.Fatalf("stream calls = %d, want 2", len(stream.calls))
	}

	// Second call must include the tool result as a Role="tool" message
	// with the right ToolCallId so the provider can thread it.
	secondCallMessages := stream.calls[1]
	var found bool
	for _, msg := range secondCallMessages {
		if msg.Role == "tool" && msg.ToolCallId == "call_abc" &&
			msg.Name == "searchUsers" &&
			strings.Contains(msg.Content, "Vale") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("second stream call missing tool-result message; got messages: %+v", secondCallMessages)
	}

	// Second call should also include the assistant message from turn 1
	// with its ToolCalls preserved (providers need this to validate the
	// tool-result that follows).
	var foundAssistant bool
	for _, msg := range secondCallMessages {
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 && msg.ToolCalls[0].ID == "call_abc" {
			foundAssistant = true
			break
		}
	}
	if !foundAssistant {
		t.Errorf("second stream call missing assistant message with tool_calls; got: %+v", secondCallMessages)
	}

	// Text chunk indices across the two turns should be monotonic
	// (first turn emits index 0; second turn continues from 1+).
	indices := collectChunkIndices(engine.executeQueries)
	if len(indices) < 2 {
		t.Fatalf("expected at least 2 text chunk emits, got %d", len(indices))
	}
	for i := 1; i < len(indices); i++ {
		if indices[i] < indices[i-1] {
			t.Errorf("chunk indices not monotonic: %v", indices)
			break
		}
	}
}

func TestStreamingToolLoop_ToolErrorEndsCleanly(t *testing.T) {
	engine := &fakeEngine{
		toolErrors: map[string]error{
			"brokenTool": fmt.Errorf("tool exploded"),
		},
	}
	c := newTestCognition(engine)

	stream := &fakeStreamProvider{
		turns: [][]common.StreamToolChunk{
			// Turn 1: acknowledgment + a tool call that will error.
			{
				textChunk("One moment."),
				toolCallChunk(0, "call_x", "brokenTool", `{}`),
			},
			// No turn 2 configured — if the loop incorrectly retries the
			// model, the provider returns an error and the test fails.
		},
	}

	result, err := c.runStreamingToolLoop(
		context.Background(), stream,
		[]common.ChatMessage{{Role: "system", Content: "sys"}},
		nil,
		"space-1", "participant-1", "reply-test", "test",
	)

	if err != nil {
		t.Fatalf("runStreamingToolLoop: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("runStreamingToolLoop: nil result")
	}
	// We should have the text from the first turn.
	if !strings.Contains(result.Text, "One moment.") {
		t.Errorf("result.Text = %q, missing first-turn text", result.Text)
	}
	// Stream should have been called exactly once — no retry loop.
	if len(stream.calls) != 1 {
		t.Errorf("stream calls = %d, want 1 (error-only tools must not retry)", len(stream.calls))
	}
	// Done signal must still fire.
	if dones := countDoneEmits(engine.executeQueries); dones != 1 {
		t.Errorf("done-emits = %d, want 1", dones)
	}
}

// countDoneEmits counts the mutationEmitTextChunk calls with done=true.
func countDoneEmits(queries []string) int {
	n := 0
	for _, q := range queries {
		if strings.Contains(q, `"done": true`) {
			n++
		}
	}
	return n
}

// collectChunkIndices extracts the integer "index": N from each emit query in
// order.
func collectChunkIndices(queries []string) []int {
	var out []int
	for _, q := range queries {
		idx := strings.Index(q, `"index":`)
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(q[idx+len(`"index":`):])
		// Read the integer up to the next comma or newline.
		end := 0
		for end < len(rest) && (rest[end] == '-' || (rest[end] >= '0' && rest[end] <= '9')) {
			end++
		}
		if end == 0 {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(rest[:end], "%d", &n); err == nil {
			out = append(out, n)
		}
	}
	return out
}
