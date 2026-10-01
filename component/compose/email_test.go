package compose

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEmailOutputPreservesEditableHTMLAndText(t *testing.T) {
	input := EmailTemplate{Subject: "Welcome", TextBody: "Hello {{displayName}}", HTMLBody: `<table><tr><td style="color:#145c44">Hello {{displayName}}</td></tr></table>`}
	raw, _ := json.Marshal(input)
	result, err := RenderEmailTemplate(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	var saved EmailTemplate
	if err := json.Unmarshal(result.Bytes, &saved); err != nil || saved != input {
		t.Fatalf("template changed: %v %v", saved, err)
	}
	if result.Embedded || !strings.Contains(result.Note, "composition") {
		t.Fatal("JSON provenance claim is wrong")
	}
}
func TestEmailOutputRefusesMalformedOrIncompleteTemplates(t *testing.T) {
	for _, source := range []string{`null`, `[]`, `{"subject":"Hi"}`, `{"subject":"Hi\nInjected","textBody":"Hi","htmlBody":"Hi"}`, `{"subject":"Hi","textBody":"Hi","htmlBody":"Hi","extra":true}`, `{"subject":"Hi","textBody":"Hi","htmlBody":"Hi"} {}`} {
		if _, err := RenderEmailTemplate(source); err == nil {
			t.Fatalf("accepted %s", source)
		}
	}
}
