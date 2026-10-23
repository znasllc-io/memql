package modelcall

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// ollamaToolCallStream is a recorded /api/chat response for a call that
// offered tools. The shape is Ollama's own -- api/types.go defines
// Message.tool_calls, ToolCall.id (omitempty) and
// ToolCallFunction{index, name, arguments}, and the API docs show the
// streamed form below.
//
// Two things in it are the whole point of the decoder:
//
//   - the tool calls arrive COMPLETE on the frame that carries them, name
//     and the entire arguments value together, rather than a few
//     characters at a time;
//   - `arguments` is a JSON OBJECT, not the string the OpenAI-compatible
//     surface sends.
//
// The `id` is absent because Ollama's native surface leaves it absent:
// the models it runs do not mint one, and the field is omitempty.
const ollamaToolCallStream = `{"model":"llama3.1:8b","created_at":"2026-09-06T18:04:11.201Z","message":{"role":"assistant","content":"Looking that up. "},"done":false}
{"model":"llama3.1:8b","created_at":"2026-09-06T18:04:11.402Z","message":{"role":"assistant","content":"","tool_calls":[{"function":{"index":0,"name":"get_current_weather","arguments":{"format":"celsius","location":"Paris, FR"}}},{"function":{"index":1,"name":"get_current_time","arguments":{"timezone":"Europe/Paris"}}}]},"done":false}
{"model":"llama3.1:8b","created_at":"2026-09-06T18:04:11.604Z","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":142,"eval_count":37}
`

// ollamaDoneFrame is the shortest legal /api/chat response: nothing
// generated, cleanly finished. The request-shape tests use it because
// what they assert is what went OUT.
const ollamaDoneFrame = `{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}
`

// serveRaw answers every request with one fixed body and hands back the
// request that asked for it. The recorded bytes are the point in half
// these tests: what a runtime is TOLD is as breakable as what it says,
// and it fails silently -- a misspelt field is dropped by the runtime's
// decoder and the model is simply never shown the tool.
func serveRaw(t *testing.T, body string) (*httptest.Server, *[]byte) {
	t.Helper()
	var seen []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func discardEmit(string) error { return nil }

func TestOllamaStreamFailurePreservesTheRuntimeCause(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"runtime tool parser", `{"error":"tool call parsing failed: XML syntax error"}`, "tool call parsing failed: XML syntax error"},
		{"context overflow", `{"error":"the input length exceeds the context length"}`, "input length exceeds the context length"},
		{"malformed frame", "{broken}\n" + ollamaDoneFrame, "invalid stream frame"},
		{"premature EOF", `{"message":{"content":"partial"}}`, "stream ended without a completion frame"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serveRaw(t, tc.body)
			m := managerFor(inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})))
			rec := newRecorder()
			m.Start(context.Background(), rec, start("r", "m", KindChat))
			end := rec.wait(t)
			if end.FinishReason != FinishError || !strings.Contains(end.Error, tc.want) {
				t.Fatalf("runtime cause lost or failure reported success: %+v", end)
			}
		})
	}
}

// The default 2048-token compute batch can evict a resident chat model even
// with an 8K embedding context. Bound that allocation without shortening input.
func TestOllamaEmbedBoundsComputeBatchWithoutShorteningInput(t *testing.T) {
	input := strings.Repeat("MemQL first-machine acceptance. ", 1360)
	srv, seen := serveRaw(t, `{"model":"qwen3-embedding:0.6b","embeddings":[[1,2]],"prompt_eval_count":8162}`)
	client := &ollamaClient{baseURL: srv.URL, http: srv.Client()}
	result, err := client.Embed(context.Background(), EmbedRequest{Model: "qwen3-embedding:0.6b", Input: []string{input}})
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Input    []string       `json:"input"`
		Truncate *bool          `json:"truncate"`
		Options  map[string]int `json:"options"`
	}
	if err := json.Unmarshal(*seen, &request); err != nil {
		t.Fatal(err)
	}
	if request.Options["num_batch"] != 512 || request.Options["num_ctx"] != 8192 {
		t.Fatalf("embedding options = %v, want batch512/context8192", request.Options)
	}
	if len(request.Input) != 1 || request.Input[0] != input || request.Truncate == nil || *request.Truncate {
		t.Fatal("embedding request must preserve the complete input and explicitly refuse truncation")
	}
	if result.Usage.InputTokens != 8162 || len(result.Embeddings) != 1 {
		t.Fatalf("embedding result lost token usage or vectors: %+v", result)
	}
}

