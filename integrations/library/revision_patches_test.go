package library

import (
	"strings"
	"testing"
)

func capturedRevision(source, quote string, start, end int) map[string]any {
	return map[string]any{"content": source, "comments": []any{map[string]any{"id": "note-1", "body": "Requested change", "anchor": map[string]any{"kind": "markdown", "startLine": start, "endLine": end, "quote": quote, "sourceQuote": strings.Join(strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")[start:end], "\n")}}}}
}
func TestRevisionPatchesPreserveUnchangedBytes(t *testing.T) {
	source := "---\r\ntitle: Example\r\n---\r\n# Plan\r\n\r\nKeep this café paragraph exactly, except obsolete words.\r\n\r\n## Next\r\n\r\nMore detail.\r\n"
	captured := capturedRevision(source, "obsolete words", 5, 6)
	for _, tc := range []struct {
		name  string
		edits []revisionReplacement
		want  string
	}{
		{"precise Unicode edit", []revisionReplacement{{Before: "obsolete words", After: "clear wording", Reason: "Clarify", CommentIDs: []string{"note-1"}}}, strings.Replace(source, "obsolete words", "clear wording", 1)},
		{"delete selected phrase", []revisionReplacement{{Before: ", except obsolete words", After: "", Reason: "Remove", CommentIDs: []string{"note-1"}}}, strings.Replace(source, ", except obsolete words", "", 1)},
		{"move to another section", []revisionReplacement{{Before: ", except obsolete words", After: "", Reason: "Remove here", CommentIDs: []string{"note-1"}}, {Before: "More detail.", After: "More detail.\n\nThe obsolete words belong here.", Reason: "Move there", CommentIDs: []string{"note-1"}}}, strings.Replace(strings.Replace(source, ", except obsolete words", "", 1), "More detail.", "More detail.\r\n\r\nThe obsolete words belong here.", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := buildRevisionProposal(captured, revisionAnswer{Summary: tc.name, Edits: tc.edits})
			if err != nil {
				t.Fatal(err)
			}
			if p["revisedContent"] != tc.want {
				t.Fatalf("unexpected changes: %q", p["revisedContent"])
			}
			if err = validateRevisionResult(captured, p); err != nil {
				t.Fatal(err)
			}
			p["revisedContent"] = "Injected"
			if validateRevisionResult(captured, p) == nil {
				t.Fatal("tampered approved result accepted")
			}
		})
	}
}
func TestRevisionPatchesRefuseAmbiguousOverlappingOrInventedEdits(t *testing.T) {
	captured := capturedRevision("# Heading\n\nSame words. Same words.", "Same", 2, 3)
	for _, edits := range [][]revisionReplacement{
		{{Before: "Same", After: "Other", Reason: "Ambiguous", CommentIDs: []string{"note-1"}}},
		{{Before: "Not in source", After: "Other", Reason: "Invented", CommentIDs: []string{"note-1"}}},
		{{Before: "Heading", After: "Other", Reason: "Unknown note", CommentIDs: []string{"someone-else"}}},
		{{Before: "# Heading", After: "# Other", Reason: "A", CommentIDs: []string{"note-1"}}, {Before: "Heading", After: "New", Reason: "B", CommentIDs: []string{"note-1"}}},
	} {
		if _, err := buildRevisionProposal(captured, revisionAnswer{Summary: "Test", Edits: edits}); err == nil {
			t.Fatalf("unsafe edits accepted: %+v", edits)
		}
	}
}
func TestRevisionExtensionsIncludeImportedAndEmptyDocuments(t *testing.T) {
	for _, source := range []string{"", "# Imported\n\nExisting content.\n"} {
		captured := map[string]any{"content": source, "comments": []any{map[string]any{"id": "note-1", "body": "Add examples", "anchor": map[string]any{"kind": "document-end", "quote": "End of document"}}}}
		before := "Existing content."
		after := "Existing content.\n\n## Examples\n\nA useful example."
		if source == "" {
			before = ""
			after = "# New document\n\nA useful example.\n"
		}
		p, err := buildRevisionProposal(captured, revisionAnswer{Summary: "Extend the document", Edits: []revisionReplacement{{Before: before, After: after, Reason: "Add requested example", CommentIDs: []string{"note-1"}}}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(asString(p["revisedContent"]), "useful example") {
			t.Fatal("missing extension")
		}
	}
}

func TestRevisionPatchesRefuseSelfOverlappingOccurrence(t *testing.T) {
	captured := capturedRevision("aaa", "aa", 0, 1)
	_, err := buildRevisionProposal(captured, revisionAnswer{Summary: "Clarify", Edits: []revisionReplacement{{Before: "aa", After: "b", Reason: "Clarify", CommentIDs: []string{"note-1"}}}})
	if err == nil {
		t.Fatal("overlapping duplicate occurrences were treated as unique")
	}
}
