package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// TestRenderConceptCatalog loads the embedded DSL (DB-free) and renders the
// catalog, asserting it is non-empty, carries public front-matter, and
// includes a known concept with a fields table.
func TestRenderConceptCatalog(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := memql.LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	concepts := memoryNodes.List()
	if len(concepts) == 0 {
		t.Fatal("no concepts loaded from the embedded DSL")
	}

	md := renderConceptCatalog(concepts)

	for _, want := range []string{
		"audience: public",
		"# Concept Catalog",
		"| Field | Type | Required | Description |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("catalog missing %q", want)
		}
	}
	// At least one concept heading with the canonical id shape v1:<ns>:<entity>.
	if !strings.Contains(md, "## `v1:") {
		t.Errorf("catalog has no v1: concept headings; got first 200 chars:\n%s", md[:min(200, len(md))])
	}
}

func TestTypeString(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"string", "string"},
		{[]any{"string", "null"}, "string \\| null"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := typeString(c.in); got != c.want {
			t.Errorf("typeString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
