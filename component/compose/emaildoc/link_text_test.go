package emaildoc

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLinkTextBoundaries(t *testing.T) {
	for _, tag := range LinkTags() {
		for _, text := range []string{tag, "Use " + tag + ".", "(" + tag + ")", tag + ", then continue", tag + ".\nNext"} {
			if problem := LinkTextProblem(text); problem != "" {
				t.Errorf("safe text %q: %s", text, problem)
			}
		}
		for _, text := range []string{"https://other.test/?u=" + tag, "=" + tag, "é" + tag, tag + ".other.test", tag + "/other", tag + "?u=1", tag + "@other.test", tag + "\u0301"} {
			if problem := LinkTextProblem(text); problem == "" {
				t.Errorf("attached recipient link was accepted: %q", text)
			}
		}
	}
}

func TestParseRefusesRecipientLinksAttachedToVisibleAddresses(t *testing.T) {
	for _, kind := range []string{"text", "heading", "highlight", "footer"} {
		for _, text := range []string{"https://other.test/?u={{viewUrl}}", "{{confirmUrl}}/elsewhere"} {
			block := map[string]any{"id": "note", "type": kind}
			if kind == "footer" {
				block["lines"] = []string{text}
			} else {
				block["text"] = text
			}
			raw, _ := json.Marshal(block)
			if _, err := Parse([]byte(layoutJSON(string(raw)))); err == nil {
				t.Errorf("%s accepted an attached recipient link: %s", kind, text)
			}
		}
	}
	for _, text := range []string{
		"https://other.test/?u=**{{viewUrl}}**",
		"**https://other.test/?u=**{{viewUrl}}",
		"[{{viewUrl}}](https://other.test/)",
		"[{{viewUrl}}]({{viewUrl}})",
	} {
		raw, _ := json.Marshal(map[string]any{"id": "note", "type": "text", "text": text})
		if _, err := Parse([]byte(layoutJSON(string(raw)))); err == nil {
			t.Errorf("Markdown hid an attached recipient link: %s", text)
		}
	}
}

func TestMergeCannotPreserveARecipientLinkAsAnotherLinksLabel(t *testing.T) {
	layout, err := Parse([]byte(layoutJSON(`{"id":"note","type":"text","text":"Your own copy: {{viewUrl}}"}`)))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"[{{viewUrl}}](https://other.test/)", "[{{viewUrl}}]({{viewUrl}})"} {
		_, err := Merge(layout, Content{Subject: "S", Blocks: []ContentItem{{ID: "note", Text: &text}}}, nil)
		if err == nil || !strings.Contains(err.Error(), "not its label") {
			t.Errorf("draft %q was not refused for its label: %v", text, err)
		}
	}
}
