package app

import (
	"context"
	"strings"
	"testing"

	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	composeint "github.com/znasllc-io/memql/integrations/compose"
)

// draftReply is what a model writes for a document: a title and a body.
const draftReply = `{"title":"Autumn news","body":"Autumn is here.","header":[],"rows":[]}`

type referenceAI struct {
	t                         *testing.T
	visionCalls, composeCalls int
	images                    []common.VisionContent
	prompt                    string
	messages                  []common.ChatMessage
}

func (a *referenceAI) CallAIVision(_ context.Context, r airoute.ResolveRequest, prompt string, images []common.VisionContent) (memql.VisionAIResult, error) {
	a.visionCalls++
	a.images = images
	a.prompt = prompt
	if !r.Needs.Vision {
		a.t.Fatal("vision requirement missing")
	}
	return memql.VisionAIResult{Text: "Dark green headline, cream background", Resolution: airoute.Resolution{ProviderName: "fleet:vision", Model: "vision"}}, nil
}
func (a *referenceAI) CallAIStructured(_ context.Context, _ airoute.ResolveRequest, messages []common.ChatMessage, schema common.StructuredSchema) (memql.StructuredAIResult, error) {
	a.composeCalls++
	a.messages = messages
	if schema.Name != "materializerDraft" {
		a.t.Fatalf("a document was composed through the %q schema, not materializerDraft", schema.Name)
	}
	return memql.StructuredAIResult{Text: draftReply, Resolution: airoute.Resolution{ProviderName: "fleet:writer", Model: "writer"}}, nil
}

// documentRequest asks for a Markdown document from a brand reference and a
// written brief.
func documentRequest() composeint.ComposeRequest {
	return composeint.ComposeRequest{Format: pure.FormatMarkdown, Statement: "Write our autumn announcement",
		Sources: []composeint.Resolved{{Ref: composeint.SourceRef{Label: "Brand references", Content: true}, Rows: []map[string]any{{"blobUrl": "https://private-storage.test/secret"}},
			Files: []composeint.SourceContent{{Name: "example.png", MimeType: "image/png", Image: []byte("actual-image-bytes")}, {Name: "brief.md", MimeType: "text/plain", Text: "Use our autumn announcement"}}}}}
}

func TestMaterializerWritesADocumentFromVisualReferences(t *testing.T) {
	ai := &referenceAI{t: t}
	request := documentRequest()
	reply, err := (materializerComposer{engine: ai}).Compose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if ai.visionCalls != 1 || ai.composeCalls != 1 || string(ai.images[0].Data) != "actual-image-bytes" {
		t.Fatal("reference did not reach exactly one visual analysis and one composition")
	}
	if !strings.Contains(ai.prompt, "an editable document") {
		t.Fatalf("the visual analysis was not asked for a document: %s", ai.prompt)
	}
	input := ai.messages[1].Content
	if !strings.Contains(input, "Dark green headline") || !strings.Contains(input, "Use our autumn announcement") || strings.Contains(input, `"image":`) || strings.Contains(input, "private-storage") {
		t.Fatalf("composition input omitted references or sent image bytes or a storage address: %s", input)
	}
	if reply.Draft.Body != "Autumn is here." || reply.Draft.Title != "Autumn news" {
		t.Fatalf("the draft did not come back as written: %+v", reply.Draft)
	}
	if len(reply.Models) != 2 || reply.Models[0].Model != "vision" || reply.Models[1].Model != "writer" {
		t.Fatal("visual model contribution missing")
	}
	if len(request.Sources[0].Files[0].Image) == 0 {
		t.Fatal("reference snapshot was mutated")
	}
}

// Emails are made in the Email app. The integration refuses a request for one
// before it gets here; the composer refuses it too, before any model is asked.
func TestMaterializerComposerRefusesAnEmail(t *testing.T) {
	ai := &referenceAI{t: t}
	request := documentRequest()
	request.Format, request.OutputKind = pure.FormatJSON, "email_template"
	if _, err := (materializerComposer{engine: ai}).Compose(context.Background(), request); err == nil || err.Error() != "Emails are made in the Email app." {
		t.Fatalf("an email request: %v", err)
	}
	if ai.visionCalls != 0 || ai.composeCalls != 0 {
		t.Fatal("an email request reached a model")
	}
}
