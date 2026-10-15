package compose

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// canonicalEmail writes v the way an email document writes its own file
// (emaildoc's Document.JSON, which this package may not import): markup left
// as it is, two-space indentation, one trailing newline.
func canonicalEmail(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// An email document in its canonical form: the shape of a layout, with what
// JSON could write in more than one way (markup and an ampersand, kept as they
// are; a line separator, escaped; a fraction; an empty list).
func sampleEmail(t *testing.T) []byte {
	t.Helper()
	type block struct {
		ID      string      `json:"id"`
		Type    string      `json:"type"`
		Text    string      `json:"text,omitempty"`
		HTML    string      `json:"html,omitempty"`
		Padding *[4]float64 `json:"padding,omitempty"`
		Blocks  []block     `json:"blocks,omitempty"`
	}
	return canonicalEmail(t, struct {
		Subject string            `json:"subject"`
		Layout  string            `json:"layout"`
		Theme   map[string]string `json:"theme"`
		Blocks  []block           `json:"blocks"`
		Lines   []string          `json:"lines"`
	}{
		Subject: "A < B & C",
		Layout:  "builtin:article",
		Theme:   map[string]string{"page": "#eef0f3", "headingFont": "Georgia"},
		Blocks: []block{
			{ID: "x", Type: "html", HTML: "<b>A &amp; B</b>"},
			{ID: "body", Type: "section", Blocks: []block{{ID: "t", Type: "text", Text: "one two\n\n- three", Padding: &[4]float64{20.47555319531862, 24, 0, 24}}}},
		},
		Lines: []string{},
	})
}

// The Materializer files an email as the canonical bytes of its document, and
// a source package rebuilds that file with RenderEmailFile alone, so the
// canonical bytes come back unchanged. (emaildoc's own tests hold every
// built-in layout to the same.)
func TestRenderEmailFileIsCanonical(t *testing.T) {
	want := sampleEmail(t)
	got, err := RenderEmailFile(string(want))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes, want) {
		t.Fatalf("RenderEmailFile changed the canonical bytes:\n got %q\nwant %q", got.Bytes, want)
	}
	if got.Embedded || !strings.Contains(got.Note, "composition") {
		t.Fatalf("an email file carries no provenance, and its note must say where it is: %+v", got)
	}
	// The same document written compactly comes out the same.
	var compact bytes.Buffer
	if err := json.Compact(&compact, want); err != nil {
		t.Fatal(err)
	}
	again, err := RenderEmailFile(compact.String())
	if err != nil || !bytes.Equal(again.Bytes, want) {
		t.Fatalf("a compact copy did not come out canonical: %q %v", again.Bytes, err)
	}
}

func TestRenderEmailFileRefusesWhatIsNotAnEmailFile(t *testing.T) {
	limit := 2 << 20
	pad := func(n int) string { return `{"a":"` + strings.Repeat("x", n-len(`{"a":""}`)) + `"}` }
	for name, body := range map[string]string{
		"a list":                   `[]`,
		"null":                     `null`,
		"a string":                 `"subject"`,
		"nothing":                  ``,
		"not JSON":                 `{"subject":`,
		"two values":               `{} {}`,
		"over 2 MiB":               pad(limit + 1),
		"over 2 MiB once laid out": pad(limit - 2), // the file it would write is a few bytes longer than the body
	} {
		if _, err := RenderEmailFile(body); err == nil {
			t.Errorf("%s was written as an email file", name)
		}
	}
	if _, err := RenderEmailFile(pad(limit - 32)); err != nil {
		t.Fatalf("a body whose file fits in 2 MiB was refused: %v", err)
	}
}

// RenderRecipeBytes writes an email recipe through RenderEmailFile, and
// nothing else: a recipe saved before a file was filed rebuilds it exactly.
func TestRenderRecipeBytesWritesAnEmailRecipeAsItsFile(t *testing.T) {
	body := sampleEmail(t)
	got, err := RenderRecipeBytes(RenderRecipe{Name: "Welcome", Format: FormatJSON, OutputKind: "email_template", Draft: Draft{Body: string(body)}})
	if err != nil || !bytes.Equal(got.Bytes, body) {
		t.Fatalf("the email recipe wrote %q (%v), want the document's own bytes", got.Bytes, err)
	}
}
