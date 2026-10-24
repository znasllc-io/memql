package modelcall

import (
	"context"
	"encoding/json"
	"testing"
)

// openAIToolCallStream is a recorded /chat/completions SSE stream for a
// call that offered tools, in the shape OpenAI documents and every
// compatible server copies: choices[].delta.tool_calls[], each fragment
// keyed by `index`, `arguments` a STRING assembled a few characters at a
// time.
//
// It is deliberately the AWKWARD version of that shape, because the tidy
// version proves nothing:
//
//   - TWO calls are in flight at once and their fragments INTERLEAVE, so
//     an accumulator keyed on array position or on "the call being built"
//     splices one call's arguments into the other's -- and the splice
//     produces valid JSON, so nothing downstream notices;
//   - the second call's `id` arrives LATE, on its own fragment after two
//     of its argument pieces, so an accumulator keyed on the id has
//     nothing to key on when its arguments start;
//   - the arguments are split MID-TOKEN, so a decoder that tried to parse
//     each fragment would fail on every one of them.
//
// The last content-bearing frame carries finish_reason "tool_calls" and
// the usage rides on a frame of its own with an empty choices array,
// which is what include_usage produces.
const openAIToolCallStream = `data: {"id":"chatcmpl-a1","object":"chat.completion.chunk","created":1757181851,"model":"qwen2.5-7b-instruct","choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1a","type":"function","function":{"name":"get_current_weather","arguments":""}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-a1","object":"chat.completion.chunk","created":1757181851,"model":"qwen2.5-7b-instruct","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"type":"function","function":{"name":"get_current_time","arguments":""}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-a1","object":"chat.completion.chunk","created":1757181851,"model":"qwen2.5-7b-instruct","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"loc"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-a1","object":"chat.completion.chunk","created":1757181851,"model":"qwen2.5-7b-instruct","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"time"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-a1","object":"chat.completion.chunk","created":1757181851,"model":"qwen2.5-7b-instruct","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ation\":\"Par"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-a1","object":"chat.completion.chunk","created":1757181851,"model":"qwen2.5-7b-instruct","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"zone\":\"Euro"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-a1","object":"chat.completion.chunk","created":1757181851,"model":"qwen2.5-7b-instruct","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2b"}]},"finish_reason":null}]}

data: {"id":"chatcmpl-a1","object":"chat.completion.chunk","created":1757181851,"model":"qwen2.5-7b-instruct","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"is, FR\"}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-a1","object":"chat.completion.chunk","created":1757181851,"model":"qwen2.5-7b-instruct","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"pe/Paris\"}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-a1","object":"chat.completion.chunk","created":1757181851,"model":"qwen2.5-7b-instruct","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"chatcmpl-a1","object":"chat.completion.chunk","created":1757181851,"model":"qwen2.5-7b-instruct","choices":[],"usage":{"prompt_tokens":231,"completion_tokens":48,"total_tokens":279}}

data: [DONE]

`

// openAIDoneStream is the shortest legal stream: one empty content delta,
// a clean stop, the terminator. The request-shape tests use it because
// what they assert is what went OUT.
const openAIDoneStream = `data: {"id":"chatcmpl-b2","object":"chat.completion.chunk","created":1757181851,"model":"m","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}]}

data: [DONE]

`

