package memql

// Phase 6 (#1536) engine-side resources/prompts tests: concept resource
// descriptors + uri parsing, prompt enumeration, and the event-bus-backed
// concept subscription. Pure -- no DB (the live rows read is integration).

import (
	"sync/atomic"
	"testing"
	"time"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
)

func TestMCPConceptIDFromURI(t *testing.T) {
	cases := map[string]string{
		"memql://concept/v1:cognition:space": "v1:cognition:space",
		"memql://concept/":                   "",
		"http://other/v1:x:y":                "",
		"v1:cognition:space":                 "",
	}
	for in, want := range cases {
		if got := mcpConceptIDFromURI(in); got != want {
			t.Errorf("mcpConceptIDFromURI(%q) = %q, want %q", in, got, want)
		}
	}
}

func conceptTestRegistry() *memoryNodes.MemoryRegistry {
	reg := memoryNodes.CloneDefaultRegistry()
	reg.MergeAll(map[string]*memoryNodes.Concept{
		"v1:cognition:space": {Name: "v1:cognition:space", Description: "A space."},
	})
	return reg
}

func TestMCPConceptResources(t *testing.T) {
	e := &MemQLEngine{concepts: conceptTestRegistry()}
	res := e.MCPConceptResources()
	if len(res) == 0 {
		t.Fatal("expected concept resources from the registry")
	}
	r := res[0]
	uri, _ := r["uri"].(string)
	if uri == "" || r["mimeType"] != "application/json" {
		t.Errorf("malformed resource descriptor: %+v", r)
	}
	if mcpConceptIDFromURI(uri) == "" {
		t.Errorf("resource uri %q does not parse back to a concept id", uri)
	}
}

func TestMCPReadConceptResource_Errors(t *testing.T) {
	e := &MemQLEngine{concepts: conceptTestRegistry()}
	// Not a concept uri.
	if _, err := e.MCPReadConceptResource(nil, "http://nope"); err == nil {
		t.Error("expected error for a non-concept uri")
	}
	// Unknown concept (errors before touching the DB).
	if _, err := e.MCPReadConceptResource(nil, "memql://concept/v1:nope:nope"); err == nil {
		t.Error("expected error for an unknown concept")
	}
}

func TestMCPPromptDefinitions(t *testing.T) {
	e := &MemQLEngine{prompts: newPromptRegistry()}
	e.prompts.set(&PromptTemplate{Name: "zzzPrompt", Description: "z"})
	e.prompts.set(&PromptTemplate{Name: "aaaPrompt", Description: "a"})
	defs := e.MCPPromptDefinitions()
	if len(defs) != 2 {
		t.Fatalf("expected 2 prompt definitions, got %d", len(defs))
	}
	// Sorted by name.
	if defs[0]["name"] != "aaaPrompt" || defs[1]["name"] != "zzzPrompt" {
		t.Errorf("prompts not sorted by name: %+v", defs)
	}
	if _, ok := defs[0]["arguments"]; !ok {
		t.Error("prompt definition missing 'arguments'")
	}
}

func TestMCPGetPrompt_UnknownErrors(t *testing.T) {
	e := &MemQLEngine{prompts: newPromptRegistry()}
	if _, err := e.MCPGetPrompt("nope", nil); err == nil {
		t.Error("expected error for an unknown prompt")
	}
	if _, err := e.MCPGetPrompt("", nil); err == nil {
		t.Error("expected error for an empty prompt name")
	}
}

func TestMCPSubscribeConceptResource(t *testing.T) {
	e := &MemQLEngine{}
	bus := events.NewBus()
	e.SetEventBus(bus)

	var hits int32
	unsub, err := e.MCPSubscribeConceptResource("memql://concept/v1:cognition:space", func() {
		atomic.AddInt32(&hits, 1)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if unsub == nil {
		t.Fatal("nil unsubscribe")
	}

	// A row event for the concept fires onUpdate; a different concept does not.
	bus.PublishSync(events.NewEvent("graph.node.created.v1:cognition:space", events.KindNodeCreated, nil))
	bus.PublishSync(events.NewEvent("graph.node.updated.v1:cognition:space", events.KindNodeUpdated, nil))
	bus.PublishSync(events.NewEvent("graph.node.created.v1:other:thing", events.KindNodeCreated, nil))

	// PublishSync is synchronous, but allow a beat for any async dispatch.
	time.Sleep(10 * time.Millisecond)
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("onUpdate fired %d times, want 2 (created+updated for the concept, not the other)", got)
	}

	// After unsubscribe, no more hits.
	unsub()
	bus.PublishSync(events.NewEvent("graph.node.created.v1:cognition:space", events.KindNodeCreated, nil))
	time.Sleep(10 * time.Millisecond)
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("onUpdate fired after unsubscribe (%d), want still 2", got)
	}

	// A non-concept uri is rejected.
	if _, err := e.MCPSubscribeConceptResource("http://nope", nil); err == nil {
		t.Error("expected error subscribing to a non-concept uri")
	}
}
