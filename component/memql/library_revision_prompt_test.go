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
