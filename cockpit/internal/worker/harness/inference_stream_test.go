package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexAnswerStreamExcludesCommentaryAndDoesNotDuplicate(t *testing.T) {
	var rec recorder
	state := newCodexTurnState()
	h := &codexAppServer{turn: state, conn: &jsonrpcConn{sink: &rec}}
	event := func(method string, fields map[string]any) {
		data, _ := json.Marshal(fields)
		h.handleNotification(method, data, data)
	}
	item := func(method, id, phase, text string) {
		event(method, map[string]any{"item": map[string]any{"type": "agentMessage", "id": id, "phase": phase, "text": text}})
	}
	item(codexNotifyItemStarted, "comment", "commentary", "")
	event(codexNotifyAgentMessageDelta, map[string]any{"itemId": "comment", "delta": "Checking things"})
	item(codexNotifyItemCompleted, "comment", "commentary", "Checking things")
	item(codexNotifyItemStarted, "final", codexPhaseFinal, "")
	event(codexNotifyAgentMessageDelta, map[string]any{"itemId": "final", "delta": "Hello"})
	if rec.joined(StreamAnswer) != "Hello" {
		t.Fatalf("no public text before item completed: %q", rec.joined(StreamAnswer))
	}
	event(codexNotifyAgentMessageDelta, map[string]any{"itemId": "final", "delta": " there"})
	item(codexNotifyItemCompleted, "final", codexPhaseFinal, "Hello there")
	if rec.joined(StreamAnswer) != "Hello there" || len(rec.on(StreamAnswer)) != 2 {
		t.Fatalf("duplicated or leaked output: %v", rec.on(StreamAnswer))
	}
}

func TestInferenceOnlyHarnessUsesSmallInstructionsAndNoToolGrant(t *testing.T) {
	binary, log := fakeCodexAppServer(t, codexTurnOK)
	spec := codexSpec(t, binary)
	spec.InferenceOnly = true
	spec.Level = "fast"
	h := startCodexAppServer(t, spec)
	if _, err := h.Turn(context.Background(), "hello", nil); err != nil {
		t.Fatal(err)
	}
	params := codexCalls(t, log, "thread/start")[0]
	config, _ := params["config"].(map[string]any)
	if params["baseInstructions"] == nil || config["features.shell_tool"] != false || config["web_search"] != "disabled" {
		t.Fatalf("coding context inherited: %v", params)
	}
	argv := claudeArgv(Spec{Binary: "claude", InferenceOnly: true}, Knobs{}, "hello", "")
	joined := strings.Join(argv, "|")
	if !strings.Contains(joined, "--tools||") || strings.Contains(joined, "--allowedTools") || !strings.Contains(joined, "--include-partial-messages") {
		t.Fatalf("tools granted or streaming disabled: %v", argv)
	}
}

func TestClaudeInferenceStreamsOnlyTextDeltas(t *testing.T) {
	var rec recorder
	turn := &claudeTurn{sink: &rec, inferenceOnly: true}
	turn.route([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"private"}}}`))
	turn.route([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}}`))
	if rec.joined(StreamAnswer) != "Hello" {
		t.Fatalf("answer=%q", rec.joined(StreamAnswer))
	}
}
