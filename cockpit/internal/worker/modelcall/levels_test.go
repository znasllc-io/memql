package modelcall

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

// levels_test.go pins what a LEVEL on a model call does on this machine
// (memql#5393, memql-cockpit#438): it never changes the model, and nothing
// is invented on the way back.
//
// The router has already chosen `model` from this machine's advertisement,
// so the level does not displace it -- a level steers an app, which has
// knobs, while a runtime has only the model it was asked for. The one runtime
// knob it reaches is Ollama's thinking on a fast call (ollama_think_test.go).
// And the End's usage carries the model the runtime REPORTED and no effort,
// because neither Ollama nor an OpenAI-compatible response states one.
func TestModelCall_TheLevelNeverDisplacesTheModelAndNoEffortIsGuessed(t *testing.T) {
	srv, seen := ollamaChatStub(t, []string{"ok"}, true)
	m := managerFor(inventoryWith(ollamaModel(srv.URL, "llama3.1:8b",
		models.Attributes{ContextWindow: 8192, MaxConcurrent: 1})))
	rec := newRecorder()

	call := start("r-level", "llama3.1:8b", KindChat)
	call.Level = "reasoning"
	m.Start(context.Background(), rec, call)
	end := rec.wait(t)

	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(*seen, &sent); err != nil {
		t.Fatalf("the runtime saw no readable request: %v", err)
	}
	if sent.Model != "llama3.1:8b" {
		t.Errorf("the runtime was asked for %q; the level must not displace the model the router chose", sent.Model)
	}
	u := end.GetUsage()
	if u.GetModel() != "llama3.1:8b" {
		t.Errorf("usage.model = %q, want the runtime's own report", u.GetModel())
	}
	if u.GetEffort() != "" {
		t.Errorf("usage.effort = %q, want empty: no local runtime states an effort, and "+
			"the level is a request, not a report", u.GetEffort())
	}
}

// The ENVELOPE'S level is what turns a fast call's thinking off, and this pins
// the whole hop on this side: ModelCallStart.level through the Manager to the
// body Ollama receives. The engine sends it on every fleet model call
// (memql fix/fast-local-triage); before that, goalComplexityTriage -- a fast
// prompt with no response schema -- reached a 4B model with thinking on and
// spent 2,052-5,636 hidden tokens (26 s to 2 m 51 s) on a fifty-token answer.
// A Manager that dropped the level would put that back with every client test
// still green.
func TestModelCall_AFastEnvelopeRunsWithThinkingOff(t *testing.T) {
	for _, tc := range []struct {
		level     string
		wantThink *bool
	}{
		{level: "fast", wantThink: new(bool)},
		{level: "", wantThink: nil},
		{level: "strong", wantThink: nil},
	} {
		t.Run("level="+tc.level, func(t *testing.T) {
			srv, seen := ollamaChatStub(t, []string{`{"complexity":"trivial"}`}, true)
			m := managerFor(inventoryWith(ollamaModel(srv.URL, "qwen3.5:4b",
				models.Attributes{ContextWindow: 8192, MaxConcurrent: 1})))
			rec := newRecorder()

			call := start("r-"+tc.level, "qwen3.5:4b", KindChat)
			call.Level = tc.level
			m.Start(context.Background(), rec, call)
			if end := rec.wait(t); end.GetError() != "" {
				t.Fatalf("call failed: %s", end.GetError())
			}

			got := thinkSent(t, *seen)
			switch {
			case tc.wantThink == nil && got != nil:
				t.Errorf("think = %v, want the key absent: above fast the model's own default stands", *got)
			case tc.wantThink != nil && (got == nil || *got != *tc.wantThink):
				t.Errorf("think = %v, want false: a fast envelope must reach Ollama with thinking off", got)
			}
		})
	}
}
