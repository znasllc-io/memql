package ast

import (
	"testing"
)

// TestBuildTriggerTopic_HappyPath_AllFields locks the canonical
// structured-trigger assembly.
func TestBuildTriggerTopic_HappyPath_AllFields(t *testing.T) {
	cases := []struct {
		event, concept, want string
	}{
		{"node.created", "v1:cognition:participant", "graph.node.created.v1:cognition:participant"},
		{"node.updated", "v1:identity:user", "graph.node.updated.v1:identity:user"},
		{"node.deleted", "v1:cluster:node", "graph.node.deleted.v1:cluster:node"},
	}
	for _, c := range cases {
		got, err := BuildTriggerTopic(c.event, c.concept)
		if err != nil {
			t.Errorf("BuildTriggerTopic(%q, %q): %v", c.event, c.concept, err)
			continue
		}
		if got != c.want {
			t.Errorf("BuildTriggerTopic(%q, %q) = %q, want %q", c.event, c.concept, got, c.want)
		}
	}
}

// TestBuildTriggerTopic_ConceptLess locks the "any concept" case.
func TestBuildTriggerTopic_ConceptLess(t *testing.T) {
	got, err := BuildTriggerTopic("node.created", "")
	if err != nil {
		t.Fatalf("BuildTriggerTopic: %v", err)
	}
	want := "graph.node.created"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildTriggerTopic_RejectsBadKind locks the event-kind allowlist.
func TestBuildTriggerTopic_RejectsBadKind(t *testing.T) {
	cases := []string{"", "created", "node.foo", "graph.node.created"}
	for _, kind := range cases {
		_, err := BuildTriggerTopic(kind, "v1:foo:bar")
		if err == nil {
			t.Errorf("expected error for event=%q, got nil", kind)
		}
	}
}

// TestParseStructuredTriggerArgs_AllFieldsPresent locks the typical
// new-shape extraction.
func TestParseStructuredTriggerArgs_AllFieldsPresent(t *testing.T) {
	args := map[string]any{
		"event":   "node.created",
		"concept": "cog.participant",
	}
	got, err := ParseStructuredTriggerArgs(args)
	if err != nil {
		t.Fatalf("ParseStructuredTriggerArgs: %v", err)
	}
	if !got.HasEvent || got.EventKind != "node.created" {
		t.Errorf("EventKind = %q (has=%v), want node.created (has=true)", got.EventKind, got.HasEvent)
	}
	if !got.HasConcept || got.Concept != "cog.participant" {
		t.Errorf("Concept = %q (has=%v), want cog.participant (has=true)", got.Concept, got.HasConcept)
	}
}

// TestParseStructuredTriggerArgs_OmitConcept locks the concept-less
// case (event-only triggers like cluster.started).
func TestParseStructuredTriggerArgs_OmitConcept(t *testing.T) {
	args := map[string]any{
		"event": "node.deleted",
	}
	got, err := ParseStructuredTriggerArgs(args)
	if err != nil {
		t.Fatalf("ParseStructuredTriggerArgs: %v", err)
	}
	if got.HasConcept {
		t.Error("HasConcept should be false when concept= absent")
	}
}

// TestParseStructuredTriggerArgs_RejectsBadEventType locks the
// type-check on event=.
func TestParseStructuredTriggerArgs_RejectsBadEventType(t *testing.T) {
	args := map[string]any{"event": int64(1)}
	_, err := ParseStructuredTriggerArgs(args)
	if err == nil {
		t.Fatal("expected error for non-string event=, got nil")
	}
}

// TestEventKindAllowed locks the closed set of event kinds.
func TestEventKindAllowed(t *testing.T) {
	allowed := []string{"node.created", "node.updated", "node.deleted"}
	for _, k := range allowed {
		if !EventKindAllowed(k) {
			t.Errorf("EventKindAllowed(%q) = false, want true", k)
		}
	}
	disallowed := []string{"", "created", "node.x", "graph.node.created"}
	for _, k := range disallowed {
		if EventKindAllowed(k) {
			t.Errorf("EventKindAllowed(%q) = true, want false", k)
		}
	}
}
