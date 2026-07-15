package memql

import "testing"

// Item 6: PromptNames enumerates the prompt registry via List() (the old
// adapter returned nil with a "no Names()" note, now stale).
func TestSenseAdapter_PromptNames(t *testing.T) {
	reg := newPromptRegistry()
	reg.set(&PromptTemplate{Name: "agentReply"})
	reg.set(&PromptTemplate{Name: "conductorTurn"})
	adapter := NewSenseAdapter(&MemQLEngine{prompts: reg})

	set := make(map[string]bool)
	for _, n := range adapter.PromptNames() {
		set[n] = true
	}
	if !set["agentReply"] || !set["conductorTurn"] {
		t.Errorf("PromptNames = %v; want agentReply + conductorTurn", adapter.PromptNames())
	}

	if names := NewSenseAdapter(&MemQLEngine{}).PromptNames(); names != nil {
		t.Errorf("PromptNames with a nil registry = %v; want nil", names)
	}
}

// Item 1: senseArgsFromSchema projects a function's parsed args schema onto the
// flat arg list Sense uses for signature help (Optional -> Required inverted).
func TestSenseArgsFromSchema(t *testing.T) {
	got := senseArgsFromSchema(&ArgsSchemaConfig{Fields: []*FunctionArgsField{
		{Name: "id", Type: "string", Optional: false},
		{Name: "limit", Type: "number", Optional: true},
		nil, // tolerated
	}})
	if len(got) != 2 {
		t.Fatalf("expected 2 args (nil skipped), got %d", len(got))
	}
	if got[0].Name != "id" || got[0].Type != "string" || !got[0].Required {
		t.Errorf("arg 0 = %+v; want {id string required}", got[0])
	}
	if got[1].Name != "limit" || got[1].Required {
		t.Errorf("arg 1 = %+v; want optional 'limit'", got[1])
	}
	if senseArgsFromSchema(nil) != nil {
		t.Error("nil schema should yield nil args")
	}
}
