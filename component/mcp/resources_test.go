package mcp

// Phase 6 (#1536) protocol-level tests for the resources + prompts surface:
// resources/list, resources/read, resources/subscribe (+ live notification),
// resources/unsubscribe, prompts/list, prompts/get, and the advertised
// capabilities. A fake ResourceEngine stands in for the engine.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

type fakeResourceEngine struct {
	resources    []map[string]any
	prompts      []map[string]any
	readText     string
	getText      string
	onUpdate     func() // captured from subscribe
	unsubscribed bool
}

func (f *fakeResourceEngine) MCPConceptResources() []map[string]any { return f.resources }
func (f *fakeResourceEngine) MCPReadConceptResource(ctx context.Context, uri string) (string, error) {
	return f.readText, nil
}
func (f *fakeResourceEngine) MCPSubscribeConceptResource(uri string, onUpdate func()) (func(), error) {
	f.onUpdate = onUpdate
	return func() { f.unsubscribed = true }, nil
}
func (f *fakeResourceEngine) MCPPromptDefinitions() []map[string]any { return f.prompts }
func (f *fakeResourceEngine) MCPGetPrompt(name string, args map[string]any) (string, error) {
	return f.getText, nil
}

func resourceServer(eng *fakeResourceEngine) *Server {
	return NewServer(slog.Default(), "test", "0", eng, Config{})
}

// call drives one JSON-RPC request through handleMessage and returns the result
// map (or nil on a protocol error).
func call(t *testing.T, s *Server, method string, params map[string]any) (map[string]any, *rpcResponse) {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, _ := json.Marshal(req)
	resp := s.handleMessage(context.Background(), raw)
	if resp == nil {
		t.Fatalf("%s returned no response", method)
	}
	result, _ := resp.Result.(map[string]any)
	return result, resp
}

func TestResourcesList(t *testing.T) {
	eng := &fakeResourceEngine{resources: []map[string]any{{"uri": "memql://concept/v1:x:y", "name": "v1:x:y"}}}
	res, _ := call(t, resourceServer(eng), "resources/list", nil)
	list, _ := res["resources"].([]map[string]any)
	if len(list) != 1 || list[0]["uri"] != "memql://concept/v1:x:y" {
		t.Errorf("resources/list = %+v", res)
	}
}

func TestResourcesRead(t *testing.T) {
	eng := &fakeResourceEngine{readText: `{"rows":[{"id":"a"}]}`}
	res, _ := call(t, resourceServer(eng), "resources/read", map[string]any{"uri": "memql://concept/v1:x:y"})
	contents, _ := res["contents"].([]map[string]any)
	if len(contents) != 1 || contents[0]["text"] != eng.readText || contents[0]["uri"] != "memql://concept/v1:x:y" {
		t.Errorf("resources/read contents = %+v", res)
	}
}

func TestResourcesRead_RequiresURI(t *testing.T) {
	_, resp := call(t, resourceServer(&fakeResourceEngine{}), "resources/read", map[string]any{})
	if resp.Error == nil {
		t.Fatal("resources/read without a uri should be a protocol error")
	}
}

func TestResourcesSubscribe_NotifiesAndUnsubscribes(t *testing.T) {
	eng := &fakeResourceEngine{}
	s := resourceServer(eng)
	var buf bytes.Buffer
	s.out = &buf // the notification sink (Serve sets this in production)

	// Subscribe.
	if _, resp := call(t, s, "resources/subscribe", map[string]any{"uri": "memql://concept/v1:x:y"}); resp.Error != nil {
		t.Fatalf("subscribe error: %v", resp.Error)
	}
	if eng.onUpdate == nil {
		t.Fatal("subscribe did not register an onUpdate with the engine")
	}
	// Fire a change -> a notification lands on the output.
	eng.onUpdate()
	out := buf.String()
	if !strings.Contains(out, "notifications/resources/updated") || !strings.Contains(out, "memql://concept/v1:x:y") {
		t.Errorf("expected a resources/updated notification, got %q", out)
	}

	// Unsubscribe tears down the engine subscription.
	if _, resp := call(t, s, "resources/unsubscribe", map[string]any{"uri": "memql://concept/v1:x:y"}); resp.Error != nil {
		t.Fatalf("unsubscribe error: %v", resp.Error)
	}
	if !eng.unsubscribed {
		t.Error("unsubscribe did not tear down the engine subscription")
	}
}

func TestCloseSubscriptions(t *testing.T) {
	eng := &fakeResourceEngine{}
	s := resourceServer(eng)
	s.out = &bytes.Buffer{}
	call(t, s, "resources/subscribe", map[string]any{"uri": "memql://concept/v1:x:y"})
	s.closeSubscriptions()
	if !eng.unsubscribed {
		t.Error("closeSubscriptions should tear down active subscriptions")
	}
}

func TestPromptsList(t *testing.T) {
	eng := &fakeResourceEngine{prompts: []map[string]any{{"name": "agentReply", "description": "d"}}}
	res, _ := call(t, resourceServer(eng), "prompts/list", nil)
	list, _ := res["prompts"].([]map[string]any)
	if len(list) != 1 || list[0]["name"] != "agentReply" {
		t.Errorf("prompts/list = %+v", res)
	}
}

func TestPromptsGet(t *testing.T) {
	eng := &fakeResourceEngine{getText: "rendered prompt body"}
	res, _ := call(t, resourceServer(eng), "prompts/get", map[string]any{"name": "agentReply", "arguments": map[string]any{"x": 1}})
	msgs, _ := res["messages"].([]map[string]any)
	if len(msgs) != 1 || msgs[0]["role"] != "user" {
		t.Fatalf("prompts/get messages = %+v", res)
	}
	content, _ := msgs[0]["content"].(map[string]any)
	if content["text"] != "rendered prompt body" {
		t.Errorf("prompts/get text = %v", content["text"])
	}
}

func TestPromptsGet_RequiresName(t *testing.T) {
	_, resp := call(t, resourceServer(&fakeResourceEngine{}), "prompts/get", map[string]any{})
	if resp.Error == nil {
		t.Fatal("prompts/get without a name should be a protocol error")
	}
}

func TestInitialize_AdvertisesResourceAndPromptCapabilities(t *testing.T) {
	res, _ := call(t, resourceServer(&fakeResourceEngine{}), "initialize", nil)
	caps, _ := res["capabilities"].(map[string]any)
	resCap, _ := caps["resources"].(map[string]any)
	if resCap == nil || resCap["subscribe"] != true {
		t.Errorf("initialize should advertise resources{subscribe:true}, got %+v", caps)
	}
	if _, ok := caps["prompts"]; !ok {
		t.Errorf("initialize should advertise prompts capability, got %+v", caps)
	}
}
