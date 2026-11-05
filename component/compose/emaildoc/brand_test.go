package emaildoc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// testPNG is a data: URI of a small PNG; noisy makes it hard to compress, so
// its size is about side*side*3 bytes.
func testPNG(t *testing.T, side int, noisy bool) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	rng := rand.New(rand.NewSource(int64(side)))
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			c := color.RGBA{R: 40, G: 90, B: 160, A: 255}
			if noisy {
				c = color.RGBA{R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)), B: uint8(rng.Intn(256)), A: 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func testKit(t *testing.T) BrandKit {
	t.Helper()
	logo, photo := testPNG(t, 4, false), testPNG(t, 5, false)
	return BrandKit{
		Theme: Theme{Page: "#eef2f6", Canvas: "#ffffff", Primary: "#12355b", Secondary: "#1d4f7c", Accent: "#c0392b",
			Tint: "#f3f6f9", Text: "#1a1a1a", Muted: "#5a5a5a", HeadingFont: "Georgia", BodyFont: "Arial"},
		Logo:      BrandPicture{Src: logo, Alt: "Northwind", Href: "https://northwind.example"},
		Signature: &BrandSignature{Photo: photo, NameImage: testPNG(t, 6, false), Title: "Founder of Northwind"},
		Follow:    "Find us online",
		Social: []Link{
			{Network: "Instagram", Href: "https://instagram.com/northwind"},
			{Network: "YouTube", Href: "https://youtube.com/@northwind", Icon: testPNG(t, 7, false)},
		},
		Footer: BrandFooter{
			Lines: []string{"NORTHWIND", "1 Harbour Road"},
			Links: []Link{{Label: "Contact", Href: "mailto:hi@northwind.example"}, {Label: "Unsubscribe", Href: TagUnsubscribeURL}},
		},
		Reply: &BrandReply{Heading: "Write back", ButtonLabel: "Reply", ButtonHref: "mailto:hi@northwind.example", Note: "We read everything."},
	}
}

func layoutOrFail(t *testing.T, id string) Document {
	t.Helper()
	d, ok := Layout(id)
	if !ok {
		t.Fatalf("no layout %s", id)
	}
	return d
}

func hasID(blocks []Block, id string) bool { return findBlock(blocks, id) != nil }

func TestBrandKitJSONRoundTrip(t *testing.T) {
	kit := testKit(t)
	sep := " | "
	kit.Footer.Separator = &sep
	first, err := kit.JSON()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseBrandKit(first)
	if err != nil {
		t.Fatalf("ParseBrandKit: %v\n%s", err, first)
	}
	if !reflect.DeepEqual(back, kit) {
		t.Fatalf("round trip differs:\n got %+v\nwant %+v", back, kit)
	}
	second, err := back.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("JSON is not stable")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(first, &keys); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(first), "{\n  \"theme\"") {
		t.Fatalf("theme should come first:\n%.60s", first)
	}
}

func TestParseBrandKitRefusesFooterWithoutUnsubscribe(t *testing.T) {
	kit := testKit(t)
	kit.Footer.Links = []Link{{Label: "Contact", Href: "mailto:hi@northwind.example"}}
	raw, _ := kit.JSON()
	_, err := ParseBrandKit(raw)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unsubscribe") {
		t.Fatalf("err = %v, want one that mentions unsubscribe", err)
	}
}

