package common

import (
	"context"
	"encoding/json"
	"testing"
)

// modelcall_test.go -- the two properties the replay key must have, and the
// one the run context must.

func baseRequest() ModelRequest {
	return ModelRequest{
		Provider: "chat54Mini",
		Model:    "gpt-5.4-mini",
		Settings: map[string]any{"temperature": 0.2, "maxTokens": 1024},
		Messages: []ChatMessage{
			{Role: "system", Content: "You are terse."},
			{Role: "user", Content: "Summarize this."},
		},
		Tools:  []ToolDefinition{{Name: "search", Description: "find", InputSchema: map[string]any{"q": "string"}}},
		Schema: StructuredSchema{Name: "summary", Schema: json.RawMessage(`{"type":"object"}`), Strict: true},
	}
}

// PROPERTY 1: the same request hashes the same, every time and in any map
// iteration order. Without it every replay is a miss and the journal is dead
// weight.
func TestHashIsStableAcrossRepeatedCalls(t *testing.T) {
	want := baseRequest().Hash()
	for i := 0; i < 64; i++ {
		if got := baseRequest().Hash(); got != want {
			t.Fatalf("iteration %d hashed differently:\n  %s\n  %s", i, got, want)
		}
	}
}

// Tool ORDER is not part of the request -- the set a model is offered is a set.
func TestHashIgnoresToolOrder(t *testing.T) {
	a := baseRequest()
	a.Tools = []ToolDefinition{{Name: "alpha"}, {Name: "beta"}}
	b := baseRequest()
	b.Tools = []ToolDefinition{{Name: "beta"}, {Name: "alpha"}}
	if a.Hash() != b.Hash() {
		t.Error("the same tool set in a different order hashed differently")
	}
}

// PROPERTY 2, and it is the one that matters: anything that changes the ANSWER
// must change the key. A journal that serves a recorded response to a request
// that has moved on is worse than no journal, because it is silent.
func TestEveryRequestFieldChangesTheHash(t *testing.T) {
	base := baseRequest().Hash()
	cases := map[string]func(*ModelRequest){
		"provider":        func(r *ModelRequest) { r.Provider = "chat54Pro" },
		"model":           func(r *ModelRequest) { r.Model = "gpt-5.4" },
		"a setting value": func(r *ModelRequest) { r.Settings["temperature"] = 0.9 },
		"a new setting":   func(r *ModelRequest) { r.Settings["topP"] = 0.5 },
		"a message role":  func(r *ModelRequest) { r.Messages[0].Role = "user" },
		"a message body":  func(r *ModelRequest) { r.Messages[1].Content = "Summarize that." },
		"a message added": func(r *ModelRequest) { r.Messages = append(r.Messages, ChatMessage{Role: "user", Content: "more"}) },
		"a tool name":     func(r *ModelRequest) { r.Tools[0].Name = "lookup" },
		"a tool schema":   func(r *ModelRequest) { r.Tools[0].InputSchema = map[string]any{"q": "number"} },
		"a tool removed":  func(r *ModelRequest) { r.Tools = nil },
		"the schema name": func(r *ModelRequest) { r.Schema.Name = "digest" },
		"the schema body": func(r *ModelRequest) { r.Schema.Schema = json.RawMessage(`{"type":"array"}`) },
		"the strict flag": func(r *ModelRequest) { r.Schema.Strict = false },
		"a tool call id":  func(r *ModelRequest) { r.Messages[0].ToolCalls = []ToolCall{{ID: "1", Name: "n", Arguments: "{}"}} },
		"a tool call's arg": func(r *ModelRequest) {
			r.Messages[0].ToolCalls = []ToolCall{{ID: "1", Name: "n", Arguments: `{"a":1}`}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := baseRequest()
			mutate(&r)
			if r.Hash() == base {
				t.Errorf("changing %s did not change the replay key -- a replay would serve the old answer", name)
			}
		})
	}
}

// The separator case: without one, a field boundary can move without changing
// the bytes being hashed.
func TestHashSeparatesAdjacentFields(t *testing.T) {
	a := ModelRequest{Provider: "ab", Model: "c"}
	b := ModelRequest{Provider: "a", Model: "bc"}
	if a.Hash() == b.Hash() {
		t.Error("two requests whose adjacent fields concatenate alike hashed the same")
	}
}

// A request carrying an unencodable setting must still hash, and must not
// collide with one carrying a different unencodable setting -- skipping it
// would make two different requests hash alike.
func TestHashSurvivesAnUnencodableSetting(t *testing.T) {
	a := baseRequest()
	a.Settings["ch"] = make(chan int)
	if a.Hash() == "" {
		t.Fatal("no hash for a request with an unencodable setting")
	}
	if a.Hash() == baseRequest().Hash() {
		t.Error("an extra unencodable setting was dropped from the key")
	}
}

// -----------------------------------------------------------------------------
// run context
// -----------------------------------------------------------------------------

// A stamp with no run must leave the context alone, so a caller can stamp
// unconditionally without turning "no run" into "a run with a blank id".
func TestContextWithRunIsANoOpWithoutARun(t *testing.T) {
	base := context.Background()
	if got := ContextWithRun(base, RunContext{GoalId: "g1", Mode: RunModeReplay}); got != base {
		t.Error("a RunContext naming no run still altered the context")
	}
	if _, ok := RunFromContext(base); ok {
		t.Error("a bare context reported a run")
	}
}

func TestRunRoundTripsThroughTheContext(t *testing.T) {
	want := RunContext{RunId: "v1:work:run:r1", GoalId: "v1:work:goal:g1", StepKey: "s2", Mode: RunModeReplay}
	got, ok := RunFromContext(ContextWithRun(context.Background(), want))
	if !ok {
		t.Fatal("the run did not come back")
	}
	if got.RunId != want.RunId || got.GoalId != want.GoalId || got.StepKey != want.StepKey || got.Mode != want.Mode {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// The fork point decides which half of a fork is served from the journal, so
// its edges are worth pinning: a step at the fork point is NOT before it, and
// a step the source run never ran is not before it either -- it has no
// journaled answer, and guessing would serve it one recorded for another step.
func TestBeforeForkPoint(t *testing.T) {
	rc := RunContext{RunId: "r", ForkAtStepKey: "c", StepOrder: []string{"a", "b", "c", "d"}}
	for step, want := range map[string]bool{"a": true, "b": true, "c": false, "d": false, "unknown": false} {
		if got := rc.BeforeForkPoint(step); got != want {
			t.Errorf("BeforeForkPoint(%q) = %v, want %v", step, got, want)
		}
	}
	none := RunContext{RunId: "r", StepOrder: []string{"a"}}
	if none.BeforeForkPoint("a") {
		t.Error("a run with no fork point reported a step before it")
	}
}
