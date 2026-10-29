package modelcall

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A structured call on a runner that does not implement `format`
// (memql-cockpit#551): Ollama 0.40.6 answers qwen3.8:27b (NVFP4) with 501
// for any request carrying `format`. The worker asks again with the schema in
// the prompt, and passes on only an answer that validates.

const reviewSchema = `{"type":"object","additionalProperties":false,"required":["reply"],"properties":{"reply":{"type":"string","minLength":1,"maxLength":24000}}}`

// noFormatRunner is an Ollama whose runner refuses `format` the way 0.40.6
// does, and otherwise answers with `answer` as the model's text. It keeps
// every request body it was sent.
type noFormatRunner struct {
	mu       sync.Mutex
	requests []map[string]any
	answer   string
	status   int    // what a request with `format` gets; 501 unless set
	refusal  string // the body that refusal carries; the 501's unless set
}

func (r *noFormatRunner) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body: %v", err)
		}
		r.mu.Lock()
		r.requests = append(r.requests, body)
		r.mu.Unlock()
		if _, ok := body["format"]; ok {
			status := r.status
			if status == 0 {
				status = http.StatusNotImplemented
			}
			w.WriteHeader(status)
			refusal := r.refusal
			if refusal == "" {
				refusal = `{"error":"structured output is unavailable"}`
			}
			fmt.Fprint(w, refusal)
			return
		}
		content, _ := json.Marshal(r.answer)
		fmt.Fprintf(w, "{\"model\":\"qwen3.8:27b\",\"message\":{\"role\":\"assistant\",\"content\":%s},\"done\":false}\n", content)
		fmt.Fprint(w, `{"model":"qwen3.8:27b","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":12,"eval_count":7}`+"\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *noFormatRunner) sent() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.requests...)
}

func structuredCall(t *testing.T, runner *noFormatRunner, req ChatRequest) (string, int, error) {
	t.Helper()
	srv := runner.serve(t)
	c := &ollamaClient{baseURL: srv.URL, http: srv.Client()}
	var emitted []string
	_, err := c.Chat(context.Background(), req, func(delta string) error {
		emitted = append(emitted, delta)
		return nil
	})
	return strings.Join(emitted, ""), len(emitted), err
}

func reviewRequest() ChatRequest {
	return ChatRequest{
		Model:    "qwen3.8:27b",
		Messages: []Message{{Role: "system", Content: "You review a draft reply."}, {Role: "user", Content: "Review it."}},
		Schema:   []byte(reviewSchema),
		Level:    "strong",
	}
}

func TestStructuredCallOnARunnerWithoutFormatAsksInThePrompt(t *testing.T) {
	runner := &noFormatRunner{answer: `{"reply":"Here is what I can help with."}`}
	got, emits, err := structuredCall(t, runner, reviewRequest())
	if err != nil {
		t.Fatalf("a schema-valid answer must be returned: %v", err)
	}
	if got != `{"reply":"Here is what I can help with."}` || emits != 1 {
		t.Fatalf("the validated JSON must go out once, whole: %q in %d emits", got, emits)
	}
	sent := runner.sent()
	if len(sent) != 2 {
		t.Fatalf("one request with format, then one without: %d requests", len(sent))
	}
	if _, ok := sent[1]["format"]; ok {
		t.Fatal("the second request must not send format again")
	}
	messages, _ := sent[1]["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("the schema joins the existing system message rather than adding one: %d messages", len(messages))
	}
	system, _ := messages[0].(map[string]any)
	content, _ := system["content"].(string)
	if system["role"] != "system" || !strings.HasPrefix(content, "You review a draft reply.") ||
		!strings.Contains(content, schemaInstruction) || !strings.Contains(content, reviewSchema) {
		t.Fatalf("the system message must carry the instruction and the schema verbatim: %q", content)
	}
	if sent[1]["think"] != false {
		t.Error("a structured call keeps thinking off, as it did with format")
	}
}

// grammarRefusal is what Ollama 0.33.3 answered on two machines for a
// structured call whose schema its llama.cpp could not compile into a
// sampling grammar (2026-10-28): llama-server's 400, wrapped by Ollama.
const grammarRefusal = `{"error":"{\"error\":{\"code\":400,\"message\":\"Failed to initialize samplers: failed to parse grammar\",\"type\":\"invalid_request_error\"}}"}`