func TestOllamaEmbedPreservesOtherModelsBatchDefaults(t *testing.T) {
	for _, model := range []string{"nomic-embed-text", "bert", "custom/qwen3-embedding:0.6b", "qwen3-embedding:8b"} {
		t.Run(model, func(t *testing.T) {
			srv, seen := serveRaw(t, `{"embeddings":[[1,2]]}`)
			client := &ollamaClient{baseURL: srv.URL, http: srv.Client()}
			if _, err := client.Embed(context.Background(), EmbedRequest{Model: model, Input: []string{strings.Repeat("document ", 1200)}}); err != nil {
				t.Fatal(err)
			}
			var request struct {
				Options map[string]int `json:"options"`
			}
			if err := json.Unmarshal(*seen, &request); err != nil {
				t.Fatal(err)
			}
			if _, present := request.Options["num_batch"]; present {
				t.Fatal("unverified embedding architectures must retain the runtime's batch default")
			}
		})
	}
}

// TestOllamaChat_ToolCallsArriveComplete. Ollama never streams a partial
// tool call, so the decoder appends whole ones and has no accumulator.
// This is the test that pins that: if Ollama ever did split a call, the
// arguments here would arrive as fragments and the assertion would name
// it rather than leaving a truncated JSON object to fail somewhere else.
func TestOllamaChat_ToolCallsArriveComplete(t *testing.T) {
	srv, _ := serveRaw(t, ollamaToolCallStream)
	c := &ollamaClient{baseURL: srv.URL, http: srv.Client()}

	var text strings.Builder
	res, err := c.Chat(context.Background(), ChatRequest{Model: "llama3.1:8b"}, func(s string) error {
		text.WriteString(s)
		return nil
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := text.String(); got != "Looking that up. " {
		t.Errorf("streamed content = %q", got)
	}

	want := []ToolCall{
		{Name: "get_current_weather", ArgumentsJSON: `{"format":"celsius","location":"Paris, FR"}`},
		{Name: "get_current_time", ArgumentsJSON: `{"timezone":"Europe/Paris"}`},
	}
	if len(res.ToolCalls) != len(want) {
		t.Fatalf("tool calls = %+v, want %d", res.ToolCalls, len(want))
	}
	for i, w := range want {
		got := res.ToolCalls[i]
		if got.Name != w.Name {
			t.Errorf("tool call %d name = %q, want %q", i, got.Name, w.Name)
		}
		if got.ArgumentsJSON != w.ArgumentsJSON {
			t.Errorf("tool call %d arguments = %q, want %q", i, got.ArgumentsJSON, w.ArgumentsJSON)
		}
		// An invented id would match nothing the model said, and the
		// engine pairs the tool result on this exact string.
		if got.ID != "" {
			t.Errorf("tool call %d id = %q; Ollama minted none and none must be invented", i, got.ID)
		}
	}
	if !res.Usage.Known || res.Usage.InputTokens != 142 || res.Usage.OutputTokens != 37 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

// TestOllamaChat_ToolSchemaIsForwardedVerbatim. The schema is the
// caller's. A round trip through a Go map would sort these keys, drop
// what encoding/json does not model and reformat the number -- and every
// one of those changes what the model is told it may call, which surfaces
// as the model calling the tool wrongly and names nothing here.
func TestOllamaChat_ToolSchemaIsForwardedVerbatim(t *testing.T) {
	srv, body := serveRaw(t, ollamaDoneFrame)
	c := &ollamaClient{baseURL: srv.URL, http: srv.Client()}

	schema := `{"type":"object","properties":{"zulu":{"type":"string"},"alpha":{"type":"number","multipleOf":0.10}},"required":["zulu","alpha"],"additionalProperties":false}`
	if _, err := c.Chat(context.Background(), ChatRequest{
		Model: "m",
		Tools: []Tool{{Name: "lookup", Description: "look something up", ParametersJSON: schema}},
	}, discardEmit); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var sent struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(*body, &sent); err != nil {
		t.Fatalf("request body: %v (%s)", err, *body)
	}
	if len(sent.Tools) != 1 {
		t.Fatalf("the tools never reached the runtime: %s", *body)
	}
	tool := sent.Tools[0]
	if tool.Type != "function" || tool.Function.Name != "lookup" || tool.Function.Description != "look something up" {
		t.Errorf("tool = %+v", tool)
	}
	if got := string(tool.Function.Parameters); got != schema {
		t.Errorf("the schema was rewritten on the way out:\n got %s\nwant %s", got, schema)
	}
}

// TestOllamaChat_ToolWithNoParametersOmitsTheKey. An empty
// json.RawMessage fails json.Marshal outright, so a tool that takes no
// arguments would otherwise turn the whole request into one that never
// leaves the machine -- and the operator would read "the runtime refused"
// for a request no runtime ever saw.
func TestOllamaChat_ToolWithNoParametersOmitsTheKey(t *testing.T) {
	srv, body := serveRaw(t, ollamaDoneFrame)
	c := &ollamaClient{baseURL: srv.URL, http: srv.Client()}

	if _, err := c.Chat(context.Background(), ChatRequest{
		Model: "m",
		Tools: []Tool{{Name: "ping"}},
	}, discardEmit); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var sent struct {
		Tools []struct {
			Function map[string]json.RawMessage `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(*body, &sent); err != nil {
		t.Fatalf("request body: %v (%s)", err, *body)
	}
	if len(sent.Tools) != 1 {
		t.Fatalf("the tool never reached the runtime: %s", *body)
	}
	if _, present := sent.Tools[0].Function["parameters"]; present {
		t.Errorf("a tool with no schema must not carry a parameters key: %s", *body)
	}
}

// TestOllamaChat_ToolRoundTripUsesOllamaSpelling. Ollama names the tool
// on a role="tool" turn `tool_name`; the OpenAI-compatible surface calls
// the same thing `name`. The disagreement is absorbed in the client so
// Message stays one shape, and getting it wrong is silent: the runtime
// drops the unknown key and the model is never told which tool answered.
func TestOllamaChat_ToolRoundTripUsesOllamaSpelling(t *testing.T) {
	srv, body := serveRaw(t, ollamaDoneFrame)
	c := &ollamaClient{baseURL: srv.URL, http: srv.Client()}

	if _, err := c.Chat(context.Background(), ChatRequest{
		Model: "m",
		Messages: []Message{
			{Role: "user", Content: "weather in Paris?"},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID: "call_1a", Name: "get_current_weather", ArgumentsJSON: `{"location":"Paris, FR"}`,
			}}},
			{Role: "tool", Name: "get_current_weather", ToolCallID: "call_1a", Content: `{"celsius":21}`},
		},
	}, discardEmit); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var sent struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolName   string `json:"tool_name"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(*body, &sent); err != nil {
		t.Fatalf("request body: %v (%s)", err, *body)
	}
	if len(sent.Messages) != 3 {
		t.Fatalf("messages = %s", *body)
	}

	assistant := sent.Messages[1]
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("the assistant turn lost its tool calls: %s", *body)
	}
	call := assistant.ToolCalls[0]
	if call.ID != "call_1a" || call.Function.Name != "get_current_weather" {
		t.Errorf("replayed call = %+v", call)
	}
	// An OBJECT, not a quoted string: a string reaches the model as a
	// blob it cannot read as arguments, and nothing rejects it.
	if got := string(call.Function.Arguments); got != `{"location":"Paris, FR"}` {
		t.Errorf("replayed arguments = %s, want the object verbatim", got)
	}

	result := sent.Messages[2]
	if result.ToolName != "get_current_weather" {
		t.Errorf("tool_name = %q; Ollama reads the tool's name from tool_name, not name", result.ToolName)
	}
	if result.ToolCallID != "call_1a" {
		t.Errorf("tool_call_id = %q", result.ToolCallID)
	}
	if result.Content != `{"celsius":21}` {
		t.Errorf("tool content = %q", result.Content)
	}
}

// An estimate is not a tokenizer. The runtime must reject a prompt it cannot
// fit, not return a successful answer or embedding after silently cutting it.
func TestOllamaContextOverflowIsAWorkerError(t *testing.T) {
	for _, kind := range []string{KindChat, KindEmbedding} {
		t.Run(kind, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				// Ollama defaults truncation on. Model its documented response
				// when input exceeds num_ctx with/without explicit refusal.
				if truncate, present := body["truncate"]; present && truncate == false {
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"error":"the input length exceeds the context length"}`)
					return
				}
				if kind == KindEmbedding {
					io.WriteString(w, `{"embeddings":[[1,2]],"prompt_eval_count":8192}`)
				} else {
					io.WriteString(w, ollamaDoneFrame)
				}
			}))
			defer srv.Close()
			m := managerFor(inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{
				ContextWindow: 65536, Embeddings: true, MaxConcurrent: 1,
			})))
			rec := newRecorder()
			request := start("r", "m", kind)
			request.Params = &memqlv1.ModelCallParams{ContextTokens: 32768}
			request.EmbeddingInput = []string{strings.Repeat("long ", 10000)}
			m.Start(context.Background(), rec, request)
			end := rec.wait(t)
			if !strings.Contains(end.Error, "input length exceeds the context length") || end.FinishReason != FinishError {
				t.Errorf("over-context input returned a successful prefix result: %+v", end)
			}
			if len(end.Embeddings) != 0 {
				t.Error("truncated embedding escaped the runtime refusal")
			}
			if body["truncate"] != false {
				t.Errorf("truncate=%v, want explicit false", body["truncate"])
			}
			options, _ := body["options"].(map[string]any)
			if kind == KindChat {
				if body["shift"] != false {
					t.Errorf("shift=%v, want explicit false", body["shift"])
				}
				if options["num_ctx"] != float64(32768) {
					t.Errorf("chat context=%v", options["num_ctx"])
				}
			} else if options["num_ctx"] != float64(8192) {
				t.Errorf("embedding working context=%v, want 8192", options["num_ctx"])
			}
		})
	}
}
