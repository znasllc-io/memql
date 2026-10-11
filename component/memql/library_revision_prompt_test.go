package memql

import (
	"strings"
	"testing"
)

// Use the installed declarations and schema validator: replacing the model
// handler in workflow tests must not hide an array rejected before inference.
func TestDocumentRevisionPromptsAcceptPreviousEditItems(t *testing.T) {
	prompts, _ := loadCorpusPrompts(t)
	for _, name := range []string{"libraryRevisionResearch", "libraryRevisionItem"} {
		t.Run(name, func(t *testing.T) {
			prompt, ok := prompts.Get(name)
			if !ok {
				t.Fatalf("missing prompt %s", name)
			}
			data := map[string]any{"document": "Original sentence.", "passages": "[]", "instruction": "Make it clearer.", "previous": []any{map[string]any{"before": "Original sentence.", "after": "Earlier proposal.", "commentIds": []any{"feedback"}}}}
			if err := prompt.ValidateData(data); err != nil {
				t.Fatal(err)
			}
			rendered, err := prompt.Render(data)
			if err != nil || !strings.Contains(rendered, "Original sentence.") || !strings.Contains(rendered, "Earlier proposal.") {
				t.Fatalf("previous item lost in prompt: %q %v", rendered, err)
			}
			data["previous"] = "not an edit list"
			if err := prompt.ValidateData(data); err == nil {
				t.Fatal("invalid previous item admitted")
			}
			if name == "libraryRevisionResearch" {
				delete(data, "previous")
				if err := prompt.ValidateData(data); err != nil {
					t.Fatalf("initial review without previous edits: %v", err)
				}
			}
		})
	}
}

// Keep request anchors without presenting a second, obsolete document to the
// reviewer. This runs the installed template, not a matching test renderer.
func TestDocumentRevisionCompletionOmitsObsoleteSource(t *testing.T) {
	prompts, _ := loadCorpusPrompts(t)
	prompt, ok := prompts.Get("libraryRevisionCompletion")
	if !ok {
		t.Fatal("missing completion prompt")
	}
	data := map[string]any{
		"document":    "### Corrected profile\nVerified current prose.",
		"instruction": "Change only this profile.", "explanation": "Corrected the profile.",
		"passages": []any{map[string]any{
			"source": "**Old heading**\nObsolete paragraph that must not be re-reviewed.",
			"comments": []any{map[string]any{
				"kind": "feedback", "scope": "section", "quote": "Requested profile",
				"feedback": "Use a level-three heading.", "prefix": "Anchor prefix",
				"suffix": "Anchor suffix", "sectionPath": []any{"Chapter", "Profile"},
			}},
		}},
	}
	if err := prompt.ValidateData(data); err != nil {
		t.Fatal(err)
	}
	rendered, err := prompt.Render(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"### Corrected profile", "Verified current prose.", "Change only this profile.", "Use a level-three heading.", "Requested profile", "Anchor prefix", "Anchor suffix", "/Chapter/Profile"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("missing request/candidate context %q", want)
		}
	}
	for _, old := range []string{"**Old heading**", "Obsolete paragraph that must not be re-reviewed."} {
		if strings.Contains(rendered, old) {
			t.Fatalf("obsolete section leaked into review: %q", old)
		}
	}
}