func TestParseBrandKitRefusesArtifactPictures(t *testing.T) {
	kit := testKit(t)
	kit.Logo.Src = "artifact:0123456789abcdef0123456789abcdef"
	raw, _ := kit.JSON()
	_, err := ParseBrandKit(raw)
	if err == nil || !strings.Contains(err.Error(), "Upload pictures through the Brand page") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseBrandKitRefusals(t *testing.T) {
	good := testKit(t)
	cases := map[string]func(k *BrandKit){
		"logo src required": func(k *BrandKit) { k.Logo.Src = "" },
		"link tag in social": func(k *BrandKit) {
			k.Social = []Link{{Network: "X", Href: TagViewURL}}
		},
		"link tag on logo":    func(k *BrandKit) { k.Logo.Href = TagViewURL },
		"nine social":         func(k *BrandKit) { k.Social = make([]Link, 9) },
		"long follow":         func(k *BrandKit) { k.Follow = strings.Repeat("a", 201) },
		"thirteen lines":      func(k *BrandKit) { k.Footer.Lines = make([]string, 13) },
		"long line":           func(k *BrandKit) { k.Footer.Lines = []string{strings.Repeat("a", 301)} },
		"reply without label": func(k *BrandKit) { k.Reply.ButtonLabel = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			k := good
			sig, rep := *good.Signature, *good.Reply
			k.Signature, k.Reply = &sig, &rep
			mutate(&k)
			raw, err := k.JSON()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseBrandKit(raw); err == nil {
				t.Fatal("kit accepted")
			}
		})
	}
	raw, _ := good.JSON()
	both := strings.Replace(string(raw), `"name": {`, `"name": {"text": "Sam",`, 1)
	if _, err := ParseBrandKit([]byte(both)); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("name with both: %v", err)
	}
	if _, err := ParseBrandKit([]byte(`{"theme":{},"extra":1}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown key: %v", err)
	}
}

func TestApplyBrandFillsEverySlot(t *testing.T) {
	kit := testKit(t)
	for _, id := range []string{"builtin:letter", "builtin:article"} {
		t.Run(id, func(t *testing.T) {
			layout := layoutOrFail(t, id)
			got, err := ApplyBrand(layout, kit)
			if err != nil {
				t.Fatal(err)
			}
			if got.Theme != kit.Theme {
				t.Fatalf("theme = %+v", got.Theme)
			}
			if b := findBlock(got.Blocks, "logo"); b == nil || b.Src != kit.Logo.Src || b.Alt != "Northwind" || b.Href != kit.Logo.Href {
				t.Fatalf("logo = %+v", b)
			}
			if b := findBlock(got.Blocks, "signature-photo"); b == nil || b.Src != kit.Signature.Photo {
				t.Fatalf("photo = %+v", b)
			}
			if b := findBlock(got.Blocks, "signature-name"); b == nil || b.Src != kit.Signature.NameImage {
				t.Fatalf("name = %+v", b)
			}
			if b := findBlock(got.Blocks, "signature-title"); b == nil || b.Text != "Founder of Northwind" {
				t.Fatalf("title = %+v", b)
			}
			if b := findBlock(got.Blocks, "follow"); id == "builtin:letter" && (b == nil || b.Text != "Find us online") {
				t.Fatalf("follow = %+v", b)
			}
			if b := findBlock(got.Blocks, "social"); id == "builtin:letter" && (b == nil || !reflect.DeepEqual(b.Links, kit.Social)) {
				t.Fatalf("social = %+v", b)
			}
			if b := findBlock(got.Blocks, "footer"); b == nil || !reflect.DeepEqual(b.Links, kit.Footer.Links) || !reflect.DeepEqual(b.Lines, kit.Footer.Lines) {
				t.Fatalf("footer = %+v", b)
			}
			if id == "builtin:article" {
				if b := findBlock(got.Blocks, "reply-heading"); b == nil || b.Text != "Write back" {
					t.Fatalf("reply-heading = %+v", b)
				}
				if b := findBlock(got.Blocks, "reply-button"); b == nil || b.Label != "Reply" || b.Href != kit.Reply.ButtonHref {
					t.Fatalf("reply-button = %+v", b)
				}
				if b := findBlock(got.Blocks, "reply-note"); b == nil || b.Text != "We read everything." {
					t.Fatalf("reply-note = %+v", b)
				}
			}
		})
	}
	// The logo sits inside Confirmation's panel.
	got, err := ApplyBrand(layoutOrFail(t, "builtin:confirmation"), kit)
	if err != nil {
		t.Fatal(err)
	}
	if b := findBlock(got.Blocks, "logo"); b == nil || b.Src != kit.Logo.Src {
		t.Fatalf("nested logo = %+v", b)
	}
	// The layout it was given is not changed.
	if findBlock(layoutOrFail(t, "builtin:letter").Blocks, "logo").Src != "" {
		t.Fatal("layout was changed")
	}
}

func TestApplyBrandRemovesEmptySlots(t *testing.T) {
	kit := testKit(t)
	kit.Signature, kit.Reply, kit.Social, kit.Follow = nil, nil, nil, ""
	for _, info := range Layouts() {
		got, err := ApplyBrand(info.Document, kit)
		if err != nil {
			t.Fatalf("%s: %v", info.ID, err)
		}
		for _, id := range []string{"signature", "signature-photo", "signature-name", "signature-title", "reply", "reply-heading", "reply-button", "reply-note", "social", "follow"} {
			if hasID(got.Blocks, id) {
				t.Fatalf("%s still has %s", info.ID, id)
			}
		}
		if !hasID(got.Blocks, "footer") {
			t.Fatalf("%s lost its footer", info.ID)
		}
	}
}

func TestApplyBrandSignatureNameAsText(t *testing.T) {
	kit := testKit(t)
	kit.Signature.NameImage, kit.Signature.NameText = "", "Sam Rivera"
	got, err := ApplyBrand(layoutOrFail(t, "builtin:letter"), kit)
	if err != nil {
		t.Fatal(err)
	}
	b := findBlock(got.Blocks, "signature-name")
	if b == nil || b.Type != "text" || b.Text != "Sam Rivera" || b.Font != "heading" {
		t.Fatalf("signature-name = %+v", b)
	}
	// It stays in the second column, above the title.
	band := findBlock(got.Blocks, "signature")
	if len(band.Columns) != 2 || band.Columns[1].Blocks[1].ID != "signature-name" {
		t.Fatalf("position lost: %+v", band.Columns)
	}
}

func TestApplyBrandNeverTouchesContent(t *testing.T) {
	kit := testKit(t)
	for _, info := range Layouts() {
		got, err := ApplyBrand(info.Document, kit)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ContentSlots(info.Document), ContentSlots(got)) {
			t.Fatalf("%s: content changed", info.ID)
		}
	}
}

func TestBrandWarningsReportsLowContrastWithoutRefusing(t *testing.T) {
	kit := testKit(t)
	kit.Theme.Secondary = "#55b9bd"
	raw, _ := kit.JSON()
	parsed, err := ParseBrandKit(raw)
	if err != nil {
		t.Fatalf("a low-contrast kit must parse: %v", err)
	}
	var contrast []BrandWarning
	for _, w := range BrandWarnings(context.Background(), parsed) {
		if w.Code == "contrast" {
			contrast = append(contrast, w)
		}
	}
	if len(contrast) == 0 {
		t.Fatal("no contrast warning")
	}
	seen := map[string]bool{}
	for _, w := range contrast {
		if seen[w.Message] {
			t.Fatalf("duplicate warning %q", w.Message)
		}
		seen[w.Message] = true
		if !strings.HasPrefix(w.Message, "Text in #") || !strings.Contains(w.Message, "to 1; the minimum here is") {
			t.Fatalf("message = %q", w.Message)
		}
	}
	if w := BrandWarnings(context.Background(), testKit(t)); len(w) != 0 {
		t.Fatalf("a readable kit warned: %+v", w)
	}
}

func TestBrandWarningsLargePictures(t *testing.T) {
	kit := testKit(t)
	kit.Logo.Src = testPNG(t, 300, true)        // about 270 KiB
	kit.Signature.Photo = testPNG(t, 301, true) // a second, different one
	kit.Social[1].Icon = kit.Logo.Src           // the same picture twice counts once
	var large []BrandWarning
	for _, w := range BrandWarnings(context.Background(), kit) {
		if w.Code == "pictures_large" {
			large = append(large, w)
		}
	}
	if len(large) != 1 || !strings.Contains(large[0].Message, "512 KiB") {
		t.Fatalf("warnings = %+v", large)
	}
	kit.Signature.Photo = testPNG(t, 5, false)
	for _, w := range BrandWarnings(context.Background(), kit) {
		if w.Code == "pictures_large" {
			t.Fatalf("one picture is under the limit: %+v", w)
		}
	}
}
