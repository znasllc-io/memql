package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tliron/commonlog"
	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/component/memql/sense"
)

func TestCompletionKindMapping(t *testing.T) {
	cases := map[string]protocol.CompletionItemKind{
		"keyword":      protocol.CompletionItemKindKeyword,
		"function":     protocol.CompletionItemKindFunction,
		"concept":      protocol.CompletionItemKindClass,
		"field":        protocol.CompletionItemKindField,
		"shape":        protocol.CompletionItemKindStruct,
		"unknown-kind": protocol.CompletionItemKindText,
	}
	for k, want := range cases {
		if got := completionKind(k); got != want {
			t.Errorf("completionKind(%q) = %v; want %v", k, got, want)
		}
	}
}

func TestToLSPCompletionItem(t *testing.T) {
	ci := toLSPCompletionItem(sense.CompletionItem{
		Label: "query", Kind: "keyword", Detail: "construct",
		Documentation: "a query", InsertText: "query", SortPriority: 3,
	})
	if ci.Label != "query" {
		t.Errorf("label = %q", ci.Label)
	}
	if ci.Kind == nil || *ci.Kind != protocol.CompletionItemKindKeyword {
		t.Errorf("kind = %v; want Keyword", ci.Kind)
	}
	if ci.Detail == nil || *ci.Detail != "construct" {
		t.Errorf("detail = %v", ci.Detail)
	}
	if ci.InsertText == nil || *ci.InsertText != "query" {
		t.Errorf("insertText = %v", ci.InsertText)
	}
	if ci.SortText == nil || *ci.SortText != "00000003" {
		t.Errorf("sortText = %v; want zero-padded 00000003", ci.SortText)
	}
	mc, ok := ci.Documentation.(protocol.MarkupContent)
	if !ok || mc.Kind != protocol.MarkupKindMarkdown || mc.Value != "a query" {
		t.Errorf("documentation = %+v; want markdown 'a query'", ci.Documentation)
	}
}

func completionAt(t *testing.T, s *server, uri string, line, char int) []protocol.CompletionItem {
	t.Helper()
	res, err := s.completion(noopCtx(), &protocol.CompletionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
			Position:     protocol.Position{Line: protocol.UInteger(line), Character: protocol.UInteger(char)},
		},
	})
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	items, _ := res.([]protocol.CompletionItem)
	return items
}

// TestCompletion_RegistryRefreshCrossFile is the WP4 headline: a concept added
// (and saved) in one file becomes visible to completion in another after the
// registry rebuild. It exercises buildSense re-reading the workspace and the
// atomic swap.
func TestCompletion_RegistryRefreshCrossFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "gadgets"), 0o755); err != nil {
		t.Fatal(err)
	}
	commonlog.Configure(-4, nil)
	s := newServer(dir, commonlog.GetLogger(lsName))
	s.buildSense() // workspace has no product concept yet

	const uriB = "file:///b.memql"
	s.docs.open(uriB, "query ") // ContextConstructConcept after "query "

	hasGadget := func() bool {
		for _, it := range completionAt(t, s, uriB, 0, 6) {
			if it.Detail != nil && *it.Detail == "v1:gadgets:gadget" {
				return true
			}
		}
		return false
	}
	if hasGadget() {
		t.Fatal("gadget concept should not exist before file A is written")
	}

	// File A defines the concept; the save-driven rebuild picks it up.
	fileA := filepath.Join(dir, "gadgets", "concepts.memql")
	if err := os.WriteFile(fileA, []byte(`@version("1.0.0")
@namespace("gadgets")
@description("A gadget.")
concept gadget {
  label string @required @description("Label")
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	s.buildSense() // the rebuild scheduleRebuild would trigger on didSave

	if !hasGadget() {
		t.Error("concept defined in file A should complete in file B after the registry rebuild")
	}
}