func TestStructuredCallTheRuntimeCannotCompileAsksInThePrompt(t *testing.T) {
	runner := &noFormatRunner{answer: `{"reply":"ok"}`, status: http.StatusBadRequest, refusal: grammarRefusal}
	got, emits, err := structuredCall(t, runner, reviewRequest())
	if err != nil {
		t.Fatalf("a schema the runtime cannot compile must be asked for in the prompt: %v", err)
	}
	if got != `{"reply":"ok"}` || emits != 1 {
		t.Fatalf("the validated JSON must go out once, whole: %q in %d emits", got, emits)
	}
	sent := runner.sent()
	if len(sent) != 2 {
		t.Fatalf("one request with format, then one without: %d requests", len(sent))
	}
	if _, ok := sent[1]["format"]; ok {
		t.Fatal("the second request must not send format again")
	}
	messages, _ := sent[1]["messages"].([]any)
	system, _ := messages[0].(map[string]any)
	if content, _ := system["content"].(string); !strings.Contains(content, schemaInstruction) {
		t.Fatalf("the schema must reach the model in the system message: %q", content)
	}
}

func TestStructuredCallKeepsOtherBadRequests(t *testing.T) {
	// A 400 that is not about the grammar -- the model cannot chat at all --
	// would fail the same way without format, so it is passed on as it was.
	runner := &noFormatRunner{answer: `{"reply":"ok"}`, status: http.StatusBadRequest, refusal: `{"error":"\"qwen3-embedding:0.6b\" does not support chat"}`}
	if _, _, err := structuredCall(t, runner, reviewRequest()); err == nil || !strings.Contains(err.Error(), "returned 400") {
		t.Fatalf("a 400 that names no grammar must fail as it did: %v", err)
	}
	if len(runner.sent()) != 1 {
		t.Fatal("only a refusal of the schema itself is asked again")
	}
}

func TestStructuredCallFindsTheJSONInAFenceOrAroundProse(t *testing.T) {
	for name, answer := range map[string]string{
		"fenced":      "```json\n{\"reply\":\"ok\"}\n```",
		"after prose": "Here you go: {\"reply\":\"ok\"} -- done.",
	} {
		t.Run(name, func(t *testing.T) {
			got, _, err := structuredCall(t, &noFormatRunner{answer: answer}, reviewRequest())
			if err != nil || got != `{"reply":"ok"}` {
				t.Fatalf("only the JSON value is passed on: %q, %v", got, err)
			}
		})
	}
}

func TestStructuredCallFailsWhenTheAnswerDoesNotMatchTheSchema(t *testing.T) {
	for name, answer := range map[string]string{
		"prose":           "I can help with lots of things.",
		"missing field":   `{"answer":"ok"}`,
		"extra field":     `{"reply":"ok","note":"x"}`,
		"too short":       `{"reply":""}`,
		"two values":      `{"reply":"a"} {"reply":"b"}`,
		"wrong json type": `["ok"]`,
	} {
		t.Run(name, func(t *testing.T) {
			got, emits, err := structuredCall(t, &noFormatRunner{answer: answer}, reviewRequest())
			if err == nil || !strings.Contains(err.Error(), "does not match the requested schema") {
				t.Fatalf("an answer the schema refuses must fail the call: %q, %v", got, err)
			}
			if emits != 0 {
				t.Fatalf("nothing may reach the engine from a refused answer: %q", got)
			}
		})
	}
}

func TestStructuredCallKeepsOtherRefusals(t *testing.T) {
	// A failure that is not the runner refusing `format` is the runtime's
	// answer, and is passed on as before.
	runner := &noFormatRunner{answer: `{"reply":"ok"}`, status: http.StatusInternalServerError}
	if _, _, err := structuredCall(t, runner, reviewRequest()); err == nil || !strings.Contains(err.Error(), "returned 500") {
		t.Fatalf("a 500 must fail as it did: %v", err)
	}
	if len(runner.sent()) != 1 {
		t.Fatal("only a refusal of the schema itself is asked again, never a 500")
	}
	// With tools offered the turn may answer with a call instead of JSON,
	// so it is not asked again either.
	withTools := reviewRequest()
	withTools.Tools = []Tool{{Name: "lookup", ParametersJSON: `{"type":"object"}`}}
	runner = &noFormatRunner{answer: `{"reply":"ok"}`}
	if _, _, err := structuredCall(t, runner, withTools); err == nil || !strings.Contains(err.Error(), "returned 501") {
		t.Fatalf("a tool turn keeps the 501: %v", err)
	}
}

func TestStructuredCallWithNoSystemMessageGainsOne(t *testing.T) {
	runner := &noFormatRunner{answer: `{"reply":"ok"}`}
	req := reviewRequest()
	req.Messages = []Message{{Role: "user", Content: "Review it."}}
	if _, _, err := structuredCall(t, runner, req); err != nil {
		t.Fatal(err)
	}
	messages, _ := runner.sent()[1]["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	if len(messages) != 2 || first["role"] != "system" || !strings.Contains(first["content"].(string), reviewSchema) {
		t.Fatalf("the schema goes in a system message ahead of the turns: %v", messages)
	}
	if len(req.Messages) != 1 {
		t.Fatal("the caller's messages must not be changed")
	}
}
