package modelcall

import (
	"context"
	"encoding/json"
	"reflect"
	"runtime"
	"testing"
)

func TestOllamaDecodingPolicyFollowsLocalHardware(t *testing.T) {
	params := Params{TemperatureSet: true, Temperature: 0.4, ContextTokens: 32768, MaxOutputTokens: 256}
	for _, tc := range []struct {
		name, hostOS, endpoint string
		disableDraft           bool
	}{
		{"local Mac", "darwin", "http://127.0.0.1:11434", true},
		{"IPv6 Mac", "darwin", "http://[::1]:11434", true},
		{"localhost Mac", "darwin", "http://localhost:11434", true},
		{"remote from Mac", "darwin", "https://inference.example.com", false},
		{"Linux GPU", "linux", "http://127.0.0.1:11434", false},
		{"Windows GPU", "windows", "http://127.0.0.1:11434", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ollamaChatOptions(params, tc.endpoint, tc.hostOS)
			value, present := got["draft_num_predict"]
			if present != tc.disableDraft || present && value != 0 {
				t.Fatalf("draft option = %v, present %t", value, present)
			}
			delete(got, "draft_num_predict")
			if !reflect.DeepEqual(got, ollamaOptions(params)) {
				t.Fatalf("decoding policy changed caller generation parameters: %v", got)
			}
		})
	}
}

func TestOllamaChatSendsDecodingPolicyWithoutChangingModelOrTools(t *testing.T) {
	srv, seen := serveRaw(t, ollamaDoneFrame)
	c := &ollamaClient{baseURL: srv.URL, http: srv.Client()}
	req := ChatRequest{Model: "arbitrary-model", Level: "reasoning", Tools: []Tool{{Name: "example", Description: "Example operation"}}}
	if _, err := c.Chat(context.Background(), req, discardEmit); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(*seen, &body); err != nil {
		t.Fatal(err)
	}
	options, _ := body["options"].(map[string]any)
	draft, present := options["draft_num_predict"]
	if present != (runtime.GOOS == "darwin") || present && draft != float64(0) {
		t.Fatalf("wire draft option = %v, present %t", draft, present)
	}
	if body["model"] != req.Model || body["think"] != nil || len(body["tools"].([]any)) != 1 {
		t.Fatal("decoding policy must preserve the requested model, reasoning and tools")
	}
}
