package emaildoc

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/compose"
)

// The Materializer writes an email into a layout: the model is shown the
// layout's content blocks (ContentSlots) and answers with what to write in them
// (Content), and Merge puts that into the layout. Brand blocks, the theme and
// the structure come from the layout untouched; a block the layout does not
// have, a brand block, a field the block cannot carry, a picture nobody chose
// to include and a replaced link tag are refused rather than dropped.

func builtin(t *testing.T, id string) Document {
	t.Helper()
	d, ok := Layout(id)
	if !ok {
		t.Fatalf("no built-in layout %s", id)
	}
	return d
}

// blockByID finds a block anywhere in the document, inside sections and
// columns too.
func blockByID(t *testing.T, d Document, id string) Block {
	t.Helper()
	var found *Block
	walkBlocks(d.Blocks, func(b Block) bool {
		if b.ID == id {
			found = &b
			return false
		}
		return true
	})
	if found == nil {
		t.Fatalf("the document has no block %q", id)
	}
	return *found
}

func blockBytes(t *testing.T, b Block) []byte {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func documentBytes(t *testing.T, d Document) []byte {
	t.Helper()
	data, err := d.JSON()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMergeWritesContentAndKeepsBrand(t *testing.T) {
	layout := builtin(t, "builtin:article")
	before := documentBytes(t, layout)
	intro := "Autumn mornings are darker, so the night before matters more.\n\n- Set out a lamp.\n- Charge the phone."
	got, err := Merge(layout, Content{
		Subject:   "Autumn at Acme",
		Preheader: "Three small changes for darker mornings.",
		Blocks: []ContentItem{
			{ID: "intro", Text: &intro},
			{ID: "banner", Image: textPtr("memql-asset:abc"), Alt: textPtr("Autumn leaves on a doorstep")},
		},
	}, map[string]string{"memql-asset:abc": "artifact:a1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "Autumn at Acme" || got.Preheader != "Three small changes for darker mornings." {
		t.Fatalf("subject and preheader = %q / %q", got.Subject, got.Preheader)
	}
	if b := blockByID(t, got, "intro"); b.Text != intro {
		t.Fatalf("intro text = %q", b.Text)
	}
	if b := blockByID(t, got, "banner"); b.Src != "artifact:a1" || b.Alt != "Autumn leaves on a doorstep" {
		t.Fatalf("banner = src %q, alt %q; want the selected picture's source and the new alt", b.Src, b.Alt)
	}
	// The brand is the layout's, byte for byte: the logo, the divider, the
	// signature columns and the footer, and the theme.
	for _, id := range []string{"logo", "question", "question-more", "signature", "rule", "footer"} {
		if !bytes.Equal(blockBytes(t, blockByID(t, got, id)), blockBytes(t, blockByID(t, layout, id))) {
			t.Errorf("brand block %q changed", id)
		}
	}
	// A content block the draft did not name keeps the layout's example.
	for _, id := range []string{"highlight", "article", "ask"} {
		if !bytes.Equal(blockBytes(t, blockByID(t, got, id)), blockBytes(t, blockByID(t, layout, id))) {
			t.Errorf("content block %q changed although the draft did not name it", id)
		}
	}
	if got.Layout != "builtin:article" || got.Theme != layout.Theme {
		t.Fatalf("layout %q and theme %+v must be the layout's own", got.Layout, got.Theme)
	}
	// Merge works on a copy: the layout it was handed is unchanged.
	if after := documentBytes(t, layout); !bytes.Equal(before, after) {
		t.Fatal("Merge changed the layout it was given")
	}
	// What Merge returns is a document Parse admits, as it is written.
	if again, err := Parse(documentBytes(t, got)); err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("the merged document does not round trip through Parse: %v", err)
	}
}

func TestMergeRefuses(t *testing.T) {
	images := map[string]string{"memql-asset:abc": "artifact:a1"}
	cases := []struct {
		name   string
		layout string
		draft  Content
		want   string // the whole sentence, unless partial
		partly bool
	}{
		{"no subject", "builtin:article", Content{Subject: "  \n "}, "the draft has no subject", false},
		{"the same block twice", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "intro", Text: textPtr("One")}, {ID: "intro", Text: textPtr("Two")}}},
			`the draft wrote block "intro" twice`, false},
		{"a block the layout does not have", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "headline", Text: textPtr("Hi")}}},
			`the draft wrote to a block this layout does not have: "headline"`, false},
		{"a brand block", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "footer", Text: textPtr("Unsubscribe here")}}},
			`the draft tried to change a brand block: "footer"`, false},
		{"a brand block it changes nothing in", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "logo"}}},
			`the draft tried to change a brand block: "logo"`, false},
		// No role of its own: it takes the role of the brand columns it sits in.
		{"a block inside brand columns", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "signature-title", Text: textPtr("CEO")}}},
			`the draft tried to change a brand block: "signature-title"`, false},
		{"a label on text", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "intro", Label: textPtr("Read")}}},
			`the draft set label on text block "intro"`, false},
		{"a link on text", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "intro", Href: textPtr("https://example.com")}}},
			`the draft set href on text block "intro"`, false},
		{"text on a button", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "ask", Text: textPtr("Ask")}}},
			`the draft set text on button block "ask"`, false},
		{"a picture on a button", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "ask", Image: textPtr("memql-asset:abc")}}},
			`the draft set image on button block "ask"`, false},
		{"text on a picture", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "banner", Text: textPtr("Caption")}}},
			`the draft set text on image block "banner"`, false},
		{"text on a section", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "body", Text: textPtr("All of it")}}},
			`the draft set text on section block "body"`, false},
		{"a picture not selected for inclusion", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "banner", Image: textPtr("memql-asset:other")}}},
			"the draft used a picture that was not selected for inclusion", false},
		{"a picture source instead of a handle", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "banner", Image: textPtr("artifact:a1")}}},
			"the draft used a picture that was not selected for inclusion", false},
		{"a web picture", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "banner", Image: textPtr("https://cdn.example/a.png")}}},
			"the draft used a picture that was not selected for inclusion", false},
		{"the confirm link replaced", "builtin:confirmation", Content{Subject: "S", Blocks: []ContentItem{{ID: "confirm", Href: textPtr("https://evil.test")}}},
			`the draft tried to replace the {{confirmUrl}} link on "confirm"`, false},
		{"the confirm link replaced by another tag", "builtin:confirmation", Content{Subject: "S", Blocks: []ContentItem{{ID: "confirm", Href: textPtr("{{unsubscribeUrl}}")}}},
			`the draft tried to replace the {{confirmUrl}} link on "confirm"`, false},
		// What is written goes through Parse with the rest of the document.
		{"a link Parse refuses", "builtin:article", Content{Subject: "S", Blocks: []ContentItem{{ID: "intro", Text: textPtr("[Win](javascript:alert(1))")}}},
			`block "intro": a link must be`, true},
		{"a subject on two lines", "builtin:article", Content{Subject: "One\nTwo"}, "email subject must be a nonempty single line", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			layout := builtin(t, c.layout)
			before := documentBytes(t, layout)
			_, err := Merge(layout, c.draft, images)
			if err == nil {
				t.Fatalf("Merge accepted it; want %q", c.want)
			}
			if c.partly && !strings.Contains(err.Error(), c.want) || !c.partly && err.Error() != c.want {
				t.Fatalf("refusal = %q, want %q", err, c.want)
			}
			if !bytes.Equal(before, documentBytes(t, layout)) {
				t.Fatal("a refused merge changed the layout it was given")
			}
		})
	}
}

