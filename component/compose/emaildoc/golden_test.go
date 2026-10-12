package emaildoc

import (
	"context"
	"encoding/base64"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/golden from the renderer")

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil || string(want) != got {
		t.Fatalf("%s differs from its golden (run with -update and review the diff): %v", name, err)
	}
}

func ptr(v float64) *float64 { return &v }

// goldenPictures are the pictures of the goldens: solid PNGs of 4 x 2, 3 x 2
// and 2 x 1 pixels, as fixed bytes. They are not tinyPNG's, because what an
// encoder writes changes between Go releases (compress/flate frames even
// stored data differently in 1.25 and 1.27), and a golden must change only
// when the renderer does.
var goldenPictures = map[string]string{
	"banner": "iVBORw0KGgoAAAANSUhEUgAAAAQAAAACCAIAAADwyuo0AAAAKklEQVR4AQAaAOX/AIiZqoiZqoiZqoiZqgCImaqImaqImaqImaoBAAD//7mCDlkEfMhvAAAAAElFTkSuQmCC",
	"photo":  "iVBORw0KGgoAAAANSUhEUgAAAAMAAAACCAIAAAASFvFNAAAAJElEQVR4AQAUAOv/AIiZqoiZqoiZqgCImaqImaqImaoBAAD//2rcCsNdhIBeAAAAAElFTkSuQmCC",
	"logo":   "iVBORw0KGgoAAAANSUhEUgAAAAIAAAABCAIAAAB7QOjdAAAAF0lEQVR4AQAHAPj/AIiZqoiZqgEAAP//DFADl48gxWQAAAAASUVORK5CYII=",
}

// TestRenderGoldenPerBlock renders each leaf block type, in send mode, as the
// only content of an email and compares the HTML and text parts with
// testdata/golden/block-<case>.html and .txt. The goldens were written by hand
// from the markup appendix of the layout engine plan before the renderer
// existed; -update rewrites them from the renderer, and the diff is then the
// review.
func TestRenderGoldenPerBlock(t *testing.T) {
	images := fakeImages{}
	for id, encoded := range goldenPictures {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		images[id] = Image{MIMEType: "image/png", Data: data}
	}
	cases := []struct {
		name   string
		blocks []Block
	}{
		{"image", []Block{
			{ID: "banner", Type: "image", Src: "artifact:banner", Alt: "A banner", Width: 552},
			{ID: "photo", Type: "image", Src: "artifact:photo", Alt: "Photo", Width: 200, Align: "left"},
			{ID: "edge", Type: "image", Src: "artifact:banner", Width: 600, Bleed: true},
		}},
		{"image-linked", []Block{
			{ID: "logo", Type: "image", Src: "artifact:logo", Alt: "Logo & name", Width: 226, Href: "https://example.com/?from=email&to=site"},
			{ID: "online", Type: "image", Src: "artifact:logo", Alt: "View online", Width: 552, Href: "{{viewUrl}}"},
		}},
		{"image-placeholder", []Block{
			{ID: "hero", Type: "image", Alt: "Hero picture", Width: 552, Height: 218},
			{ID: "portrait", Type: "image", Width: 201, Href: "https://example.com/team"},
		}},
		{"heading", []Block{
			{ID: "headline", Type: "heading", Text: "Hello & welcome,\n{{displayName}}"},
			{ID: "kicker", Type: "heading", Text: "Small <print>", Size: 14.7, LineHeight: 22.4, Color: "secondary", Align: "left", Italic: true, Weight: 400, Font: "body", LetterSpacing: "-0.032em", Padding: &[4]float64{8, 24, 0, 24}},
			{ID: "subhead", Type: "heading", Text: "Base font, computed line height", Size: 26.7, Color: "text", Font: "base"},
		}},
		{"text", []Block{
			{ID: "body", Type: "text", Text: "Hello {{displayName}},\nthanks for **joining** us.\n\n- *Fast* setup\n- ***Friendly*** help\n\n1. Pick a plan\n2. Read [the guide](https://example.com/guide?a=1&b=2)\n\nSee you soon."},
			{ID: "aside", Type: "text", Text: "Lead line\n\n- First point\n- Second point\n\nWrite to [us](mailto:hello@example.com).", Size: 18.7, LineHeight: 18.7, ListLineHeight: 17.64, Gap: ptr(10), Color: "muted", LinkColor: "accent", Align: "center", Weight: 700, Italic: true, Font: "heading", LetterSpacing: "0.05em"},
		}},
		{"highlight", []Block{
			{ID: "statement", Type: "highlight", Text: "The fix that works:\nmake one decision & keep it."},
			{ID: "aside", Type: "highlight", Text: "One quiet line\nand another", Size: 17.3, Color: "text", Background: "tint", Align: "left", Weight: 400, Italic: true, Gap: ptr(4)},
		}},
		{"button", []Block{
			{ID: "cta", Type: "button", Label: "Read the story", Href: "https://example.com/story"},
			{ID: "confirm", Type: "button", Label: "Confirm & go", Href: "{{confirmUrl}}", Width: 303, Size: 20.2, Color: "accent", TextColor: "text", Align: "left", Font: "base", PaddingY: 12},
		}},
		{"divider", []Block{
			{ID: "rule", Type: "divider"},
			{ID: "rule-thick", Type: "divider", Height: 2.5, Color: "muted", Padding: &[4]float64{8, 0, 8, 0}},
		}},
		{"spacer", []Block{
			{ID: "space", Type: "spacer", Height: 74.65},
			{ID: "space-small", Type: "spacer", Height: 8},
		}},
		{"html", []Block{
			{ID: "note", Type: "html", HTML: `<table width=100%><tr><td align=center style="font-size:13px;color:#8a8f98">Questions? <a href="mailto:hello@example.com">Write to us</a> &amp; we&#39;ll reply.<br>&copy; 2026 Example&nbsp;Co.</td></tr></table>`},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Document{Subject: "Golden", Theme: neutral(), Blocks: c.blocks}
			r, err := Render(context.Background(), d, images, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, "block-"+c.name+".html", r.HTML)
			checkGolden(t, "block-"+c.name+".txt", r.Text)
		})
	}
}
