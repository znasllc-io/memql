package ast

import (
	"testing"
)

// TestBuildTriggerTopic_HappyPath_AllFields locks the canonical
// structured-trigger assembly.
func TestBuildTriggerTopic_HappyPath_AllFields(t *testing.T) {
	cases := []struct {
		event, concept, partition, want string
	}{
		{"node.created", "v1:cognition:participant", "*", "graph.node.created.*.v1:cognition:participant"},
		{"node.updated", "v1:identity:user", "acme", "graph.node.updated.acme.v1:identity:user"},
		{"node.deleted", "v1:cluster:node", "_system", "graph.node.deleted._system.v1:cluster:node"},
	}
	for _, c := range cases {
		got, err := BuildTriggerTopic(c.event, c.concept, c.partition)
		if err != nil {
			t.Errorf("BuildTriggerTopic(%q, %q, %q): %v", c.event, c.concept, c.partition, err)
			continue
		}
		if got != c.want {
			t.Errorf("BuildTriggerTopic(%q, %q, %q) = %q, want %q", c.event, c.concept, c.partition, got, c.want)
		}
	}
}

// TestBuildTriggerTopic_ConceptLess locks the "any concept" case.
func TestBuildTriggerTopic_ConceptLess(t *testing.T) {
	got, err := BuildTriggerTopic("node.created", "", "*")
	if err != nil {
		t.Fatalf("BuildTriggerTopic: %v", err)
	}
	want := "graph.node.created.*"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildTriggerTopic_RejectsBadKind locks the event-kind allowlist.
func TestBuildTriggerTopic_RejectsBadKind(t *testing.T) {
	cases := []string{"", "created", "node.foo", "graph.node.created"}
	for _, kind := range cases {
		_, err := BuildTriggerTopic(kind, "v1:foo:bar", "*")
		if err == nil {
			t.Errorf("expected error for event=%q, got nil", kind)
		}
	}
}

// TestBuildTriggerTopic_RejectsEmptyPartition locks the partition
// required-non-empty rule.
func TestBuildTriggerTopic_RejectsEmptyPartition(t *testing.T) {
	_, err := BuildTriggerTopic("node.created", "v1:foo:bar", "")
	if err == nil {
		t.Fatal("expected error for empty partition, got nil")
	}
}

// TestParseStructuredTriggerArgs_AllFieldsPresent locks the typical
// new-shape extraction.
func TestParseStructuredTriggerArgs_AllFieldsPresent(t *testing.T) {
	args := map[string]any{
		"event":     "node.created",
		"concept":   "cog.participant",
		"partition": "*",
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
	if !got.HasPartition || got.Partition != "*" {
		t.Errorf("Partition = %q (has=%v), want * (has=true)", got.Partition, got.HasPartition)
	}
}

// TestParseStructuredTriggerArgs_OmitConcept locks the concept-less
// case (event-only triggers like cluster.started).
func TestParseStructuredTriggerArgs_OmitConcept(t *testing.T) {
	args := map[string]any{
		"event":     "node.deleted",
		"partition": "*",
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

// TestParseStructuredTriggerArgs_RejectsBadPartitionType locks the
// type-check on partition=.
func TestParseStructuredTriggerArgs_RejectsBadPartitionType(t *testing.T) {
	args := map[string]any{"partition": 42}
	_, err := ParseStructuredTriggerArgs(args)
	if err == nil {
		t.Fatal("expected error for non-string partition=, got nil")
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
