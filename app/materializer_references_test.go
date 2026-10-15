package app

import (
	"context"
	"strings"
	"testing"

	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	composeint "github.com/znasllc-io/memql/integrations/compose"
)

// emailContentReply is what a model writes into the Confirmation layout: its
// headline, and nothing else.
const emailContentReply = `{"subject":"Autumn news","preheader":"","content":[{"id":"headline","text":"Autumn","label":null,"href":null,"alt":null,"image":null}]}`

type referenceAI struct {
	t                         *testing.T
	visionCalls, composeCalls int
	images                    []common.VisionContent
	messages                  []common.ChatMessage
	reply                     string // the structured reply; emailContentReply when empty
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
	if schema.Name != "emailContent" {
		a.t.Fatalf("an email was composed through the %q schema, not emailContent", schema.Name)
	}
	reply := a.reply
	if reply == "" {
		reply = emailContentReply
	}
	return memql.StructuredAIResult{Text: reply, Resolution: airoute.Resolution{ProviderName: "fleet:writer", Model: "writer"}}, nil
}

// emailRequest asks for an email written into the Confirmation layout, from a
// brand reference whose picture may appear in the email and a written brief.
func emailRequest(t *testing.T) composeint.ComposeRequest {
	t.Helper()
	layout, ok := emaildoc.Layout("builtin:confirmation")
	if !ok {
		t.Fatal("no confirmation layout")
	}
	return composeint.ComposeRequest{Format: pure.FormatJSON, OutputKind: "email_template", Statement: "Create our autumn welcome email", EmailLayout: &layout,
		Sources: []composeint.Resolved{{Ref: composeint.SourceRef{Label: "Brand references", Content: true, IncludeImages: true}, Rows: []map[string]any{{"blobUrl": "https://private-storage.test/secret"}},
			Files: []composeint.SourceContent{{Name: "example.png", MimeType: "image/png", Image: []byte("actual-image-bytes")}, {Name: "brief.md", MimeType: "text/plain", Text: "Use our autumn announcement"}}}}}
}

func TestMaterializerWritesEmailContentFromVisualReferences(t *testing.T) {
	ai := &referenceAI{t: t}
	request := emailRequest(t)
	reply, err := (materializerComposer{engine: ai}).Compose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if ai.visionCalls != 1 || ai.composeCalls != 1 || string(ai.images[0].Data) != "actual-image-bytes" {
		t.Fatal("reference did not reach exactly one visual analysis and one composition")
	}
	if ai.messages[0].Content != emailContentInstructions {
		t.Fatal("the email was not written under the email content instructions")
	}
	input := ai.messages[1].Content
	if !strings.Contains(input, "Dark green headline") || !strings.Contains(input, "Use our autumn announcement") || strings.Contains(input, `"image":`) || strings.Contains(input, "private-storage") {
		t.Fatalf("composition input omitted references or sent image bytes or a storage address: %s", input)
	}
	// The model sees the layout's content blocks, with their text as the example,
	// and none of its brand blocks.
	if !strings.Contains(input, `"id":"headline"`) || !strings.Contains(input, "Welcome aboard!") || !strings.Contains(input, `"id":"confirm"`) {
		t.Fatalf("the layout's content blocks were not offered: %s", input)
	}
	for _, brand := range []string{"footer", `"id":"logo"`, "rule-top", "rule-bottom"} {
		if strings.Contains(input, brand) {
			t.Fatalf("a brand block (%s) was offered to the model: %s", brand, input)
		}
	}
	// The picture selected for inclusion is offered by its handle only.
	if handle := "memql-asset:" + (pure.Result{Bytes: []byte("actual-image-bytes")}).SHA256(); !strings.Contains(input, handle) {
		t.Fatalf("the included picture's handle %s was not offered: %s", handle, input)
	}
	if reply.EmailContent == nil || len(reply.EmailContent.Blocks) != 1 || reply.EmailContent.Blocks[0].ID != "headline" ||
		reply.EmailContent.Blocks[0].Text == nil || *reply.EmailContent.Blocks[0].Text != "Autumn" || reply.EmailContent.Blocks[0].Label != nil || reply.EmailContent.Subject != "Autumn news" {
		t.Fatalf("the content the model wrote did not come back as written: %+v", reply.EmailContent)
	}
	if reply.Draft.Body != "" {
		t.Fatalf("an email is written as content for its layout, not as a draft body: %q", reply.Draft.Body)
	}
	if len(reply.Models) != 2 || reply.Models[0].Model != "vision" || reply.Models[1].Model != "writer" {
		t.Fatal("visual model contribution missing")
	}
	if len(request.Sources[0].Files[0].Image) == 0 {
		t.Fatal("reference snapshot was mutated")
	}
}

func TestMaterializerEmailNeedsALayout(t *testing.T) {
	ai := &referenceAI{t: t}
	request := emailRequest(t)
	request.EmailLayout = nil
	if _, err := (materializerComposer{engine: ai}).Compose(context.Background(), request); err == nil || err.Error() != "materializer: an email needs a layout" {
		t.Fatalf("an email without a layout: %v", err)
	}
	if ai.visionCalls != 0 || ai.composeCalls != 0 {
		t.Fatal("an email without a layout reached a model")
	}
}

// The reply is read strictly: a field the schema does not have, or anything
// after the object, is a reply that cannot be trusted to mean what it says.
func TestMaterializerEmailContentIsReadStrictly(t *testing.T) {
	for name, reply := range map[string]string{
		"an unknown field":      `{"subject":"S","preheader":"","content":[],"html":"<p>Hi</p>"}`,
		"an unknown item field": `{"subject":"S","preheader":"","content":[{"id":"headline","text":"Hi","label":null,"href":null,"alt":null,"image":null,"role":"brand"}]}`,
		"trailing data":         emailContentReply + `{"subject":"again"}`,
		"not an object":         `["headline"]`,
		"cut off":               `{"subject":"S","preheader":"","content":[`,
	} {
		ai := &referenceAI{t: t, reply: reply}
		if _, err := (materializerComposer{engine: ai}).Compose(context.Background(), emailRequest(t)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
