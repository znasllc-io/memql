package library

import (
	"strings"
	"testing"
	"unicode/utf8"
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

func TestRevisionPatchesPreserveFormattingWithoutUserReminders(t *testing.T) {
	source := "---\r\ntitle: Workshop\n---\r\n# Activities\n\n- **Draft Sharing**: Read excerpts.\r\n- [Peer feedback](https://example.com/review): Give comments.\n\nCafé notes.\r\n"
	captured := capturedRevision(source, "Sharing", 5, 6)
	captured["comments"].([]any)[0].(map[string]any)["body"] = "Rename this to Draft Exchange"
	before := strings.ReplaceAll(source, "\r\n", "\n")
	proposal, err := buildRevisionProposal(captured, revisionAnswer{Summary: "Rename activity", Edits: []revisionReplacement{{
		Before: before, After: strings.Replace(before, "Draft Sharing", "Draft Exchange", 1), Reason: "Rename selected activity", CommentIDs: []string{"note-1"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(source, "Draft Sharing", "Draft Exchange", 1); proposal["revisedContent"] != want {
		t.Fatalf("formatting or unrelated bytes changed: %q", proposal["revisedContent"])
	}
	if err := validateRevisionResult(captured, proposal); err != nil {
		t.Fatal(err)
	}
}

func TestRevisionExtensionsCanInsertAtRequestedLocationsWithoutRewriting(t *testing.T) {
	source := "# Guide\n\nExisting introduction.\n\n## Last section\n\nExisting ending.\n"
	captured := map[string]any{"content": source, "comments": []any{map[string]any{"id": "note-1", "body": "Add practice prompts", "anchor": map[string]any{"kind": "document-end", "quote": "End of document"}}}}
	for _, tc := range []struct {
		name, before, after string
		allowed             bool
	}{
		{"append", "Existing ending.", "Existing ending.\n\n## Practice\n\nTry an example.", true},
		{"rewrite ending", "Existing ending.", "A polished ending.\n\n## Practice\n\nTry an example.", false},
		{"insert in middle", "Existing introduction.", "Existing introduction.\n\nTry an example.", true},
		{"delete", "Existing introduction.", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := buildRevisionProposal(captured, revisionAnswer{Summary: "Add practice prompts", Edits: []revisionReplacement{{Before: tc.before, After: tc.after, Reason: "Practice prompts", CommentIDs: []string{"note-1"}}}})
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v, error=%v", tc.allowed, err)
			}
			if tc.allowed && p["revisedContent"] != strings.Replace(source, tc.before, tc.after, 1) {
				t.Fatal("extension changed unrelated content")
			}
		})
	}
}

func TestRevisionChangeBoundsStayOnUTF8Characters(t *testing.T) {
	for _, tc := range [][2]string{{"café", "cafè"}, {"\u0080", "\u0480"}, {"😀 text", "😁 text"}, {"same", "same"}, {"", "new"}, {"old", ""}} {
		prefix, beforeEnd, afterEnd := revisionChangeBounds(tc[0], tc[1])
		for _, part := range []string{tc[0][:prefix], tc[0][prefix:beforeEnd], tc[0][beforeEnd:], tc[1][:prefix], tc[1][prefix:afterEnd], tc[1][afterEnd:]} {
			if !utf8.ValidString(part) {
				t.Fatalf("split a Unicode character in %q → %q: %d/%d/%d", tc[0], tc[1], prefix, beforeEnd, afterEnd)
			}
		}
		if tc[0][:prefix]+tc[1][prefix:afterEnd]+tc[0][beforeEnd:] != tc[1] {
			t.Fatal("changed the replacement")
		}
	}
}

func TestAnchoredExtensionsKeepAdditiveContract(t *testing.T) {
	source := "# Guide\n\n## Examples\n\nAn existing example.\n\n## Next\n\nKeep this.\n"
	for _, section := range []bool{false, true} {
		start, quote := 4, "existing example"
		if section {
			start, quote = 2, "Examples"
		}
		captured := capturedRevision(source, quote, start, start+1)
		anchor := captured["comments"].([]any)[0].(map[string]any)["anchor"].(map[string]any)
		anchor["intent"] = "extend"
		if section {
			anchor["scope"] = "section"
		}
		passages, err := revisionPassages(captured)
		if err != nil {
			t.Fatal(err)
		}
		if passages[0].Comments[0]["kind"] != "extend" || passages[0].Comments[0]["scope"] != anchor["scope"] {
			t.Fatalf("lost extension location: %v", passages)
		}
		for _, after := range []string{"An existing example.\n\nOne more example.", "A replacement example."} {
			proposal, err := buildRevisionProposal(captured, revisionAnswer{Summary: "Add an example", Edits: []revisionReplacement{{Before: "An existing example.", After: after, Reason: "Add example", CommentIDs: []string{"note-1"}}}})
			additive := strings.HasPrefix(after, "An existing example.")
			if (err == nil) != additive {
				t.Fatalf("additive=%v, err=%v", additive, err)
			}
			if additive && proposal["revisedContent"] != strings.Replace(source, "An existing example.", after, 1) {
				t.Fatal("changed unrelated section")
			}
		}
	}
}

// The editor's empty state includes newline-only files. An insertion must
// preserve those bytes, while an empty anchor in real content stays invalid.
func TestRevisionEmptyAnchorInBlankDocument(t *testing.T) {
	for _, source := range []string{"", "\n", " \t\r\n\r\n", "# Existing\n"} {
		captured := map[string]any{"content": source, "comments": []any{map[string]any{"id": "note-1", "body": "Create a report", "anchor": map[string]any{"kind": "document", "quote": "Entire document"}}}}
		edit := revisionReplacement{Before: "", After: "# Report\n\nA generated draft.\n", Reason: "Create requested report", CommentIDs: []string{"note-1"}}
		proposal, err := buildRevisionProposal(captured, revisionAnswer{Summary: "Create report", Edits: []revisionReplacement{edit}})
		if strings.TrimSpace(source) != "" {
			if err == nil {
				t.Fatal("empty anchor accepted in nonempty source")
			}
			continue
		}
		if err != nil {
			t.Fatalf("blank %q: %v", source, err)
		}
		after := edit.After
		if strings.Contains(source, "\r\n") {
			after = strings.ReplaceAll(after, "\n", "\r\n")
		}
		if proposal["revisedContent"] != after+source {
			t.Fatalf("original blank bytes changed: %q", proposal["revisedContent"])
		}
		if err := validateRevisionResult(captured, proposal); err != nil {
			t.Fatal(err)
		}
		if _, err := buildRevisionProposal(captured, revisionAnswer{Summary: "Ambiguous", Edits: []revisionReplacement{edit, edit}}); err == nil {
			t.Fatal("multiple empty anchors accepted")
		}
	}
}

func TestRevisionSectionResearchIncludesBodyAndNestedHeadings(t *testing.T) {
	for _, heading := range []string{"## Selected", "Selected\n--------"} {
		source := "# Book\n\n" + heading + "\n\nSelected claim.\n\n### Child\n\nChild evidence.\n\n```markdown\n## Not a boundary\n```\n\n> ## Quoted heading\n> Quoted evidence.\n\n## Unrelated\n\nPrivate unrelated chapter.\n"
		captured := capturedRevision(source, "Selected", 2, 3)
		anchor := captured["comments"].([]any)[0].(map[string]any)["anchor"].(map[string]any)
		anchor["scope"] = "section"
		passages, err := revisionPassages(captured)
		if err != nil {
			t.Fatal(err)
		}
		if len(passages) != 1 {
			t.Fatalf("passages=%v", passages)
		}
		for _, want := range []string{"Selected claim.", "Child evidence.", "## Not a boundary", "Quoted evidence."} {
			if !strings.Contains(passages[0].Source, want) {
				t.Fatalf("section lost %q: %v", want, passages)
			}
		}
		if strings.Contains(passages[0].Source, "Unrelated") {
			t.Fatal("section research leaked the next chapter")
		}
		delete(anchor, "scope")
		narrow, err := revisionPassages(captured)
		if err != nil || strings.Contains(narrow[0].Source, "Selected claim.") {
			t.Fatalf("ordinary heading selection expanded: %v %v", narrow, err)
		}
	}
}