// TestOpenAIChat_ToolCallsAccumulateByIndex. THE INDEX IS THE KEY, and
// this is the test that says so. Two interleaved calls, a late id and
// arguments split mid-token: every one of those breaks an accumulator
// keyed on anything else, and breaks it into valid JSON that asks the
// tool for something the model never asked for.
func TestOpenAIChat_ToolCallsAccumulateByIndex(t *testing.T) {
	srv, _ := serveRaw(t, openAIToolCallStream)
	c := &openAIClient{baseURL: srv.URL, http: srv.Client()}

	res, err := c.Chat(context.Background(), ChatRequest{Model: "qwen2.5-7b-instruct"}, discardEmit)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	want := []ToolCall{
		{ID: "call_1a", Name: "get_current_weather", ArgumentsJSON: `{"location":"Paris, FR"}`},
		{ID: "call_2b", Name: "get_current_time", ArgumentsJSON: `{"timezone":"Europe/Paris"}`},
	}
	if len(res.ToolCalls) != len(want) {
		t.Fatalf("tool calls = %+v, want %d", res.ToolCalls, len(want))
	}
	for i, w := range want {
		got := res.ToolCalls[i]
		if got.ID != w.ID {
			t.Errorf("tool call %d id = %q, want %q", i, got.ID, w.ID)
		}
		if got.Name != w.Name {
			t.Errorf("tool call %d name = %q, want %q", i, got.Name, w.Name)
		}
		if got.ArgumentsJSON != w.ArgumentsJSON {
			t.Errorf("tool call %d arguments = %q, want %q", i, got.ArgumentsJSON, w.ArgumentsJSON)
		}
		// The assembled arguments are the only thing a tool dispatch
		// ever reads, so they have to parse. A splice between two calls
		// would also parse, which is why the exact-string assertion
		// above is the one that matters and this is the backstop.
		var parsed map[string]any
		if err := json.Unmarshal([]byte(got.ArgumentsJSON), &parsed); err != nil {
			t.Errorf("tool call %d arguments do not parse: %v (%q)", i, err, got.ArgumentsJSON)
		}
	}

	// "tool_calls" is a finished generation whose output happens to be a
	// call rather than prose; the envelope's closed set has no separate
	// reason for it.
	if res.FinishReason != FinishStop {
		t.Errorf("finish reason = %q, want %q", res.FinishReason, FinishStop)
	}
	if !res.Usage.Known || res.Usage.InputTokens != 231 || res.Usage.OutputTokens != 48 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if res.Usage.Model != "qwen2.5-7b-instruct" {
		t.Errorf("usage model = %q", res.Usage.Model)
	}
}

// TestOpenAIChat_TruncatedToolCallIsNotReported. A stream cut mid-
// arguments leaves half a JSON object in the accumulator. Handing that up
// as a tool call the model asked for would have the caller dispatch a
// tool with arguments the model never finished writing -- so the call
// fails and reports nothing rather than reporting a fragment.
func TestOpenAIChat_TruncatedToolCallIsNotReported(t *testing.T) {
	truncated := `data: {"id":"c","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"lookup","arguments":"{\"a\":"}}]},"finish_reason":null}]}

`
	srv, _ := serveRaw(t, truncated)
	c := &openAIClient{baseURL: srv.URL, http: srv.Client()}

	res, err := c.Chat(context.Background(), ChatRequest{Model: "m"}, discardEmit)
	if err == nil {
		t.Fatal("a stream that ended without [DONE] must fail the call")
	}
	if len(res.ToolCalls) != 0 {
		t.Errorf("a half-written tool call must not be reported: %+v", res.ToolCalls)
	}
}

// TestOpenAIChat_ToolSchemaIsForwardedVerbatim. Same rule as the Ollama
// side and for the same reason: the schema is the caller's, and a Go map
// round trip rewrites it in ways that change what the model is told.
func TestOpenAIChat_ToolSchemaIsForwardedVerbatim(t *testing.T) {
	srv, body := serveRaw(t, openAIDoneStream)
	c := &openAIClient{baseURL: srv.URL, http: srv.Client()}

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
	if sent.Tools[0].Type != "function" || sent.Tools[0].Function.Name != "lookup" {
		t.Errorf("tool = %+v", sent.Tools[0])
	}
	if got := string(sent.Tools[0].Function.Parameters); got != schema {
		t.Errorf("the schema was rewritten on the way out:\n got %s\nwant %s", got, schema)
	}
}

// TestOpenAIChat_ToolRoundTripUsesTheStringArguments. This surface takes
// `arguments` as a STRING and names the tool on a role="tool" turn
// `name` -- both the opposite of Ollama. Sending an object here is not
// rejected; it is decoded into a shape the server does not expect and the
// model is shown a call it did not make.
func TestOpenAIChat_ToolRoundTripUsesTheStringArguments(t *testing.T) {
	srv, body := serveRaw(t, openAIDoneStream)
	c := &openAIClient{baseURL: srv.URL, http: srv.Client()}

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
			Name       string `json:"name"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
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
	if call.ID != "call_1a" || call.Type != "function" || call.Function.Name != "get_current_weather" {
		t.Errorf("replayed call = %+v", call)
	}
	if call.Function.Arguments != `{"location":"Paris, FR"}` {
		t.Errorf("replayed arguments = %q, want the JSON as a string", call.Function.Arguments)
	}

	result := sent.Messages[2]
	if result.Name != "get_current_weather" {
		t.Errorf("name = %q; this surface reads the tool's name from name, not tool_name", result.Name)
	}
	if result.ToolCallID != "call_1a" {
		t.Errorf("tool_call_id = %q", result.ToolCallID)
	}
}
