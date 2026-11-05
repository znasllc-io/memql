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
	kit.Logo.OnColour, kit.Logo.Wide = testRectPNG(t, 30, 17), testRectPNG(t, 120, 31)
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
	// The logo's keys come in the format's order: src, onColour, wide, alt, href.
	last := -1
	for _, key := range []string{`"src": "data:`, `"onColour": "data:`, `"wide": "data:`, `"alt": "Northwind"`, `"href": "https://northwind.example"`} {
		at := strings.Index(string(first), key)
		if at <= last {
			t.Fatalf("logo key %s is missing or out of order:\n%.300s", key, first)
		}
		last = at
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
	manyLinks := func(n int, label bool) []Link {
		links := make([]Link, n)
		for i := range links {
			links[i] = Link{Network: "Mastodon", Href: "https://social.example/northwind"}
			if label {
				links[i] = Link{Label: "More", Href: "https://northwind.example/more"}
			}
		}
		return links
	}
	cases := []struct {
		name   string
		mutate func(k *BrandKit)
		want   string
	}{
		{"logo src required", func(k *BrandKit) { k.Logo.Src = "" }, "logo.src is required"},
		{"logo alt required", func(k *BrandKit) { k.Logo.Alt = "" }, "logo.alt is required"},
		{"link tag in social", func(k *BrandKit) { k.Social = []Link{{Network: "X", Href: TagViewURL}} }, "footer link"},
		{"link tag on logo", func(k *BrandKit) { k.Logo.Href = TagViewURL }, "footer link"},
		{"nine social", func(k *BrandKit) { k.Social = manyLinks(9, false) }, "at most 8 social links"},
		{"nine footer links", func(k *BrandKit) {
			k.Footer.Links = append(manyLinks(8, true), Link{Label: "Unsubscribe", Href: TagUnsubscribeURL})
		}, "at most 8 links"},
		{"long follow", func(k *BrandKit) { k.Follow = strings.Repeat("a", 201) }, "follow can be at most 200"},
		{"thirteen lines", func(k *BrandKit) { k.Footer.Lines = make([]string, 13) }, "at most 12 lines"},
		{"long line", func(k *BrandKit) { k.Footer.Lines = []string{strings.Repeat("a", 301)} }, "longer than 300"},
		{"reply without label", func(k *BrandKit) { k.Reply.ButtonLabel = "" }, "reply needs a heading"},
		{"web picture", func(k *BrandKit) { k.Signature.Photo = "https://x.example/p.png" }, "embedded PNG"},
		{"pictures too big together", func(k *BrandKit) {
			k.Logo.Src = testPNG(t, 520, true)
			k.Signature.Photo = testPNG(t, 521, true)
			k.Signature.NameImage = testPNG(t, 522, true)
		}, "Use fewer or smaller pictures"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := good
			sig, rep := *good.Signature, *good.Reply
			k.Signature, k.Reply = &sig, &rep
			c.mutate(&k)
			raw, err := k.JSON()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseBrandKit(raw); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
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
	old := findBlock(layoutOrFail(t, "builtin:letter").Blocks, "signature-name")
	if !reflect.DeepEqual(b.Padding, old.Padding) || (old.Align != "" && b.Align != old.Align) {
		t.Fatalf("padding/align not kept: %+v vs %+v", b, old)
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

func TestApplyBrandPartialSignaturesStayValid(t *testing.T) {
	base := testKit(t)
	big := testPNG(t, 520, true) // about 780 KiB, under the 1 MiB limit
	name, photo, title := base.Signature.NameImage, base.Signature.Photo, "Founder"
	sigs := map[string]*BrandSignature{
		"photo only":      {Photo: photo},
		"name only":       {NameImage: name},
		"text name only":  {NameText: "Sam Rivera"},
		"title only":      {Title: title},
		"name and title":  {NameImage: name, Title: title},
		"photo and title": {Photo: photo, Title: title},
		"photo and name":  {Photo: photo, NameText: "Sam"},
		"empty":           {},
		"big pictures":    {Photo: testPNG(t, 300, true), NameImage: name, Title: title},
	}
	for label, sig := range sigs {
		for _, info := range Layouts() {
			t.Run(label+"/"+info.ID, func(t *testing.T) {
				kit := base
				kit.Signature = sig
				kit.Logo.Src = big
				if _, err := ApplyBrand(info.Document, kit); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	// A photo-only signature gives the whole band to the photo's column.
	kit := base
	kit.Signature = &BrandSignature{Photo: photo}
	got, err := ApplyBrand(layoutOrFail(t, "builtin:letter"), kit)
	if err != nil {
		t.Fatal(err)
	}
	if band := findBlock(got.Blocks, "signature"); len(band.Columns) != 1 || band.Columns[0].Width < 95 {
		t.Fatalf("columns = %+v", band.Columns)
	}
	kit.Signature = &BrandSignature{}
	got, _ = ApplyBrand(layoutOrFail(t, "builtin:letter"), kit)
	if hasID(got.Blocks, "signature") {
		t.Fatal("an empty signature should be removed")
	}
}

// testRectPNG is a solid PNG of w by h pixels: a picture with a shape of its own.
func testRectPNG(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 90, B: 40, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// compactSignatureLayouts are the built-in layouts whose signature slot has no
// background of its own: the compact form, which a whole picture can fill.
var compactSignatureLayouts = []string{"builtin:article", "builtin:three-reasons", "builtin:side-by-side"}

func TestApplyBrandCompactSignatureUsesWholePicture(t *testing.T) {
	picture := testRectPNG(t, 300, 90) // 10 to 3
	kit := testKit(t)
	kit.Signature.Picture = picture
	for _, id := range compactSignatureLayouts {
		t.Run(id, func(t *testing.T) {
			got, err := ApplyBrand(layoutOrFail(t, id), kit)
			if err != nil {
				t.Fatal(err)
			}
			band := findBlock(got.Blocks, SlotSignature)
			if band == nil || band.Type != typeColumns || band.Role != roleBrand || band.Inset != nil || band.MaxWidth != 0 ||
				len(band.Columns) != 1 || band.Columns[0].Width != 100 || len(band.Columns[0].Blocks) != 1 {
				t.Fatalf("want the slot holding one column of the whole width, got %+v", band)
			}
			// The whole picture at the slot's full content width (the email's
			// 552 pixels inside the slot's padding), as tall as its shape makes
			// it, and named for the title.
			want := Block{ID: "signature-picture", Type: typeImage, Src: picture, Alt: "Founder of Northwind", Width: 552, Height: 166}
			if pic := band.Columns[0].Blocks[0]; !reflect.DeepEqual(pic, want) {
				t.Fatalf("picture block:\n got %+v\nwant %+v", pic, want)
			}
			for _, part := range []string{"signature-photo", "signature-space", "signature-name", "signature-title"} {
				if hasID(got.Blocks, part) {
					t.Errorf("the part %s stayed beside the whole picture", part)
				}
			}
			r, err := Render(context.Background(), got, nil, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(r.HTML, `<img src="`+picture+`" width="552" height="166" alt="Founder of Northwind"`) {
				t.Fatal("the picture is not drawn 552 pixels wide and 166 high")
			}
			for _, issue := range r.Issues {
				if issue.Block == "signature-picture" {
					t.Errorf("the signature picture is reported: %+v", issue)
				}
			}
		})
	}
	// The picture is named for the title, else the name written as text,
	// else "Signature".
	for _, c := range []struct {
		sig  BrandSignature
		want string
	}{
		{BrandSignature{Picture: picture, NameText: "Sam Rivera", Title: "Founder"}, "Founder"},
		{BrandSignature{Picture: picture, NameText: "Sam Rivera"}, "Sam Rivera"},
		{BrandSignature{Picture: picture, NameImage: testPNG(t, 6, false)}, "Signature"},
		{BrandSignature{Picture: picture}, "Signature"},
	} {
		k := testKit(t)
		sig := c.sig
		k.Signature = &sig
		got, err := ApplyBrand(layoutOrFail(t, "builtin:article"), k)
		if err != nil {
			t.Fatal(err)
		}
		if b := findBlock(got.Blocks, "signature-picture"); b == nil || b.Alt != c.want {
			t.Errorf("%+v: want the alt %q, got %+v", c.sig, c.want, b)
		}
	}
	// Without a picture nothing changes: the slot keeps its parts.
	plain, err := ApplyBrand(layoutOrFail(t, "builtin:article"), testKit(t))
	if err != nil {
		t.Fatal(err)
	}
	if hasID(plain.Blocks, "signature-picture") || findBlock(plain.Blocks, "signature-photo") == nil || findBlock(plain.Blocks, SlotSignature).Inset == nil {
		t.Fatal("a kit without a picture must keep the parts and the slot as the layout has them")
	}
}

func TestApplyBrandBandSignatureKeepsParts(t *testing.T) {
	kit := testKit(t)
	kit.Signature.Picture = testRectPNG(t, 300, 90)
	got, err := ApplyBrand(layoutOrFail(t, "builtin:letter"), kit)
	if err != nil {
		t.Fatal(err)
	}
	if hasID(got.Blocks, "signature-picture") {
		t.Fatal("a band, a signature slot with a background of its own, must not take the whole picture")
	}
	band := findBlock(got.Blocks, SlotSignature)
	if band == nil || band.Background != "secondary" || len(band.Columns) != 2 {
		t.Fatalf("band = %+v", band)
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
	// A kit with the whole picture and no parts leaves a band nothing to show.
	kit.Signature = &BrandSignature{Picture: testRectPNG(t, 300, 90)}
	if got, err = ApplyBrand(layoutOrFail(t, "builtin:letter"), kit); err != nil {
		t.Fatal(err)
	}
	if hasID(got.Blocks, SlotSignature) {
		t.Fatal("a band without parts to show must be removed")
	}
}

func TestParseBrandKitSignaturePictureRules(t *testing.T) {
	picture := testRectPNG(t, 300, 90)
	base := testKit(t)
	// The whole picture alone is a signature, and beside the parts it is kept
	// with them; both survive the kit's JSON.
	for _, sig := range []BrandSignature{
		{Picture: picture},
		{Photo: base.Signature.Photo, NameImage: base.Signature.NameImage, Title: "Founder", Picture: picture},
	} {
		k := base
		s := sig
		k.Signature = &s
		raw, err := k.JSON()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"picture": "data:image/png;base64,`) {
			t.Fatalf("the picture is not written as signature.picture:\n%.400s", raw)
		}
		back, err := ParseBrandKit(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*back.Signature, sig) {
			t.Fatalf("round trip: got %+v, want %+v", *back.Signature, sig)
		}
	}
	// It is held to the rules of every kit picture.
	for _, c := range []struct{ name, src, want string }{
		{"a Library picture", "artifact:0123456789abcdef0123456789abcdef", "signature picture: Upload pictures through the Brand page"},
		{"a web address", "https://x.example/sign.png", "signature picture: "},
		{"not a raster", "data:image/svg+xml;base64,PHN2Zz4=", "signature picture: "},
		{"larger than 1 MiB", testPNG(t, 600, true), "signature picture: email image exceeds 1 MiB"},
	} {
		k := base
		k.Signature = &BrandSignature{Picture: c.src}
		raw, err := k.JSON()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseBrandKit(raw); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error containing %q, got %v", c.name, c.want, err)
		}
	}
	// It counts toward the kit's pictures: the size a document can carry,
	// and the warning that leaves an email little room.
	big := base
	big.Logo.Src = testPNG(t, 520, true)
	big.Signature = &BrandSignature{Picture: testPNG(t, 521, true)}
	raw, err := big.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseBrandKit(raw); err == nil || !strings.Contains(err.Error(), "Use fewer or smaller pictures") {
		t.Fatalf("two large pictures, the signature's one of them: %v", err)
	}
	warned := base
	warned.Signature = &BrandSignature{Picture: testPNG(t, 300, true)} // about 270 KiB
	warned.Logo.Src = testPNG(t, 301, true)
	large := 0
	for _, w := range BrandWarnings(context.Background(), warned) {
		if w.Code == "pictures_large" {
			large++
		}
	}
	if large != 1 {
		t.Fatalf("the signature picture must count toward pictures_large: %d warnings", large)
	}
}

// TestParseBrandKitFooterLinesMayBreak pins that an entry of a kit's footer
// lines is a paragraph that may hold line breaks, the 300 characters counted
// per entry, and that applied to a layout each entry stays one entry.
func TestParseBrandKitFooterLinesMayBreak(t *testing.T) {
	kit := testKit(t)
	address := "NORTHWIND\n1 Harbour Road\n" + strings.Repeat("a", 300-len("NORTHWIND\n1 Harbour Road\n"))
	kit.Footer.Lines = []string{address, "Privacy"}
	raw, err := kit.JSON()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseBrandKit(raw)
	if err != nil {
		t.Fatalf("an entry of 300 characters with line breaks: %v", err)
	}
	got, err := ApplyBrand(layoutOrFail(t, "builtin:lesson"), back)
	if err != nil {
		t.Fatal(err)
	}
	if b := findBlock(got.Blocks, SlotFooter); b == nil || !reflect.DeepEqual(b.Lines, []string{address, "Privacy"}) {
		t.Fatalf("footer lines = %+v", b)
	}
	kit.Footer.Lines = []string{address + "a"}
	raw, _ = kit.JSON()
	if _, err := ParseBrandKit(raw); err == nil || !strings.Contains(err.Error(), "footer: line 1 is longer than 300 characters") {
		t.Fatalf("301 characters in one entry: %v", err)
	}
}

// TestBrandWarningsFlagsALightSecondaryOnFooterLinks pins that a kit whose
// secondary colour is too light for text on white is warned about where the
// layouts draw text in it: the footer's links (14.7 pixels, which need 4.5 to
// 1) and the reply's heading (21.3 pixels and bold, large text, which needs 3
// to 1). The kit is accepted all the same: contrast warns, never refuses.
func TestBrandWarningsFlagsALightSecondaryOnFooterLinks(t *testing.T) {
	const light, white = "#d9eef0", "#ffffff"
	kit := testKit(t)
	kit.Theme.Secondary, kit.Theme.Canvas = light, white
	raw, err := kit.JSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseBrandKit(raw)
	if err != nil {
		t.Fatalf("a kit with a light secondary must be accepted: %v", err)
	}
	// The runs the warnings are made of: in each layout with a reply, the
	// footer's links and the reply's heading are read in the secondary on white.
	for _, id := range compactSignatureLayouts {
		doc, err := ApplyBrand(layoutOrFail(t, id), parsed)
		if err != nil {
			t.Fatal(err)
		}
		r, err := Render(context.Background(), doc, nil, RenderOptions{Preview: true})
		if err != nil {
			t.Fatal(err)
		}
		runs, err := TextRuns(r.HTML)
		if err != nil {
			t.Fatal(err)
		}
		links, heading := 0, 0
		for _, run := range runs {
			if run.Foreground != light || run.Background != white {
				continue
			}
			switch {
			case run.Block == SlotFooter && !run.Large():
				links++
			case run.Block == "reply-heading" && run.Large():
				heading++
			}
		}
		if links < len(kit.Footer.Links) || heading == 0 {
			t.Errorf("%s: want every footer link and the reply heading read in %s on %s, got %d link runs and %d heading runs", id, light, white, links, heading)
		}
	}
	// The warnings name the secondary: as text, for the footer's links, and as
	// large text, for the reply's heading.
	want := map[string]bool{"the minimum here is 4.5 to 1.": false, "the minimum here is 3 to 1.": false}
	for _, w := range BrandWarnings(context.Background(), parsed) {
		if w.Code != "contrast" || !strings.HasPrefix(w.Message, "Text in "+light+" on "+white+" has a contrast of 1.2 to 1;") {
			continue
		}
		for end := range want {
			if strings.HasSuffix(w.Message, end) {
				want[end] = true
			}
		}
	}
	for end, found := range want {
		if !found {
			t.Errorf("no contrast warning for text in %s on %s ending %q", light, white, end)
		}
	}
}

// logoVariantKit is the test kit with a reversed logo for coloured panels and a
// wide one for headers, each a picture of its own.
func logoVariantKit(t *testing.T) BrandKit {
	t.Helper()
	kit := testKit(t)
	kit.Logo.OnColour, kit.Logo.Wide = testRectPNG(t, 30, 17), testRectPNG(t, 120, 31)
	return kit
}

// logoIn is a layout whose logo sits where the blocks put it, with a footer.
func logoIn(t *testing.T, blocks string) Document {
	t.Helper()
	d, err := Parse([]byte(layoutJSON(blocks + `,{"id":"footer","type":"footer","role":"brand","links":[{"label":"Unsubscribe","href":"{{unsubscribeUrl}}"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// appliedLogo is the logo block of layout with kit applied.
func appliedLogo(t *testing.T, layout Document, kit BrandKit) Block {
	t.Helper()
	got, err := ApplyBrand(layout, kit)
	if err != nil {
		t.Fatal(err)
	}
	b := findBlock(got.Blocks, SlotLogo)
	if b == nil {
		t.Fatal("no logo")
	}
	return *b
}

const logoBlock = `{"id":"logo","type":"image","role":"brand","alt":"Logo","width":200,"height":50}`

func TestApplyBrandUsesOnColourLogoOnAColouredPanel(t *testing.T) {
	kit := logoVariantKit(t)
	// Confirmation's logo sits in its panel, a section on the secondary colour.
	logo := appliedLogo(t, layoutOrFail(t, "builtin:confirmation"), kit)
	if logo.Src != kit.Logo.OnColour || logo.Alt != kit.Logo.Alt || logo.Href != kit.Logo.Href {
		t.Fatalf("confirmation logo = %+v, want the reversed logo with the kit's alt and link", logo)
	}
	// Any enclosing section or columns on a colour that is not canvas, white or
	// page, however deep; none of those three, and no container, is not coloured.
	for _, c := range []struct {
		name, blocks string
		reversed     bool
	}{
		{"top level", logoBlock, false},
		{"section of canvas", `{"id":"s","type":"section","background":"canvas","blocks":[` + logoBlock + `]}`, false},
		{"section of white", `{"id":"s","type":"section","background":"white","blocks":[` + logoBlock + `]}`, false},
		{"section of page", `{"id":"s","type":"section","background":"page","blocks":[` + logoBlock + `]}`, false},
		{"section without a background", `{"id":"s","type":"section","blocks":[` + logoBlock + `]}`, false},
		{"section of primary", `{"id":"s","type":"section","background":"primary","blocks":[` + logoBlock + `]}`, true},
		{"columns of accent", `{"id":"c","type":"columns","background":"accent","columns":[{"width":100,"blocks":[` + logoBlock + `]}]}`, true},
		{"plain columns in a coloured section", `{"id":"s","type":"section","background":"secondary","blocks":[{"id":"c","type":"columns","columns":[{"width":100,"blocks":[` + logoBlock + `]}]}]}`, true},
	} {
		want := kit.Logo.Src
		if c.reversed {
			want = kit.Logo.OnColour
		}
		if logo := appliedLogo(t, logoIn(t, c.blocks), kit); logo.Src != want {
			t.Errorf("%s: want the %s logo", c.name, map[bool]string{true: "reversed", false: "standard"}[c.reversed])
		}
	}
	// Letter's logo sits on the canvas: the standard logo.
	if logo := appliedLogo(t, layoutOrFail(t, "builtin:letter"), kit); logo.Src != kit.Logo.Src {
		t.Fatal("letter: want the standard logo")
	}
}

func TestApplyBrandUsesWideLogoForABleedHeader(t *testing.T) {
	kit := logoVariantKit(t)
	for _, id := range []string{"builtin:article", "builtin:lesson", "builtin:three-reasons", "builtin:side-by-side", "builtin:spotlight"} {
		logo := appliedLogo(t, layoutOrFail(t, id), kit)
		if !logo.Bleed || logo.Src != kit.Logo.Wide || logo.Alt != kit.Logo.Alt || logo.Href != kit.Logo.Href {
			t.Errorf("%s: logo = %+v, want the wide logo with the kit's alt and link", id, logo)
		}
	}
	// A bleed logo takes the wide one even on a coloured panel.
	bleed := `{"id":"s","type":"section","background":"secondary","blocks":[{"id":"logo","type":"image","role":"brand","alt":"Logo","width":600,"height":155,"bleed":true}]}`
	if logo := appliedLogo(t, logoIn(t, bleed), kit); logo.Src != kit.Logo.Wide {
		t.Fatal("a bleed logo on a coloured panel: want the wide logo")
	}
}

func TestApplyBrandLogoVariantsFallBackToTheLogo(t *testing.T) {
	plain := testKit(t)
	for _, info := range Layouts() {
		if logo := appliedLogo(t, info.Document, plain); logo.Src != plain.Logo.Src {
			t.Errorf("%s: a kit without variants must give its logo", info.ID)
		}
	}
	bleed := `{"id":"s","type":"section","background":"secondary","blocks":[{"id":"logo","type":"image","role":"brand","alt":"Logo","width":600,"height":155,"bleed":true}]}`
	onlyWide := testKit(t)
	onlyWide.Logo.Wide = testRectPNG(t, 120, 31)
	onlyReversed := testKit(t)
	onlyReversed.Logo.OnColour = testRectPNG(t, 30, 17)
	for _, c := range []struct {
		name   string
		layout Document
		kit    BrandKit
		want   string
	}{
		{"a coloured panel without a reversed logo", layoutOrFail(t, "builtin:confirmation"), onlyWide, onlyWide.Logo.Src},
		{"a bleed header without a wide logo", layoutOrFail(t, "builtin:article"), onlyReversed, onlyReversed.Logo.Src},
		{"a bleed header on a coloured panel without a wide logo", logoIn(t, bleed), onlyReversed, onlyReversed.Logo.Src},
		{"the standard place with both variants", layoutOrFail(t, "builtin:letter"), logoVariantKit(t), testKit(t).Logo.Src},
	} {
		if logo := appliedLogo(t, c.layout, c.kit); logo.Src != c.want {
			t.Errorf("%s: want the kit's logo", c.name)
		}
	}
}

func TestParseBrandKitLogoVariantRules(t *testing.T) {
	base := logoVariantKit(t)
	raw, err := base.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if back, err := ParseBrandKit(raw); err != nil || back.Logo != base.Logo {
		t.Fatalf("a kit with both variants: %v", err)
	}
	// Each variant is held to the rules of every kit picture.
	for _, field := range []string{"onColour", "wide"} {
		for _, c := range []struct{ name, src, want string }{
			{"a Library picture", "artifact:0123456789abcdef0123456789abcdef", "Upload pictures through the Brand page"},
			{"a web address", "https://x.example/logo.png", "a picture must be a Library file (artifact:) or an embedded PNG, JPEG or GIF"},
			{"not a raster", "data:image/svg+xml;base64,PHN2Zz4=", ""},
			{"larger than 1 MiB", testPNG(t, 600, true), "email image exceeds 1 MiB"},
		} {
			k := base
			if field == "onColour" {
				k.Logo.OnColour = c.src
			} else {
				k.Logo.Wide = c.src
			}
			raw, err := k.JSON()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseBrandKit(raw); err == nil || !strings.Contains(err.Error(), "logo."+field+": "+c.want) {
				t.Errorf("%s %s: want an error with %q, got %v", field, c.name, "logo."+field+": "+c.want, err)
			}
		}
	}
	// Both count toward the kit's pictures: the size a document can carry,
	// and the warning that leaves an email little room.
	big := testKit(t)
	big.Logo.Src, big.Logo.Wide = testPNG(t, 520, true), testPNG(t, 521, true)
	raw, _ = big.JSON()
	if _, err := ParseBrandKit(raw); err == nil || !strings.Contains(err.Error(), "Use fewer or smaller pictures") {
		t.Fatalf("a large logo and a large wide one: %v", err)
	}
	warned := testKit(t)
	warned.Logo.OnColour = testPNG(t, 300, true) // about 270 KiB
	warned.Logo.Wide = testPNG(t, 301, true)
	large := 0
	for _, w := range BrandWarnings(context.Background(), warned) {
		if w.Code == "pictures_large" {
			large++
		}
	}
	if large != 1 {
		t.Fatalf("the logo variants must count toward pictures_large: %d warnings", large)
	}
}
