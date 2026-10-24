package modelcall

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// ollama_think_test.go pins when this machine turns a model's thinking off.
//
// Measured on 2026-09-28 against qwen3.5:4b on Ollama 0.33.3, classifying
// "hi there" with a JSON schema: left to its default the model spent 1,482
// tokens (39.1 s) thinking before a 216-character answer; with think:false it
// gave the same answer in 41 tokens (1.2 s). Thinking is hidden from the
// caller and counts against the output budget, so on a structured call it
// also produced EMPTY answers once the budget ran out -- which the engine
// reported as "materializer: invalid draft: EOF".

// thinkSent decodes what the runtime was told about thinking: nil when the
// key was never sent.
func thinkSent(t *testing.T, body []byte) *bool {
	t.Helper()
	var sent struct {
		Think *bool `json:"think"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("request body: %v (%s)", err, body)
	}
	return sent.Think
}

func TestOllamaChat_AStructuredCallTurnsThinkingOff(t *testing.T) {
	srv, body := serveRaw(t, ollamaDoneFrame)
	c := &ollamaClient{baseURL: srv.URL, http: srv.Client()}

	if _, err := c.Chat(context.Background(), ChatRequest{
		Model:  "m",
		Level:  "strong",
		Schema: []byte(`{"type":"object"}`),
	}, discardEmit); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := thinkSent(t, *body); got == nil || *got {
		t.Errorf("think = %v, want false: a structured answer is parsed from the content, and hidden thinking only spends its budget", got)
	}
}

func TestOllamaChat_AFastCallTurnsThinkingOff(t *testing.T) {
	srv, body := serveRaw(t, ollamaDoneFrame)
	c := &ollamaClient{baseURL: srv.URL, http: srv.Client()}

	if _, err := c.Chat(context.Background(), ChatRequest{Model: "m", Level: "fast"}, discardEmit); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := thinkSent(t, *body); got == nil || *got {
		t.Errorf("think = %v, want false: a fast call must not spend minutes thinking", got)
	}
}

func TestOllamaChat_FreeTextAboveFastLeavesThinkingToTheModel(t *testing.T) {
	for _, level := range []string{"", "strong", "reasoning"} {
		srv, body := serveRaw(t, ollamaDoneFrame)
		c := &ollamaClient{baseURL: srv.URL, http: srv.Client()}

		if _, err := c.Chat(context.Background(), ChatRequest{Model: "m", Level: level}, discardEmit); err != nil {
			t.Fatalf("level %q: Chat: %v", level, err)
		}
		if got := thinkSent(t, *body); got != nil {
			t.Errorf("level %q: think = %v, want the key absent: the model's own default stands", level, *got)
		}
	}
}

// A runtime that ignores think:false, or a model that thinks anyway, must
// not hand back an empty structured answer as a clean stop.
func TestOllamaChat_AStructuredAnswerThatIsOnlyThinkingIsAnError(t *testing.T) {
	stream := `{"model":"m","message":{"role":"assistant","content":"","thinking":"Let me consider the goal..."},"done":false}
{"model":"m","message":{"role":"assistant","content":"","thinking":" it is a greeting."},"done":false}
{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"length","prompt_eval_count":90,"eval_count":2048}
`
	srv, _ := serveRaw(t, stream)
	c := &ollamaClient{baseURL: srv.URL, http: srv.Client()}

	_, err := c.Chat(context.Background(), ChatRequest{Model: "m", Schema: []byte(`{"type":"object"}`)}, discardEmit)
	if err == nil {
		t.Fatal("an answer that was only thinking finished as a clean stop")
	}
	for _, want := range []string{"thinking", "no answer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
}
