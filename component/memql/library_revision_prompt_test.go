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
		"edits":       []any{},
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
	data["recovery"] = "Independently check whether the objection is justified."
	data["previousFindings"] = []any{map[string]any{
		"kind": "present_text", "quote": "### Corrected profile",
		"explanation": "The title is still a bold paragraph.",
	}}
	if err := prompt.ValidateData(data); err != nil {
		t.Fatal(err)
	}
	rendered, err = prompt.Render(data)
	if err != nil || !strings.Contains(rendered, "untrusted review data") || !strings.Contains(rendered, "Quote: ### Corrected profile") || !strings.Contains(rendered, "Objection: The title is still a bold paragraph.") {
		t.Fatalf("reassessment lost or trusted the previous objection: %q %v", rendered, err)
	}
	data["previousFindings"] = "not a findings array"
	if err := prompt.ValidateData(data); err == nil {
		t.Fatal("invalid prior findings admitted")
	}
}

// The preview contains untouched sections; edit scope comes from native patches,
// not from counting profiles in that preview. Old prose and model reasons must
// not become competing candidates in the reviewer prompt.
func TestDocumentRevisionCompletionCarriesMeasuredEditScope(t *testing.T) {
	prompts, _ := loadCorpusPrompts(t)
	prompt, ok := prompts.Get("libraryRevisionCompletion")
	if !ok {
		t.Fatal("missing completion prompt")
	}
	data := map[string]any{
		"document":    "# Report\n\nUntouched introduction.\n\n### First\nNew first.\n\n### Second\nNew second.\n\nUntouched conclusion.",
		"instruction": "Replace exactly two profiles; keep everything else.",
		"explanation": "Two replacements.", "passages": []any{},
		"edits": []any{
			map[string]any{"startLine": 5, "endLine": 7, "commentIds": []any{"request-first"}, "before": "Obsolete first prose.", "after": "New first.", "reason": "Untrusted editor claim."},
			map[string]any{"startLine": 9, "endLine": 11, "commentIds": []any{"request-second"}, "before": "Obsolete second prose.", "after": "New second."},
		},
	}
	if err := prompt.ValidateData(data); err != nil {
		t.Fatal(err)
	}
	rendered, err := prompt.Render(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Validated edit count: 2", "Original lines 5–7", "Original lines 9–11", "request-first", "request-second", "Untouched introduction.", "Untouched conclusion."} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("missing measured scope or preview context %q", want)
		}
	}
	for _, obsolete := range []string{"Obsolete first prose.", "Obsolete second prose.", "Untrusted editor claim."} {
		if strings.Contains(rendered, obsolete) {
			t.Fatalf("obsolete/editor text leaked into measured scope: %q", obsolete)
		}
	}
	data["edits"] = []any{}
	rendered, err = prompt.Render(data)
	if err != nil || !strings.Contains(rendered, "Validated edit count: 0") {
		t.Fatalf("lost no-op scope: %v", err)
	}
	data["edits"] = "not an edit list"
	if err := prompt.ValidateData(data); err == nil {
		t.Fatal("invalid edit scope admitted")
	}
}

func TestDocumentRevisionImagePlanCarriesMissingEvidence(t *testing.T) {
	prompts, _ := loadCorpusPrompts(t)
	prompt, ok := prompts.Get("libraryRevisionImagePlan")
	if !ok {
		t.Fatal("missing image planning prompt")
	}
	data := map[string]any{
		"passages": "Requested portraits", "references": "Attached references",
		"evidence": "Exact raster and its catalog association", "previous": `{"images":[],"limitations":"Missing raster"}`,
		"gaps": "Missing raster",
	}
	if err := prompt.ValidateData(data); err != nil {
		t.Fatal(err)
	}
	rendered, err := prompt.Render(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Missing raster", "Exact raster and its catalog association", "structurally valid but incomplete", "retaining its already verified entries unchanged"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("missing recovery context %q: %s", want, rendered)
		}
	}
	if strings.Contains(rendered, "previous plan failed validation") {
		t.Fatal("missing evidence misrepresented as invalid JSON")
	}
	delete(data, "gaps")
	if err := prompt.ValidateData(data); err != nil {
		t.Fatal(err)
	}
	rendered, err = prompt.Render(data)
	if err != nil || !strings.Contains(rendered, "previous plan failed validation") {
		t.Fatalf("contract repair lost: %s %v", rendered, err)
	}
	delete(data, "previous")
	if err := prompt.ValidateData(data); err != nil {
		t.Fatal(err)
	}
	if _, err := prompt.Render(data); err != nil {
		t.Fatal(err)
	}
}
