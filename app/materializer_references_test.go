package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	composeint "github.com/znasllc-io/memql/integrations/compose"
)

type referenceAI struct {
	t                         *testing.T
	visionCalls, composeCalls int
	images                    []common.VisionContent
	messages                  []common.ChatMessage
}

func (a *referenceAI) CallAIVision(_ context.Context, r airoute.ResolveRequest, _ string, images []common.VisionContent) (memql.VisionAIResult, error) {
	a.visionCalls++
	a.images = images
	if !r.Needs.Vision {
		a.t.Fatal("vision requirement missing")
	}
	return memql.VisionAIResult{Text: "Dark green headline, cream background", Resolution: airoute.Resolution{ProviderName: "fleet:vision", Model: "vision"}}, nil
}
func (a *referenceAI) CallAIStructured(_ context.Context, _ airoute.ResolveRequest, messages []common.ChatMessage, schema common.StructuredSchema) (memql.StructuredAIResult, error) {
	a.composeCalls++
	a.messages = messages
	if schema.Name != "emailTemplate" {
		a.t.Fatal("email was composed through the generic table schema")
	}
	return memql.StructuredAIResult{Text: `{"subject":"Autumn news","textBody":"Welcome to autumn","htmlBody":"<table><tr><td>Welcome to autumn</td></tr></table>"}`, Resolution: airoute.Resolution{ProviderName: "fleet:writer", Model: "writer"}}, nil
}
func TestMaterializerComposesEditableEmailFromVisualReferences(t *testing.T) {
	ai := &referenceAI{t: t}
	request := composeint.ComposeRequest{Format: pure.FormatJSON, OutputKind: "email_template", Statement: "Create our autumn welcome email", Sources: []composeint.Resolved{{Ref: composeint.SourceRef{Label: "Brand references"}, Files: []composeint.SourceContent{{Name: "example.png", MimeType: "image/png", Image: []byte("actual-image-bytes")}, {Name: "brief.md", MimeType: "text/plain", Text: "Use our autumn announcement"}}}}}
	reply, err := (materializerComposer{engine: ai}).Compose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if ai.visionCalls != 1 || ai.composeCalls != 1 || string(ai.images[0].Data) != "actual-image-bytes" {
		t.Fatal("reference did not reach exactly one visual analysis and one composition")
	}
	input := ai.messages[1].Content
	if !strings.Contains(input, "Dark green headline") || !strings.Contains(input, "Use our autumn announcement") || strings.Contains(input, `"image":`) {
		t.Fatalf("composition input omitted references or sent image base64 as prose: %s", input)
	}
	var email pure.EmailTemplate
	if err := json.Unmarshal([]byte(reply.Draft.Body), &email); err != nil || !strings.Contains(email.HTMLBody, "<table>") {
		t.Fatal("output is not editable email JSON")
	}
	if len(reply.Models) != 2 || reply.Models[0].Model != "vision" || reply.Models[1].Model != "writer" {
		t.Fatal("visual model contribution missing")
	}
	if len(request.Sources[0].Files[0].Image) == 0 {
		t.Fatal("reference snapshot was mutated")
	}
}
