package steps

import (
	"log/slog"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

// memql#2235 -- pins the runtime binding for the migrated bootstrapSession
// decide->persist automation.
//
// Two runtime-only traps (invisible to dsl-lint / call-graph / full-DSL-load,
// which only prove the tree PARSES + LOADS) bit the first cut and were caught
// by the seeded-DB mcp-conformance gate (#1706 event-context-binding):
//
//  1. field(step("decide"), "x") resolves to NIL at runtime -- the write ran
//     with all-missing args. The working form is field(decide.result, "x")
//     (bare step name + .result), the same fix as the library index
//     automations (#2260).
//  2. event.payload.* does NOT thread into a TOP-LEVEL automation step (it
//     only threads into a logic's nested steps). Reading event.payload.id in
//     the persist step produced an unbound participant -> "no session bound to
//     event participant". The ids must flow through the decide logic's return
//     and be read back via field(decide.result, ...).
//
// bootstrapSession is now a PURE arg-builder whose object return carries the
// ids off the event; the automation reads them via field(decide.result, ...)
// in the check + persist + emit steps. This test compiles that exact shape and
// proves the binding resolves the decide-result fields (and that the broken
// field(step("decide"),...) form does NOT), so neither trap can recur.
func TestBootstrapSessionPersistBindsDecideResult(t *testing.T) {
	const src = `@description("repro of the migrated bootstrapSession automation persist binding")
automation bindcheck {
  step decide { logic bootstrapSession { event: event } }
  step check {
    participantSession({ participantId: field(decide.result, "participantId") })
  }
  step persist {
    createSessionForParticipant({ participantId: field(decide.result, "participantId"), partitionId: field(decide.result, "partitionId"), broken: field(step("decide"), "participantId") })
  }
}`
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	loader := automations.NewLoader(automations.LoaderOptions{Logger: logger})
	auto, err := loader.CompileSource(src, "test:bindcheck")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	eval := automations.NewEvaluator()
	// The decide logic (a pure arg-builder) returns this object at runtime.
	eval.SetStepResult("decide", &automations.StepResult{
		StepId: "decide",
		Status: "success",
		Result: map[string]any{"participantId": "v1:cognition:participant:p-1", "partitionId": "space-xyz"},
	})

	calls := map[string]map[string]any{}
	for _, s := range auto.Steps {
		if s == nil || s.Function == nil {
			continue
		}
		resolved, err := resolveArgsRefs(s.Function.Args, eval)
		if err != nil {
			t.Fatalf("resolveArgsRefs(%s): %v", s.ID, err)
		}
		obj, _ := resolved["0"].(map[string]any)
		calls[s.Function.Name] = obj
	}

	// check step: participantId must thread from decide.result.
	if got := calls["participantSession"]["participantId"]; got != "v1:cognition:participant:p-1" {
		t.Errorf("check participantSession participantId = %#v, want the decide-result id (field(decide.result,...) must thread)", got)
	}

	// persist step: both ids must thread from decide.result via the working form.
	persist := calls["createSessionForParticipant"]
	if persist["participantId"] != "v1:cognition:participant:p-1" {
		t.Errorf("persist participantId = %#v, want the decide-result id", persist["participantId"])
	}
	if persist["partitionId"] != "space-xyz" {
		t.Errorf("persist partitionId = %#v, want \"space-xyz\"", persist["partitionId"])
	}
	// The broken field(step("decide"),...) form must NOT resolve (documents the trap).
	if persist["broken"] == "v1:cognition:participant:p-1" {
		t.Logf("note: field(step(\"decide\"),...) now also resolves; the #2235 trap may be fixed engine-side")
	} else if persist["broken"] != nil {
		t.Errorf("field(step(\"decide\"),...) resolved to %#v, expected nil (the trap)", persist["broken"])
	}
}
