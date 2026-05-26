package main

import (
	"strings"
	"testing"
)

// TestJSONPathExpr covers the path translator the audit pass uses
// to navigate payload.{...} fields via Postgres JSONB operators.
// The shape of the produced SQL string is part of the script's
// contract: a regression that emits `->` instead of `->>` for the
// terminal segment changes the column type from text to jsonb and
// breaks the audit's = ANY($1) text-array comparison.
func TestJSONPathExpr(t *testing.T) {
	cases := []struct {
		base, path, want string
	}{
		// Single-segment path: terminal `->>`, no intermediate `->`.
		{"payload", "agentId", "payload->>'agentId'"},
		// Nested path: intermediate `->` for the object, terminal
		// `->>` for the scalar. This is the v1:cognition:utterance
		// `payload.source.agentId` case.
		{"payload", "source.agentId", "payload->'source'->>'agentId'"},
		// Three-segment path proves the loop generalises.
		{"payload", "a.b.c", "payload->'a'->'b'->>'c'"},
		// Non-payload root works too -- defensive: the audit pins
		// `payload`, but the helper shouldn't bake that in.
		{"provenance", "name", "provenance->>'name'"},
	}
	for _, c := range cases {
		t.Run(c.base+":"+c.path, func(t *testing.T) {
			got, err := jsonPathExpr(c.base, c.path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestJSONPathExpr_Rejects locks the defensive rejects so the
// helper can't accidentally widen the audit's SELECT to the entire
// payload column when a caller passes garbage.
func TestJSONPathExpr_Rejects(t *testing.T) {
	cases := []struct {
		name, base, path, wantContains string
	}{
		{"empty base", "", "agentId", "base is empty"},
		{"empty path", "payload", "", "path is empty"},
		{"whitespace path", "payload", "   ", "path is empty"},
		{"leading dot", "payload", ".agentId", "empty segment"},
		{"trailing dot", "payload", "source.", "empty segment"},
		{"consecutive dots", "payload", "a..b", "empty segment"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := jsonPathExpr(c.base, c.path)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.wantContains) {
				t.Errorf("error %q missing %q", err.Error(), c.wantContains)
			}
		})
	}
}

// TestAgentReferenceConceptsCoverage locks in the concept set the
// audit knows how to walk. Adding a new agentId-bearing concept
// without updating this list silently shrinks the audit's
// coverage; the test fails so the addition lands with the
// accompanying audit entry.
//
// Membership rule: any concept whose payload carries a single-
// valued agentId field (string, not array). Array-valued fields
// like payload.agentIds[] are intentionally not in this set --
// see auditAgentReferences's CAVEAT.
func TestAgentReferenceConceptsCoverage(t *testing.T) {
	want := map[string]string{
		"v1:agents:agentAuthorization": "agentId",
		"v1:identity:delegation":       "agentId",
		"v1:cognition:audioOverride":   "agentId",
		"v1:cognition:videoOverride":   "agentId",
		"v1:cognition:clientToolRequest": "agentId",
		"v1:cognition:utterance":       "source.agentId",
	}
	got := map[string]string{}
	for _, ref := range agentReferenceConcepts {
		got[ref.Concept] = ref.Path
	}
	if len(got) != len(want) {
		t.Errorf("agent-reference concept count = %d, want %d", len(got), len(want))
	}
	for concept, wantPath := range want {
		gotPath, ok := got[concept]
		if !ok {
			t.Errorf("missing concept %q in agentReferenceConcepts", concept)
			continue
		}
		if gotPath != wantPath {
			t.Errorf("concept %q path = %q, want %q", concept, gotPath, wantPath)
		}
	}
}