func TestMergeKeepsALinkTagItIsGivenAgain(t *testing.T) {
	layout := builtin(t, "builtin:confirmation")
	got, err := Merge(layout, Content{Subject: "Confirm", Blocks: []ContentItem{{ID: "confirm", Label: textPtr("Yes, sign me up"), Href: textPtr(TagConfirmURL)}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := blockByID(t, got, "confirm"); b.Label != "Yes, sign me up" || b.Href != TagConfirmURL {
		t.Fatalf("confirm = label %q, href %q", b.Label, b.Href)
	}
}

// An organization's layout can carry a link tag in a sentence rather than on a
// button. New text for that block keeps linking to it, as often as the layout
// did; written over with an address, or left out, it is refused.
func TestMergeKeepsLinkTagsInText(t *testing.T) {
	layout, err := Parse([]byte(layoutJSON(`{"id":"note","type":"text","text":"Please [confirm your address]({{confirmUrl}}).\n\n- [Unsubscribe]({{unsubscribeUrl}}) at any time."}`)))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ text, want string }{
		"an address in its place":   {"Please [confirm your address](https://evil.test/confirm).\n\n- [Unsubscribe]({{unsubscribeUrl}}) any time.", `the draft tried to replace the {{confirmUrl}} link on "note"`},
		"the link left out":         {"Welcome aboard.\n\n- [Unsubscribe]({{unsubscribeUrl}}) any time.", `the draft tried to replace the {{confirmUrl}} link on "note"`},
		"the tag kept, but as text": {"Confirm at {{confirmUrl}}.\n\n- [Unsubscribe]({{unsubscribeUrl}}) any time.", `the draft tried to replace the {{confirmUrl}} link on "note"`},
		"the other tag dropped":     {"Please [confirm]({{confirmUrl}}).", `the draft tried to replace the {{unsubscribeUrl}} link on "note"`},
	} {
		if _, err := Merge(layout, Content{Subject: "S", Blocks: []ContentItem{{ID: "note", Text: textPtr(c.text)}}}, nil); err == nil || err.Error() != c.want {
			t.Errorf("%s: refusal = %v, want %q", name, err, c.want)
		}
	}
	kept := "Tap [here]({{confirmUrl}}) to confirm, and [read our story](https://example.com/story).\n\n1. [Leave the list]({{unsubscribeUrl}}) whenever you like."
	got, err := Merge(layout, Content{Subject: "S", Blocks: []ContentItem{{ID: "note", Text: &kept}}}, nil)
	if err != nil || blockByID(t, got, "note").Text != kept {
		t.Fatalf("new text that keeps both links was refused: %v", err)
	}
	// Markdown that cannot be read is refused by Parse, which says why.
	if _, err := Merge(layout, Content{Subject: "S", Blocks: []ContentItem{{ID: "note", Text: textPtr("[]({{confirmUrl}}) [x]({{unsubscribeUrl}})")}}}, nil); err == nil || !strings.Contains(err.Error(), "a link needs a label") {
		t.Fatalf("unreadable Markdown: %v", err)
	}
}

// A link tag can also stand in text as itself ("Your own link: {{viewUrl}}"),
// and it is kept there too: new text must mention each tag as often as the
// layout's did, and link to it as often. A bare tag may become a link to
// itself; a link may not become bare text, and neither may go.
func TestMergeKeepsBareLinkTagsInText(t *testing.T) {
	layout, err := Parse([]byte(layoutJSON(`{"id":"note","type":"text","text":"Your own copy: {{viewUrl}}"},` +
		`{"id":"head","type":"heading","text":"Confirm at {{confirmUrl}}"},{"id":"plain","type":"text","text":"Nothing linked here."}`)))
	if err != nil {
		t.Fatal(err)
	}
	refused := []struct{ id, text, want string }{
		{"note", "Read it in your browser.", `the draft tried to replace the {{viewUrl}} link on "note"`},
		{"note", "Your own copy: https://evil.test/view", `the draft tried to replace the {{viewUrl}} link on "note"`},
		{"head", "Welcome aboard", `the draft tried to replace the {{confirmUrl}} link on "head"`},
	}
	for _, c := range refused {
		if _, err := Merge(layout, Content{Subject: "S", Blocks: []ContentItem{{ID: c.id, Text: textPtr(c.text)}}}, nil); err == nil || err.Error() != c.want {
			t.Errorf("%s -> %q: refusal = %v, want %q", c.id, c.text, err, c.want)
		}
	}
	for id, text := range map[string]string{
		"note": "Prefer a browser? Open [your own copy]({{viewUrl}}).",
		"head": "Almost there: {{confirmUrl}}",
	} {
		got, err := Merge(layout, Content{Subject: "S", Blocks: []ContentItem{{ID: id, Text: textPtr(text)}}}, nil)
		if err != nil || blockByID(t, got, id).Text != text {
			t.Errorf("%s -> %q keeps its tag and was refused: %v", id, text, err)
		}
	}
	// Nor can a draft put a link tag inside an address of its own, anywhere:
	// Parse refuses it with the rest of the document.
	if _, err := Merge(layout, Content{Subject: "S", Blocks: []ContentItem{{ID: "plain", Text: textPtr("[Leave](https://x.test/?u={{unsubscribeUrl}})")}}}, nil); err == nil ||
		!strings.Contains(err.Error(), "{{unsubscribeUrl}} must be the whole link") {
		t.Fatalf("a link tag inside a drafted address: %v", err)
	}
}

func TestMergeRefusesACustomDocument(t *testing.T) {
	custom, err := Parse([]byte(`{"subject":"Plain","textBody":"Hello","htmlBody":""}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Merge(custom, Content{Subject: "S"}, nil); err == nil {
		t.Fatal("Merge wrote into a custom HTML email, which has no blocks")
	}
}

func TestNullKeepsTheLayoutsValue(t *testing.T) {
	layout := builtin(t, "builtin:article")
	got, err := Merge(layout, Content{Subject: "Autumn", Blocks: []ContentItem{
		{ID: "intro"},
		{ID: "highlight", Text: textPtr("  \n\t ")},
		{ID: "ask", Label: textPtr(" "), Href: nil},
		{ID: "banner", Alt: textPtr(""), Image: nil},
		// Blank fields a block does not have are not set either, so they are not
		// refused: a model answering null-ish for everything changes nothing.
		{ID: "article", Label: textPtr(" "), Href: textPtr(""), Image: textPtr("  ")},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"intro", "highlight", "ask", "banner", "article"} {
		if !bytes.Equal(blockBytes(t, blockByID(t, got, id)), blockBytes(t, blockByID(t, layout, id))) {
			t.Errorf("block %q changed although the draft set nothing in it", id)
		}
	}
	// The preheader is the draft's: one it leaves empty is no preheader, never
	// the layout's example for another email.
	if got.Preheader != "" {
		t.Fatalf("preheader = %q, want none", got.Preheader)
	}
}

func TestContentSlotsListOnlyContentBlocks(t *testing.T) {
	article := builtin(t, "builtin:article")
	slots := ContentSlots(article)
	var ids []string
	for _, slot := range slots {
		ids = append(ids, slot.ID)
	}
	if want := []string{"banner", "intro", "highlight", "article", "ask"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("article slots = %v, want %v", ids, want)
	}
	byID := map[string]Slot{}
	for _, slot := range slots {
		byID[slot.ID] = slot
	}
	intro := blockByID(t, article, "intro")
	if s := byID["intro"]; s.Type != "text" || s.Text != intro.Text || s.Size != intro.Size {
		t.Errorf("intro slot = %+v", s)
	}
	if s := byID["banner"]; s.Type != "image" || s.Alt != "Article banner" || s.Width != 552 || s.Text != "" {
		t.Errorf("banner slot = %+v", s)
	}
	if s := byID["ask"]; s.Type != "button" || s.Label != "Ask our team" || s.Width != 305 {
		t.Errorf("ask slot = %+v", s)
	}
	confirmation := builtin(t, "builtin:confirmation")
	for _, slot := range ContentSlots(confirmation) {
		if slot.ID == "confirm" && slot.Href != TagConfirmURL {
			t.Errorf("the confirm slot shows the link it keeps: %+v", slot)
		}
	}

	// No layout offers a brand block, a container or a block with nothing to
	// write (a spacer, a divider, social links, the footer).
	for _, info := range Layouts() {
		brand := map[string]bool{}
		for _, entry := range roleList(info.Document.Blocks, "content") {
			id, role, _ := strings.Cut(entry, "=")
			brand[id] = role == "brand"
		}
		for _, slot := range ContentSlots(info.Document) {
			if brand[slot.ID] {
				t.Errorf("%s offers the brand block %q", info.ID, slot.ID)
			}
			switch slot.Type {
			case typeHeading, typeText, typeHighlight, typeButton, typeImage:
			default:
				t.Errorf("%s offers block %q of type %s, which has nothing to write", info.ID, slot.ID, slot.Type)
			}
		}
	}
}

// The Materializer files an email as its document's JSON, and a source package
// rebuilds the file with compose.RenderEmailFile, which knows nothing of email
// documents and only lays JSON out. For the rebuilt file to be the delivered
// one, laying a document's JSON out again must leave it byte for byte as it
// was: every built-in layout, and an email merged into one.
func TestRenderEmailFileKeepsADocumentsOwnBytes(t *testing.T) {
	var documents []Document
	for _, info := range Layouts() {
		documents = append(documents, info.Document)
	}
	merged, err := Merge(builtin(t, "builtin:article"), Content{Subject: "A < B & C", Preheader: "Fish & chips", Blocks: []ContentItem{
		{ID: "intro", Text: textPtr("One two, **three** & <four>.\n\n- [Five](https://example.com/?a=1&b=2)")},
		{ID: "banner", Image: textPtr("memql-asset:abc"), Alt: textPtr("A \"quoted\" picture")},
	}}, map[string]string{"memql-asset:abc": imageURI(t, "png")})
	if err != nil {
		t.Fatal(err)
	}
	documents = append(documents, merged)
	for _, doc := range documents {
		want := documentBytes(t, doc)
		got, err := compose.RenderEmailFile(string(want))
		if err != nil {
			t.Fatalf("%s: %v", doc.Layout, err)
		}
		if !bytes.Equal(got.Bytes, want) {
			t.Fatalf("%s: RenderEmailFile changed the document's bytes:\n got %q\nwant %q", doc.Layout, got.Bytes, want)
		}
	}
}

// A block's own role wins over its container's: a content block an
// organization keeps inside brand columns is still written.
func TestContentInsideABrandContainerIsWritable(t *testing.T) {
	d, err := Parse([]byte(layoutJSON(`{"id":"band","type":"columns","role":"brand","columns":[` +
		`{"width":50,"blocks":[{"id":"name","type":"heading","text":"Sam"}]},` +
		`{"width":50,"blocks":[{"id":"offer","type":"text","role":"content","text":"This week only."}]}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if slots := ContentSlots(d); len(slots) != 1 || slots[0].ID != "offer" {
		t.Fatalf("slots = %+v, want only offer", slots)
	}
	got, err := Merge(d, Content{Subject: "S", Blocks: []ContentItem{{ID: "offer", Text: textPtr("Next week too.")}}}, nil)
	if err != nil || blockByID(t, got, "offer").Text != "Next week too." {
		t.Fatalf("merge into a content block in brand columns: %v", err)
	}
	if _, err := Merge(d, Content{Subject: "S", Blocks: []ContentItem{{ID: "name", Text: textPtr("Alex")}}}, nil); err == nil || err.Error() != `the draft tried to change a brand block: "name"` {
		t.Fatalf("a block that takes the brand role of its columns: %v", err)
	}
}
