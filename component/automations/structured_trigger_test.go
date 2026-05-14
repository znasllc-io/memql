package automations

import (
	"log/slog"
	"os"
	"testing"
)

// TestStructuredTrigger_NormalizesToCanonicalTopic locks the contract
// that the structured @trigger form -- the canonical author surface in
// the new tree -- produces the same canonical 5-segment subscription
// topic the legacy single-string form produces. If this test breaks,
// every automation in dsl/<domain>/automations.memql that uses the
// structured form silently stops firing.
func TestStructuredTrigger_NormalizesToCanonicalTopic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	loader := NewLoader(LoaderOptions{Logger: logger})

	const structuredSource = `@trigger(event="node.created", concept="v1:cognition:participant", partition="*")
automation testStructured {
  step noop { logic noopLogic { } }
}`
	const legacySource = `@trigger(event="graph.node.created.*.v1:cognition:participant")
automation testLegacy {
  step noop { logic noopLogic { } }
}`

	structured, err := loader.compileMemQL(structuredSource, "test:structured")
	if err != nil {
		t.Fatalf("structured compile: %v", err)
	}
	legacy, err := loader.compileMemQL(legacySource, "test:legacy")
	if err != nil {
		t.Fatalf("legacy compile: %v", err)
	}
	if structured.Trigger.Event != legacy.Trigger.Event {
		t.Errorf("structured trigger %q != legacy trigger %q", structured.Trigger.Event, legacy.Trigger.Event)
	}
	if structured.Trigger.Event != "graph.node.created.*.v1:cognition:participant" {
		t.Errorf("unexpected canonical topic: %q", structured.Trigger.Event)
	}
}

// TestStructuredTrigger_DefaultsPartitionToWildcard locks the partition=
// default. Authors rarely care about partition scoping and the dominant
// case is "any partition" -- the loader has to supply "*" when the
// field is omitted.
func TestStructuredTrigger_DefaultsPartitionToWildcard(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	loader := NewLoader(LoaderOptions{Logger: logger})

	const src = `@trigger(event="node.updated", concept="v1:identity:user")
automation testDefaultPartition {
  step noop { logic noopLogic { } }
}`
	auto, err := loader.compileMemQL(src, "test:default-partition")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if auto.Trigger.Event != "graph.node.updated.*.v1:identity:user" {
		t.Errorf("expected wildcard partition default, got %q", auto.Trigger.Event)
	}
}

// TestStructuredTrigger_RejectsMissingConcept locks the
// "missing concept" error so future authors get a clear message rather
// than a silent un-routed trigger.
func TestStructuredTrigger_RejectsMissingConcept(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	loader := NewLoader(LoaderOptions{Logger: logger})

	const src = `@trigger(event="node.created")
automation testBad {
  step noop { logic noopLogic { } }
}`
	if _, err := loader.compileMemQL(src, "test:bad"); err == nil {
		t.Fatalf("expected error for structured form missing concept=, got nil")
	}
}

// TestStructuredTrigger_LeavesNonGraphTriggersAlone locks that
// system.* / cognition.* / schedule= triggers are untouched. Only
// the allowed event-kind values (node.created/updated/deleted) opt
// into the structured-form rewrite.
func TestStructuredTrigger_LeavesNonGraphTriggersAlone(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	loader := NewLoader(LoaderOptions{Logger: logger})

	cases := []struct {
		name, src, wantEvent string
	}{
		{
			name:      "system.startup",
			src:       `@trigger(event="system.startup")` + "\nautomation testSystem { step noop { logic noopLogic { } } }",
			wantEvent: "system.startup",
		},
		{
			name:      "cognition.capability.unmet",
			src:       `@trigger(event="cognition.capability.unmet")` + "\nautomation testCog { step noop { logic noopLogic { } } }",
			wantEvent: "cognition.capability.unmet",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auto, err := loader.compileMemQL(tc.src, "test:"+tc.name)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if auto.Trigger.Event != tc.wantEvent {
				t.Errorf("got %q, want %q", auto.Trigger.Event, tc.wantEvent)
			}
		})
	}
}
