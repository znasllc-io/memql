package emaildoc

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// tinyPNG encodes a solid w x h PNG with image/png. It is written without
// compression so its size is known, about 3 x w x h bytes, which the tests of
// the picture budget rely on.
func tinyPNG(t testing.TB, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 0x88, G: 0x99, B: 0xaa, A: 0xff})
		}
	}
	var buf bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	if err := encoder.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func hasIssue(issues []Issue, code string) bool {
	for _, issue := range issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func countIssues(issues []Issue, code string) int {
	n := 0
	for _, issue := range issues {
		if issue.Code == code {
			n++
		}
	}
	return n
}

// neutral is the neutral theme of parse_test.go's neutralTheme, as a value.
func neutral() Theme {
	return Theme{
		Page: "#eef0f3", Canvas: "#ffffff", Primary: "#1f5f8b", Secondary: "#5b8fb9", Accent: "#d9534f",
		Tint: "#f6f4ef", Text: "#2b2b2b", Muted: "#8a8f98", HeadingFont: "Georgia", BodyFont: "Arial",
	}
}

// oneBlock is a layout of b followed by a legal line that carries the
// unsubscribe link, so no test meets unsubscribe_missing by accident.
func oneBlock(b Block) Document {
	return Document{
		Subject: "Hello",
		Theme:   neutral(),
		Blocks:  []Block{b, {ID: "legal", Type: "text", Text: "[Unsubscribe]({{unsubscribeUrl}})"}},
	}
}

type fakeImages map[string]Image

func (f fakeImages) ResolveImage(_ context.Context, id string) (Image, error) {
	if img, ok := f[id]; ok {
		return img, nil
	}
	return Image{}, errors.New("not readable")
}

func pngImage(t testing.TB, w, h int) Image {
	return Image{MIMEType: "image/png", Data: tinyPNG(t, w, h)}
}

func dataURI(data []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
}

func TestRenderSkeletonUsesTheExamplesTechnique(t *testing.T) {
	r, err := Render(context.Background(), oneBlock(Block{ID: "h", Type: "heading", Text: "Hi"}), nil, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN"`,
		`@media (max-width:599px){.ecw{width:100%!important;min-width:0!important}}`,
		`class="ecw" style="max-width:600px;min-height:600px;margin:0 auto;background-color:#ffffff;width:600px;min-width:600px"`,
		`<!--[if mso]><center><table align="center" border="0" cellpadding="0" cellspacing="0" width="600">`,
		`background-color:#eef0f3;margin:0;padding:0"`,
	} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.HasSuffix(r.HTML, "</body></html>") || strings.Count(r.HTML, "</body>") != 1 || strings.Contains(r.HTML, "\n") {
		t.Fatal("skeleton must end once with </body></html> and carry no newlines")
	}
}

func TestPreviewMarksBlocksAndSendDoesNot(t *testing.T) {
	d := oneBlock(Block{ID: "headline", Type: "heading", Text: "Hi"})
	p, _ := Render(context.Background(), d, nil, RenderOptions{Preview: true})
	s, _ := Render(context.Background(), d, nil, RenderOptions{})
	if !strings.Contains(p.HTML, `data-block="headline"`) || strings.Contains(s.HTML, "data-block") {
		t.Fatal("data-block belongs to previews only")
	}
}

func TestPreviewMarksEveryLeafOnItsOuterCell(t *testing.T) {
	images := fakeImages{"a1": pngImage(t, 2, 2)}
	d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{
		{ID: "pic", Type: "image", Src: "artifact:a1", Width: 100},
		{ID: "head", Type: "heading", Text: "H"},
		{ID: "body", Type: "text", Text: "T"},
		{ID: "band", Type: "highlight", Text: "B"},
		{ID: "go", Type: "button", Label: "Go", Href: "https://example.com"},
		{ID: "rule", Type: "divider"},
		{ID: "gap", Type: "spacer", Height: 8},
		{ID: "raw", Type: "html", HTML: "<p>x</p>"},
	}}
	r, err := Render(context.Background(), d, images, RenderOptions{Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<tr><td data-block="pic" style="padding:`,
		`<tr><td data-block="head" dir="ltr"`,
		`<tr><td data-block="body" style="padding:`,
		`<tr><td data-block="band" style="padding:`,
		`<tr><td data-block="go" style="padding:`,
		`<tr><td data-block="rule" style="padding:`,
		`<tr><td data-block="gap" style="height:8px;`,
		`<tr><td data-block="raw" style="padding:`,
	} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("missing %q", want)
		}
	}
	if got := strings.Count(r.HTML, "data-block="); got != 8 {
		t.Errorf("want 8 marked cells, one per block, got %d", got)
	}
}

func TestRenderEscapesUntrustedText(t *testing.T) {
	d := oneBlock(Block{ID: "h", Type: "heading", Text: `<script>alert(1)</script> & "q"`})
	r, _ := Render(context.Background(), d, nil, RenderOptions{Sample: map[string]string{"displayName": "Ada & <Bo>"}})
	if strings.Contains(r.HTML, "<script>") || !strings.Contains(r.HTML, "&lt;script&gt;") {
		t.Fatal("heading text reached HTML unescaped")
	}
}

func TestRenderEscapesEveryTextField(t *testing.T) {
	const hostile = `<b>"x" & 'y'</b>`
	const escaped = `&lt;b&gt;&#34;x&#34; &amp; &#39;y&#39;&lt;/b&gt;`
	for _, b := range []Block{
		{ID: "a", Type: "heading", Text: hostile},
		{ID: "a", Type: "text", Text: hostile},
		{ID: "a", Type: "text", Text: "- " + hostile},
		{ID: "a", Type: "text", Text: "[" + hostile + "](https://example.com)"},
		{ID: "a", Type: "highlight", Text: hostile},
		{ID: "a", Type: "button", Label: hostile, Href: "https://example.com"},
		{ID: "a", Type: "image", Width: 100, Alt: hostile},
	} {
		t.Run(b.Type, func(t *testing.T) {
			r, err := Render(context.Background(), oneBlock(b), nil, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(r.HTML, "<b>") || !strings.Contains(r.HTML, escaped) {
				t.Fatalf("text reached the HTML unescaped:\n%s", r.HTML)
			}
		})
	}
	// An alt is an attribute as well as the placeholder's text.
	d := oneBlock(Block{ID: "i", Type: "image", Src: "artifact:a1", Width: 100, Alt: hostile})
	r, _ := Render(context.Background(), d, fakeImages{"a1": pngImage(t, 2, 2)}, RenderOptions{})
	if !strings.Contains(r.HTML, `alt="`+escaped+`"`) {
		t.Fatalf("alt reached the attribute unescaped:\n%s", r.HTML)
	}
}

func TestSampleValuesAreEscapedInHTMLAndRawInText(t *testing.T) {
	d := oneBlock(Block{ID: "t", Type: "text", Text: "Hi {{displayName}} from {{fields.company}}, [stop]({{unsubscribeUrl}})"})
	d.Subject = "For {{displayName}}"
	r, _ := Render(context.Background(), d, nil, RenderOptions{Sample: map[string]string{"displayName": "Ada & <Bo>", "fields.company": "Acme"}})
	if !strings.Contains(r.HTML, "Hi Ada &amp; &lt;Bo&gt; from Acme") || !strings.Contains(r.Text, "Hi Ada & <Bo> from Acme") || r.Subject != "For Ada & <Bo>" {
		t.Fatalf("sample substitution wrong:\n%s\n%s", r.HTML, r.Text)
	}
	if !strings.Contains(r.HTML, `href="{{unsubscribeUrl}}"`) {
		t.Fatal("link tags stay literal in previews")
	}
}

func TestSampleFillsOnlyTheMergeTagsItKnows(t *testing.T) {
	d := oneBlock(Block{ID: "t", Type: "text", Text: "{{displayName}}|{{email}}|{{accountName}}|{{campaignName}}|{{fields.plan}}|{{fields.other}}|{{unknown}}|{{fields.a}} {{viewUrl}}|[v]({{viewUrl}})"})
	d.Preheader = "Dear {{displayName}}"
	r, err := Render(context.Background(), d, nil, RenderOptions{Sample: map[string]string{
		"displayName": "Ada", "email": "ada@example.com", "accountName": "Org", "campaignName": "Spring", "fields.plan": "Pro",
		// None of these is a sample key: an unknown tag, a link tag, a key
		// in the wrong case, an empty field name, and a field name that
		// would swallow a link tag.
		"unknown": "U", "viewUrl": "V", "unsubscribeUrl": "W", "Email": "E", "fields.": "F", "fields.a}} {{viewUrl": "S",
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := "Ada|ada@example.com|Org|Spring|Pro|{{fields.other}}|{{unknown}}|{{fields.a}} {{viewUrl}}|"
	if !strings.Contains(r.HTML, want) || !strings.Contains(r.Text, want) {
		t.Fatalf("want %q in both parts:\n%s\n%s", want, r.HTML, r.Text)
	}
	if !strings.Contains(r.HTML, `href="{{viewUrl}}"`) || !strings.Contains(r.HTML, `href="{{unsubscribeUrl}}"`) {
		t.Fatal("link tags stay literal in previews")
	}
	if !strings.Contains(r.HTML, ">Dear Ada&#847;") {
		t.Fatal("the preheader is sampled with the rest of the HTML")
	}
}

func TestImagesResolveThroughTheResolverAtTheirNaturalRatio(t *testing.T) {
	d := oneBlock(Block{ID: "i", Type: "image", Src: "artifact:a1", Alt: "Banner", Width: 200})
	r, _ := Render(context.Background(), d, fakeImages{"a1": {MIMEType: "image/png", Data: tinyPNG(t, 20, 10)}}, RenderOptions{})
	if !strings.Contains(r.HTML, `src="data:image/png;base64,`) || !strings.Contains(r.HTML, `width="200" height="100"`) || len(r.Issues) != 0 {
		t.Fatalf("picture not embedded: %v", r.Issues)
	}
}

func TestImageHeightIsRoundedFromTheNaturalRatio(t *testing.T) {
	for _, c := range []struct {
		width, naturalW, naturalH int
		want                      string
	}{
		{200, 3, 2, `width="200" height="133"`}, // 133.33
		{100, 3, 2, `width="100" height="67"`},  // 66.67
		{101, 2, 1, `width="101" height="51"`},  // 50.5 rounds up
		{552, 4, 2, `width="552" height="276"`},
		{50, 7, 30, `width="50" height="214"`}, // 214.29
	} {
		d := oneBlock(Block{ID: "i", Type: "image", Src: "artifact:a1", Width: c.width})
		r, err := Render(context.Background(), d, fakeImages{"a1": pngImage(t, c.naturalW, c.naturalH)}, RenderOptions{})
		if err != nil || !strings.Contains(r.HTML, c.want) {
			t.Errorf("%d wide from %dx%d: want %s (%v)", c.width, c.naturalW, c.naturalH, c.want, err)
		}
	}
}

func TestImageIssues(t *testing.T) {
	ctx := context.Background()
	missing, _ := Render(ctx, oneBlock(Block{ID: "i", Type: "image", Alt: "Banner", Width: 200, Height: 80}), nil, RenderOptions{Preview: true})
	if len(missing.Issues) != 1 || missing.Issues[0] != (Issue{Code: IssueImageMissing, Block: "i", Message: `Choose a picture for "Banner".`}) || !strings.Contains(missing.HTML, "height:80px") {
		t.Fatalf("placeholder: %+v", missing.Issues)
	}
	unreadable, _ := Render(ctx, oneBlock(Block{ID: "i", Type: "image", Src: "artifact:nope", Width: 200}), fakeImages{}, RenderOptions{})
	if unreadable.Issues[0].Code != IssueImageUnavailable || unreadable.Issues[0].Message != `The picture for "i" could not be read. Choose it again.` {
		t.Fatalf("unreadable: %+v", unreadable.Issues)
	}
	images := fakeImages{}
	var blocks []Block
	for n := 0; n < 9; n++ {
		id := fmt.Sprintf("a%d", n)
		images[id] = Image{MIMEType: "image/png", Data: tinyPNG(t, 2+n, 2)}
		blocks = append(blocks, Block{ID: "i" + id, Type: "image", Src: "artifact:" + id, Width: 10})
	}
	d := oneBlock(blocks[0])
	d.Blocks = append(blocks, d.Blocks[1:]...)
	limit, _ := Render(ctx, d, images, RenderOptions{})
	if !hasIssue(limit.Issues, IssueImagesLimit) || limit.HTML == "" {
		t.Fatal("nine pictures must be an issue, and the render must still return HTML")
	}
	twice := oneBlock(Block{ID: "x", Type: "image", Src: "artifact:a1", Width: 10})
	twice.Blocks = append([]Block{{ID: "y", Type: "image", Src: "artifact:a1", Width: 10}}, twice.Blocks...)
	once, _ := Render(ctx, twice, fakeImages{"a1": {MIMEType: "image/png", Data: tinyPNG(t, 2, 2)}}, RenderOptions{})
	if len(once.Issues) != 0 {
		t.Fatal("the same picture twice counts once")
	}
}

func TestPicturesOverTheBudgetAreOneIssue(t *testing.T) {
	ctx := context.Background()
	images := fakeImages{}
	var blocks []Block
	for n := 0; n < 10; n++ {
		id := fmt.Sprintf("a%d", n)
		images[id] = pngImage(t, 2+n, 2)
		blocks = append(blocks, Block{ID: "i" + id, Type: "image", Src: "artifact:" + id, Width: 10})
	}
	d := oneBlock(blocks[0])
	d.Blocks = append(blocks, d.Blocks[1:]...)
	r, err := Render(ctx, d, images, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := Issue{Code: IssueImagesLimit, Block: "ia8", Message: "Use up to eight pictures totaling 1 MiB."}
	if len(r.Issues) != 1 || r.Issues[0] != want {
		t.Fatalf("ten pictures: want only %+v, got %+v", want, r.Issues)
	}
	// Rendering continues: every picture is still in the email.
	if got := strings.Count(r.HTML, `<img src="data:image/png;base64,`); got != 10 {
		t.Fatalf("want all 10 pictures embedded, got %d", got)
	}
	// Eight distinct pictures, used twice each, are within the budget.
	var reused []Block
	for n := 0; n < 16; n++ {
		reused = append(reused, Block{ID: fmt.Sprintf("r%d", n), Type: "image", Src: fmt.Sprintf("artifact:a%d", n%8), Width: 10})
	}
	d = oneBlock(reused[0])
	d.Blocks = append(reused, d.Blocks[1:]...)
	if r, _ := Render(ctx, d, images, RenderOptions{}); len(r.Issues) != 0 {
		t.Fatalf("eight distinct pictures: %+v", r.Issues)
	}
}

func TestPicturesOverAMebibyteInTotalAreAnIssue(t *testing.T) {
	// Each about 0.53 MiB; two of them make more than 1 MiB.
	big1, big2 := pngImage(t, 420, 440), pngImage(t, 440, 420)
	if n := len(big1.Data) + len(big2.Data); len(big1.Data) > MaxImageBytes || n <= MaxImageBytes {
		t.Fatalf("fixture sizes %d and %d do not straddle the budget", len(big1.Data), len(big2.Data))
	}
	d := oneBlock(Block{ID: "one", Type: "image", Src: "artifact:b1", Width: 100})
	d.Blocks = append([]Block{{ID: "two", Type: "image", Src: "artifact:b2", Width: 100}}, d.Blocks...)
	r, err := Render(context.Background(), d, fakeImages{"b1": big1, "b2": big2}, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := Issue{Code: IssueImagesLimit, Block: "one", Message: "Use up to eight pictures totaling 1 MiB."}
	if len(r.Issues) != 1 || r.Issues[0] != want {
		t.Fatalf("want only %+v, got %+v", want, r.Issues)
	}
	// One of them alone is within the budget.
	alone := oneBlock(Block{ID: "one", Type: "image", Src: "artifact:b1", Width: 100})
	if r, _ := Render(context.Background(), alone, fakeImages{"b1": big1}, RenderOptions{}); len(r.Issues) != 0 {
		t.Fatalf("one picture: %+v", r.Issues)
	}
}

func TestPicturesThatCannotBeSent(t *testing.T) {
	ctx := context.Background()
	huge := pngImage(t, 620, 600) // more than 1 MiB on its own
	if len(huge.Data) <= MaxImageBytes {
		t.Fatalf("fixture is only %d bytes", len(huge.Data))
	}
	jpeg := Image{MIMEType: "image/png", Data: encodedImage(t, "jpeg")}
	for _, c := range []struct {
		name   string
		images ImageResolver
		want   Issue
	}{
		{"no resolver", nil, Issue{Code: IssueImageUnavailable, Block: "i", Message: `The picture for "Banner" could not be read. Choose it again.`}},
		{"not a picture", fakeImages{"a1": {MIMEType: "image/png", Data: []byte("not a picture")}}, Issue{Code: IssueImageUnavailable, Block: "i", Message: `The picture for "Banner" could not be read. Choose it again.`}},
		{"empty", fakeImages{"a1": {MIMEType: "image/png"}}, Issue{Code: IssueImageUnavailable, Block: "i", Message: `The picture for "Banner" could not be read. Choose it again.`}},
		{"a type it is not", fakeImages{"a1": jpeg}, Issue{Code: IssueImageUnavailable, Block: "i", Message: `The picture for "Banner" could not be read. Choose it again.`}},
		{"an svg", fakeImages{"a1": {MIMEType: "image/svg+xml", Data: []byte("<svg/>")}}, Issue{Code: IssueImageUnavailable, Block: "i", Message: `The picture for "Banner" could not be read. Choose it again.`}},
		{"more than 1 MiB", fakeImages{"a1": huge}, Issue{Code: IssueImagesLimit, Block: "i", Message: "Use up to eight pictures totaling 1 MiB."}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := oneBlock(Block{ID: "i", Type: "image", Src: "artifact:a1", Alt: "Banner", Width: 200, Height: 90})
			r, err := Render(ctx, d, c.images, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Issues) != 1 || r.Issues[0] != c.want {
				t.Fatalf("want only %+v, got %+v", c.want, r.Issues)
			}
			// What cannot be sent is drawn as the placeholder.
			if strings.Contains(r.HTML, "<img") || !strings.Contains(r.HTML, "height:90px;") || !strings.Contains(r.HTML, ">Banner</div>") {
				t.Fatalf("want the placeholder:\n%s", r.HTML)
			}
		})
	}
}

func TestAPictureTooLargeToSendIsOneIssue(t *testing.T) {
	// Used twice, or after the budget is already exceeded, a picture over
	// 1 MiB adds no second images_limit issue.
	huge := pngImage(t, 620, 600)
	d := oneBlock(Block{ID: "two", Type: "image", Src: "artifact:huge", Width: 10})
	d.Blocks = append([]Block{{ID: "one", Type: "image", Src: "artifact:huge", Width: 10}}, d.Blocks...)
	r, err := Render(context.Background(), d, fakeImages{"huge": huge}, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Issue{Code: IssueImagesLimit, Block: "one", Message: "Use up to eight pictures totaling 1 MiB."}); len(r.Issues) != 1 || r.Issues[0] != want {
		t.Fatalf("used twice: want only %+v, got %+v", want, r.Issues)
	}
	images := fakeImages{"huge": huge}
	var blocks []Block
	for n := 0; n < 9; n++ {
		id := fmt.Sprintf("a%d", n)
		images[id] = pngImage(t, 2+n, 2)
		blocks = append(blocks, Block{ID: "i" + id, Type: "image", Src: "artifact:" + id, Width: 10})
	}
	blocks = append(blocks, Block{ID: "last", Type: "image", Src: "artifact:huge", Width: 10})
	d = oneBlock(blocks[0])
	d.Blocks = append(blocks, d.Blocks[1:]...)
	r, _ = Render(context.Background(), d, images, RenderOptions{})
	if countIssues(r.Issues, IssueImagesLimit) != 1 || r.Issues[0].Block != "ia8" {
		t.Fatalf("after the budget: want one images_limit, at ia8, got %+v", r.Issues)
	}
}

func TestEmbeddedPicturesKeepTheirSourceAndShareTheBudget(t *testing.T) {
	ctx := context.Background()
	embedded := dataURI(tinyPNG(t, 20, 10))
	r, err := Render(ctx, oneBlock(Block{ID: "i", Type: "image", Src: embedded, Width: 200}), nil, RenderOptions{})
	if err != nil || len(r.Issues) != 0 || !strings.Contains(r.HTML, `<img src="`+embedded+`" width="200" height="100"`) {
		t.Fatalf("embedded picture: %v %+v", err, r.Issues)
	}
	// Eight Library pictures and one embedded picture are nine.
	images := fakeImages{}
	var blocks []Block
	for n := 0; n < 8; n++ {
		id := fmt.Sprintf("a%d", n)
		images[id] = pngImage(t, 30+n, 2)
		blocks = append(blocks, Block{ID: "i" + id, Type: "image", Src: "artifact:" + id, Width: 10})
	}
	blocks = append(blocks, Block{ID: "embedded", Type: "image", Src: embedded, Width: 10})
	d := oneBlock(blocks[0])
	d.Blocks = append(blocks, d.Blocks[1:]...)
	r, _ = Render(ctx, d, images, RenderOptions{})
	if want := (Issue{Code: IssueImagesLimit, Block: "embedded", Message: "Use up to eight pictures totaling 1 MiB."}); len(r.Issues) != 1 || r.Issues[0] != want {
		t.Fatalf("want only %+v, got %+v", want, r.Issues)
	}
	// A Library picture and an embedded copy of the same bytes are one picture.
	same := oneBlock(Block{ID: "i", Type: "image", Src: embedded, Width: 10})
	same.Blocks = append([]Block{{ID: "j", Type: "image", Src: "artifact:a", Width: 10}}, same.Blocks...)
	dup := fakeImages{"a": {MIMEType: "image/png", Data: tinyPNG(t, 20, 10)}}
	for n := 0; n < 7; n++ {
		id := fmt.Sprintf("o%d", n)
		dup[id] = pngImage(t, 40+n, 2)
		same.Blocks = append([]Block{{ID: "x" + id, Type: "image", Src: "artifact:" + id, Width: 10}}, same.Blocks...)
	}
	if r, _ := Render(ctx, same, dup, RenderOptions{}); len(r.Issues) != 0 {
		t.Fatalf("eight distinct pictures: %+v", r.Issues)
	}
}

func TestImageLabelIsItsAltElseItsID(t *testing.T) {
	r, _ := Render(context.Background(), oneBlock(Block{ID: "hero", Type: "image", Width: 100}), nil, RenderOptions{})
	if want := (Issue{Code: IssueImageMissing, Block: "hero", Message: `Choose a picture for "hero".`}); len(r.Issues) != 1 || r.Issues[0] != want {
		t.Fatalf("want %+v, got %+v", want, r.Issues)
	}
	r, _ = Render(context.Background(), oneBlock(Block{ID: "hero", Type: "image", Width: 100, Height: 40}), nil, RenderOptions{})
	if !strings.Contains(r.HTML, ">Picture</div>") {
		t.Fatal(`a placeholder without an alt says "Picture"`)
	}
}

func TestImagePlaceholderIsHalfAsTallAsWideWithoutAHeight(t *testing.T) {
	r, _ := Render(context.Background(), oneBlock(Block{ID: "i", Type: "image", Width: 201}), nil, RenderOptions{})
	if !strings.Contains(r.HTML, "max-width:201px;height:101px;") || !strings.Contains(r.HTML, "line-height:101px;") {
		t.Fatalf("want a 101px tall placeholder:\n%s", r.HTML)
	}
}

func TestOutlookGetsAFixedWidthTableOnlyForANarrowerPicture(t *testing.T) {
	mso := func(width int) string {
		return fmt.Sprintf(`<!--[if mso]><table cellpadding="0" cellspacing="0" border="0" width="%d" style="width:%dpx"><tbody><tr><td><![endif]-->`, width, width)
	}
	images := fakeImages{"a1": pngImage(t, 2, 2)}
	for _, c := range []struct {
		name  string
		block Block
		mso   bool
		pad   string
	}{
		{"as wide as the column", Block{Width: 552}, false, "0px 24px 16px 24px"},
		{"one pixel narrower", Block{Width: 551}, true, "0px 24px 16px 24px"},
		{"bleed, full width", Block{Width: 600, Bleed: true}, false, "0px 0px 16px 0px"},
		{"bleed, narrower", Block{Width: 599, Bleed: true}, true, "0px 0px 16px 0px"},
		{"own padding, as wide", Block{Width: 580, Padding: &[4]float64{0, 10, 0, 10}}, false, "0px 10px 0px 10px"},
		{"own padding, narrower", Block{Width: 579, Padding: &[4]float64{0, 10, 0, 10}}, true, "0px 10px 0px 10px"},
		// With the bleed's zero sides the column would be 600 and 595 narrower.
		{"own padding wins over bleed", Block{Width: 595, Bleed: true, Padding: &[4]float64{2, 3, 4, 5}}, false, "2px 3px 4px 5px"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := c.block
			b.ID, b.Type, b.Src = "i", "image", "artifact:a1"
			r, err := Render(context.Background(), oneBlock(b), images, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(r.HTML, mso(b.Width)) != c.mso {
				t.Errorf("Outlook table: want %v", c.mso)
			}
			if !strings.Contains(r.HTML, `<tr><td style="padding:`+c.pad+`"><table cellpadding="0"`) {
				t.Errorf("want padding %s:\n%s", c.pad, r.HTML)
			}
		})
	}
}

func TestComputedLengthsAreRounded(t *testing.T) {
	for _, c := range []struct {
		name  string
		block Block
		want  []string
	}{
		{"heading line height from 28", Block{Type: "heading", Text: "x"}, []string{"line-height:30.8px;mso-line-height-alt:30.8px"}},
		{"heading line height from 26.7", Block{Type: "heading", Text: "x", Size: 26.7}, []string{"line-height:29.37px;mso-line-height-alt:29.37px"}},
		{"heading line height from 37.3", Block{Type: "heading", Text: "x", Size: 37.3}, []string{"line-height:41.03px;"}},
		{"highlight line height from 18", Block{Type: "highlight", Text: "x"}, []string{"line-height:9px;mso-line-height-alt:9px"}},
		{"a list's line height is the text's", Block{Type: "text", Text: "- x", LineHeight: 30}, []string{"line-height:30px;mso-line-height-alt:30px;font-size:0"}},
		{"a list's own line height", Block{Type: "text", Text: "x\n\n- y", LineHeight: 30, ListLineHeight: 17.64}, []string{"line-height:30px;mso-line-height-alt:30px\">x<", "line-height:17.64px;mso-line-height-alt:17.64px;font-size:0"}},
		{"highlight line height from 17.35", Block{Type: "highlight", Text: "x", Size: 17.35}, []string{"line-height:8.68px;"}},
		{"button 20", Block{Type: "button", Label: "x", Href: "https://example.com"}, []string{"height:50px;width:300px;", "border-radius:25px;", "line-height:28px;"}},
		{"button 20.2", Block{Type: "button", Label: "x", Href: "https://example.com", Size: 20.2}, []string{"height:51px;width:300px;", "border-radius:25px;", "line-height:28.3px;"}},
		{"button 21.1 with 12 above and below", Block{Type: "button", Label: "x", Href: "https://example.com", Size: 21.1, PaddingY: 12, Width: 316}, []string{"height:54px;width:316px;", "border-radius:26px;", "line-height:29.5px;", "padding-top:12px;padding-bottom:12px;"}},
		{"button 20.3", Block{Type: "button", Label: "x", Href: "https://example.com", Size: 20.3}, []string{"height:51px;", "border-radius:25px;", "line-height:28.4px;"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := c.block
			b.ID = "b"
			r, err := Render(context.Background(), oneBlock(b), nil, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range c.want {
				if !strings.Contains(r.HTML, want) {
					t.Errorf("missing %q", want)
				}
			}
		})
	}
}

func TestLargeTextIsMarkedForPhones(t *testing.T) {
	for _, c := range []struct {
		name  string
		block Block
		want  string // the class, or "" for none
	}{
		{"heading default 28", Block{Type: "heading", Text: "x"}, ` class="efs-280"`},
		{"heading 26.7", Block{Type: "heading", Text: "x", Size: 26.7}, ` class="efs-267"`},
		{"heading 16", Block{Type: "heading", Text: "x", Size: 16}, ""},
		{"heading 16.1", Block{Type: "heading", Text: "x", Size: 16.1}, ` class="efs-161"`},
		{"heading 26.75, rounded to the tenth", Block{Type: "heading", Text: "x", Size: 26.75}, ` class="efs-268"`},
		{"text default 16", Block{Type: "text", Text: "x"}, ""},
		{"text default 16 with a list", Block{Type: "text", Text: "x\n\n- y"}, ""},
		{"highlight default 18", Block{Type: "highlight", Text: "x"}, ` class="efs-180"`},
		{"button 20", Block{Type: "button", Label: "x", Href: "https://example.com"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := c.block
			b.ID = "b"
			r, err := Render(context.Background(), oneBlock(b), nil, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if c.want == "" {
				if strings.Contains(r.HTML, "efs-") {
					t.Fatalf("want no efs class:\n%s", r.HTML)
				}
				return
			}
			if !strings.Contains(r.HTML, c.want) {
				t.Fatalf("want %s:\n%s", c.want, r.HTML)
			}
		})
	}
	// A text block over 16px marks its paragraphs and its list items' text.
	r, _ := Render(context.Background(), oneBlock(Block{ID: "b", Type: "text", Text: "x\n\n- y", Size: 18.7}), nil, RenderOptions{})
	if strings.Count(r.HTML, ` class="efs-187"`) != 2 {
		t.Fatalf("want the paragraph and the item marked:\n%s", r.HTML)
	}
}

func TestLinkMissingIssue(t *testing.T) {
	r, err := Render(context.Background(), oneBlock(Block{ID: "b", Type: "button", Href: "", Label: "Shop"}), nil, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Issue{Code: IssueLinkMissing, Block: "b", Message: `Add a link to "Shop".`}); len(r.Issues) != 1 || r.Issues[0] != want {
		t.Fatalf("want only %+v, got %+v", want, r.Issues)
	}
	// A picture without a link is an ordinary picture.
	r, _ = Render(context.Background(), oneBlock(Block{ID: "i", Type: "image", Src: "artifact:a1", Width: 10}), fakeImages{"a1": pngImage(t, 2, 2)}, RenderOptions{})
	if len(r.Issues) != 0 {
		t.Fatalf("an image needs no link: %+v", r.Issues)
	}
}

func TestUnsubscribeIssueIsLayoutModeOnly(t *testing.T) {
	ctx := context.Background()
	without := Document{Subject: "Hello", Theme: neutral(), Blocks: []Block{{ID: "h", Type: "heading", Text: "Hi"}}}
	r, _ := Render(ctx, without, nil, RenderOptions{})
	want := Issue{Code: IssueUnsubscribeMissing, Message: "Add an unsubscribe link ({{unsubscribeUrl}}) to the footer."}
	if len(r.Issues) != 1 || r.Issues[0] != want {
		t.Fatalf("layout without the link: want only %+v, got %+v", want, r.Issues)
	}
	empty := Document{Subject: "Hello", Theme: neutral(), Blocks: []Block{}}
	if r, _ := Render(ctx, empty, nil, RenderOptions{}); !hasIssue(r.Issues, IssueUnsubscribeMissing) {
		t.Fatal("an empty layout has no unsubscribe link either")
	}
	if r, _ := Render(ctx, oneBlock(Block{ID: "h", Type: "heading", Text: "Hi"}), nil, RenderOptions{}); len(r.Issues) != 0 {
		t.Fatalf("legal text carries the tag: %+v", r.Issues)
	}
	// The link can be anywhere in the email, an html block included.
	inMarkup := Document{Subject: "Hello", Theme: neutral(), Blocks: []Block{{ID: "x", Type: "html", HTML: `<p><a href="{{unsubscribeUrl}}">Leave</a></p>`}}}
	if r, _ := Render(ctx, inMarkup, nil, RenderOptions{}); len(r.Issues) != 0 {
		t.Fatalf("an html block's link counts: %+v", r.Issues)
	}
	custom := Document{Subject: "Hello", TextBody: "Hi", HTMLBody: "<p>Hi</p>"}
	if r, _ := Render(ctx, custom, nil, RenderOptions{}); len(r.Issues) != 0 {
		t.Fatalf("custom HTML keeps getting the appended footer: %+v", r.Issues)
	}
}

func TestConfirmIssueOnlyWhenRequired(t *testing.T) {
	ctx := context.Background()
	d := oneBlock(Block{ID: "h", Type: "heading", Text: "Hi"})
	if r, _ := Render(ctx, d, nil, RenderOptions{}); hasIssue(r.Issues, IssueConfirmMissing) {
		t.Fatal("confirm_missing without RequireConfirm")
	}
	r, _ := Render(ctx, d, nil, RenderOptions{RequireConfirm: true})
	want := Issue{Code: IssueConfirmMissing, Message: "Add a confirm link ({{confirmUrl}}) so people can confirm their subscription."}
	if len(r.Issues) != 1 || r.Issues[0] != want {
		t.Fatalf("want only %+v, got %+v", want, r.Issues)
	}
	button := oneBlock(Block{ID: "b", Type: "button", Label: "Confirm", Href: "{{confirmUrl}}"})
	if r, _ := Render(ctx, button, nil, RenderOptions{RequireConfirm: true}); len(r.Issues) != 0 {
		t.Fatalf("the button carries the link: %+v", r.Issues)
	}
	// A custom email can carry it in either part.
	for _, c := range []struct {
		name string
		doc  Document
		want bool
	}{
		{"neither part", Document{Subject: "x", TextBody: "Hi", HTMLBody: "<p>Hi</p>"}, true},
		{"the text part of a text email", Document{Subject: "x", TextBody: "Confirm: {{confirmUrl}}", HTMLBody: ""}, false},
		{"the HTML part", Document{Subject: "x", TextBody: "Hi", HTMLBody: `<a href="{{confirmUrl}}">Confirm</a>`}, false},
		// An HTML email is read as HTML: its link has to be there, as a link.
		{"only the text part of an HTML email", Document{Subject: "x", TextBody: "Confirm: {{confirmUrl}}", HTMLBody: "<p>Hi</p>"}, true},
		{"HTML text, not a link", Document{Subject: "x", TextBody: "Hi", HTMLBody: "<p>Confirm: {{confirmUrl}}</p>"}, true},
		{"a picture's alt", Document{Subject: "x", TextBody: "Hi", HTMLBody: `<img src="https://example.com/a.png" alt="{{confirmUrl}}">`}, true},
		{"the preheader", Document{Subject: "x", Preheader: "{{confirmUrl}}", TextBody: "Hi", HTMLBody: "<p>Hi</p>"}, true},
	} {
		r, _ := Render(ctx, c.doc, nil, RenderOptions{RequireConfirm: true})
		if hasIssue(r.Issues, IssueConfirmMissing) != c.want {
			t.Errorf("custom, link in %s: want confirm_missing %v, got %+v", c.name, c.want, r.Issues)
		}
	}
}

func TestIssuesFollowTheDocumentOrder(t *testing.T) {
	d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{
		{ID: "first", Type: "button", Label: "One"},
		{ID: "second", Type: "image", Width: 10, Alt: "Two"},
		{ID: "third", Type: "button", Label: "Three"},
	}}
	r, err := Render(context.Background(), d, nil, RenderOptions{RequireConfirm: true})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, issue := range r.Issues {
		got = append(got, issue.Code+"/"+issue.Block)
	}
	want := []string{"link_missing/first", "image_missing/second", "link_missing/third", "unsubscribe_missing/", "confirm_missing/"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestCustomModeRender(t *testing.T) {
	ctx := context.Background()
	images := fakeImages{"a1": pngImage(t, 2, 2)}
	d := Document{Subject: "Hello", Preheader: "Soon", TextBody: "Hi there", HTMLBody: "<html><body><p>Hi</p><img src=\"artifact:a1\"></body></html>"}
	r, err := Render(ctx, d, images, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, afterBody, ok := strings.Cut(r.HTML, "<body>")
	if !ok || !strings.HasPrefix(afterBody, `<div style="display:none;`) {
		t.Fatalf("the preheader must follow <body>:\n%s", r.HTML)
	}
	preheader, _, _ := strings.Cut(afterBody, "</div>")
	if !strings.Contains(preheader, ">Soon&#847;") {
		t.Fatalf("the preheader must carry its text: %s", preheader)
	}
	if !strings.Contains(r.HTML, `<p>Hi</p><img src="data:image/png;base64,`) || strings.Contains(r.HTML, "artifact:") {
		t.Fatalf("the picture must be embedded:\n%s", r.HTML)
	}
	if r.Text != d.TextBody || len(r.Issues) != 0 || r.Subject != "Hello" {
		t.Fatalf("text %q issues %+v subject %q", r.Text, r.Issues, r.Subject)
	}
	plain := Document{Subject: "Hello", Preheader: "Soon", TextBody: "Hi", HTMLBody: ""}
	p, err := Render(ctx, plain, images, RenderOptions{})
	if err != nil || p.HTML != "" || len(p.Issues) != 0 || p.Text != "Hi" {
		t.Fatalf("plain text email: %q %+v %v", p.HTML, p.Issues, err)
	}
}

func TestCustomModeKeepsTheAuthorsMarkup(t *testing.T) {
	src := "<!DOCTYPE html>\n<html>\n<body class=\"x\">\n<!-- a <body> in a comment -->\n<p style=\"color:red\">Hi {{displayName}}</p>\n</body>\n</html>\n"
	d := Document{Subject: "x", Preheader: "Soon", TextBody: "Hi {{displayName}}", HTMLBody: src}
	r, err := Render(context.Background(), d, nil, RenderOptions{Sample: map[string]string{"displayName": "A&B"}})
	if err != nil {
		t.Fatal(err)
	}
	head, rest, ok := strings.Cut(r.HTML, `<body class="x">`)
	if !ok || head != "<!DOCTYPE html>\n<html>\n" || !strings.HasPrefix(rest, `<div style="display:none;`) {
		t.Fatalf("the preheader goes after the first body tag and nothing else moves:\n%s", r.HTML)
	}
	_, after, _ := strings.Cut(rest, "</div>")
	if after != "\n<!-- a <body> in a comment -->\n<p style=\"color:red\">Hi A&amp;B</p>\n</body>\n</html>\n" {
		t.Fatalf("the author's markup changed:\n%q", after)
	}
	if r.Text != "Hi A&B" {
		t.Fatalf("text: %q", r.Text)
	}
	// Without a body tag the preheader goes first.
	bare := Document{Subject: "x", Preheader: "Soon", TextBody: "Hi", HTMLBody: "<p>Hi</p>"}
	r, _ = Render(context.Background(), bare, nil, RenderOptions{})
	if !strings.HasPrefix(r.HTML, `<div style="display:none;`) || !strings.HasSuffix(r.HTML, "</div><p>Hi</p>") {
		t.Fatalf("want the preheader first:\n%s", r.HTML)
	}
	// Without a preheader the markup is the author's, byte for byte.
	d.Preheader = ""
	r, _ = Render(context.Background(), d, nil, RenderOptions{})
	if r.HTML != src {
		t.Fatalf("want the source back:\n%q", r.HTML)
	}
}

func TestCustomModePictures(t *testing.T) {
	embedded := dataURI(tinyPNG(t, 3, 3))
	body := `<img src="artifact:a1" alt="Logo">` +
		`<img src="artifact:gone" alt="Gone">` +
		`<img src="artifact:nameless">` +
		`<img src="cid:image-1@memql" alt="Old">` +
		`<img src="memql-asset:x1">` +
		`<img src="https://example.com/remote.png" alt="Remote">` +
		`<img src="` + embedded + `" alt="Embedded">` +
		`<img src="data:image/png;base64,bm90IGEgcGljdHVyZQ==" alt="Broken">`
	d := Document{Subject: "x", TextBody: "x", HTMLBody: body}
	r, err := Render(context.Background(), d, fakeImages{"a1": pngImage(t, 2, 2)}, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	unavailable := func(label string) Issue {
		return Issue{Code: IssueImageUnavailable, Message: `The picture for "` + label + `" could not be read. Choose it again.`}
	}
	want := []Issue{unavailable("Gone"), unavailable("nameless"), unavailable("Old"), unavailable("memql-asset:x1"), unavailable("Broken")}
	if fmt.Sprint(r.Issues) != fmt.Sprint(want) {
		t.Fatalf("issues:\n got %+v\nwant %+v", r.Issues, want)
	}
	for _, kept := range []string{`src="artifact:gone"`, `src="cid:image-1@memql"`, `src="https://example.com/remote.png"`, `src="` + embedded + `"`} {
		if !strings.Contains(r.HTML, kept) {
			t.Errorf("want %s left as written", kept)
		}
	}
	if strings.Contains(r.HTML, "artifact:a1") || strings.Count(r.HTML, `src="data:image/png;base64,`) != 3 {
		t.Fatalf("want the Library picture embedded:\n%s", r.HTML)
	}
	// Embedded pictures share the budget with Library pictures.
	var many strings.Builder
	images := fakeImages{}
	for n := 0; n < 8; n++ {
		id := fmt.Sprintf("p%d", n)
		images[id] = pngImage(t, 5+n, 2)
		many.WriteString(`<img src="artifact:` + id + `">`)
	}
	many.WriteString(`<img src="` + embedded + `">`)
	r, _ = Render(context.Background(), Document{Subject: "x", TextBody: "x", HTMLBody: many.String()}, images, RenderOptions{})
	if want := (Issue{Code: IssueImagesLimit, Message: "Use up to eight pictures totaling 1 MiB."}); len(r.Issues) != 1 || r.Issues[0] != want {
		t.Fatalf("want only %+v, got %+v", want, r.Issues)
	}
}

func TestTextPart(t *testing.T) {
	d := oneBlock(Block{ID: "t", Type: "heading", Text: "Title"})
	d.Blocks = append([]Block{
		d.Blocks[0],
		{ID: "body", Type: "text", Text: "Para one\n\n- a\n- b\n\n[Docs](https://example.com)"},
		{ID: "b", Type: "button", Label: "Read", Href: "https://example.com/r"},
		{ID: "d", Type: "divider"},
		{ID: "i", Type: "image", Width: 100},
	}, d.Blocks[1:]...)
	r, err := Render(context.Background(), d, nil, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := "Title\n\nPara one\n\n- a\n- b\n\nDocs (https://example.com)\n\nRead: https://example.com/r\n\nUnsubscribe ({{unsubscribeUrl}})\n"
	if r.Text != want {
		t.Fatalf("text part:\n got %q\nwant %q", r.Text, want)
	}
}

func TestTextPartOfEachLeaf(t *testing.T) {
	for _, c := range []struct {
		name  string
		block Block
		want  string
	}{
		{"heading lines as written", Block{Type: "heading", Text: "One\r\nTwo"}, "One\nTwo\n"},
		{"emphasis markers are removed", Block{Type: "text", Text: "**b** *i* ***bi*** \\*literal\\*"}, "b i bi *literal*\n"},
		{"a line break stays", Block{Type: "text", Text: "one\ntwo"}, "one\ntwo\n"},
		{"numbered items keep their numbers", Block{Type: "text", Text: "3. c\n4. d"}, "3. c\n4. d\n"},
		{"a link in a list item", Block{Type: "text", Text: "- see [x](mailto:a@example.com)"}, "- see x (mailto:a@example.com)\n"},
		{"highlight lines", Block{Type: "highlight", Text: "one\ntwo"}, "one\ntwo\n"},
		{"a button without a link", Block{Type: "button", Label: "Shop"}, "Shop\n"},
		{"html text content", Block{Type: "html", HTML: "<p>One <b>two</b></p><p>three<br>four</p><ul><li>five</li><li>six</li></ul><table><tr><td>a</td><td>b</td></tr></table>"}, "One two\nthree\nfour\nfive\nsix\na b\n"},
		{"html whitespace collapses", Block{Type: "html", HTML: "<div>\n  spaced   out\n</div>"}, "spaced out\n"},
		{"html link text without its address", Block{Type: "html", HTML: `<a href="https://example.com">here</a>`}, "here\n"},
		{"spacer", Block{Type: "spacer", Height: 10}, ""},
		{"divider", Block{Type: "divider"}, ""},
		{"image", Block{Type: "image", Width: 10, Alt: "x", Href: "https://example.com"}, ""},
		{"an empty heading", Block{Type: "heading"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := c.block
			b.ID = "b"
			d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{b}}
			r, err := Render(context.Background(), d, nil, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if r.Text != c.want {
				t.Fatalf("got %q, want %q", r.Text, c.want)
			}
		})
	}
}

func TestHTMLBlocksAreSplicedInTheirNormalizedForm(t *testing.T) {
	// The renderer splices safeMarkup(Block.HTML), never Block.HTML. This source
	// differs from its normalized form in every way normalizing can differ.
	src := `<table width=100%><tr><td align=center>A &amp; B&nbsp;&copy; it&#39;s<br></td></tr></table>`
	r, err := Render(context.Background(), oneBlock(Block{ID: "x", Type: "html", HTML: src}), nil, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := `<tr><td style="padding:0px 24px 16px 24px"><table width="100%"><tbody><tr><td align="center">A &amp; B` + "\u00a0" + `© it&#39;s<br/></td></tr></tbody></table></td></tr>`
	if !strings.Contains(r.HTML, want) {
		t.Fatalf("want the normalized markup %s in:\n%s", want, r.HTML)
	}
	if strings.Contains(r.HTML, src) || strings.Contains(r.HTML, "width=100%") || strings.Contains(r.HTML, "&copy;") {
		t.Fatal("the source of an html block reached the email")
	}
	// Markup Parse would refuse never reaches the email, in any form: the
	// block's cell stays empty.
	for _, bad := range []string{`</td></tr></table><p>after</p>`, `<img src="https://evil.example/p.gif"`, `<script>alert(1)</script>`, `<!-- x`} {
		r, err := Render(context.Background(), oneBlock(Block{ID: "x", Type: "html", HTML: bad}), nil, RenderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.HTML, `<tr><td style="padding:0px 24px 16px 24px"></td></tr>`) ||
			strings.Contains(r.HTML, "after") || strings.Contains(r.HTML, "evil") || strings.Contains(r.HTML, "alert") || strings.Contains(r.HTML, "<!-- x") {
			t.Errorf("%q reached the email:\n%s", bad, r.HTML)
		}
	}
}

func TestRenderedEmailIsUTF8(t *testing.T) {
	// Normalized html blocks carry entities as the characters they name.
	d := oneBlock(Block{ID: "x", Type: "html", HTML: `<p>A&nbsp;B &copy; C &mdash; D</p>`})
	r, err := Render(context.Background(), d, nil, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.HTML, `<meta http-equiv="Content-Type" content="text/html; charset=UTF-8">`) {
		t.Fatal("the HTML must declare UTF-8")
	}
	const chars = "A\u00a0B © C — D"
	if !strings.Contains(r.HTML, chars) || !strings.Contains(r.Text, chars) || !utf8.ValidString(r.HTML) || !utf8.ValidString(r.Text) {
		t.Fatalf("want %q as UTF-8 in both parts:\n%s\n%q", chars, r.HTML, r.Text)
	}
}

func TestPreheaderIsHiddenAtTheTopOfTheBody(t *testing.T) {
	d := oneBlock(Block{ID: "h", Type: "heading", Text: "Hi"})
	d.Preheader = "Soon & {{displayName}}"
	r, err := Render(context.Background(), d, nil, RenderOptions{Sample: map[string]string{"displayName": "<Ada>"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `margin:0;padding:0"><div style="display:none;font-size:1px;color:#eef0f3;line-height:1px;max-height:0px;max-width:0px;opacity:0;overflow:hidden;mso-hide:all">Soon &amp; &lt;Ada&gt;` +
		strings.Repeat("&#847;&zwnj;&nbsp;", 60) + `</div><table width="100%" border="0"`
	if !strings.Contains(r.HTML, want) {
		t.Fatalf("want the preheader at the top of the body:\n%s", r.HTML)
	}
	d.Preheader = ""
	if r, _ := Render(context.Background(), d, nil, RenderOptions{}); strings.Contains(r.HTML, "mso-hide:all") || strings.Contains(r.HTML, "&#847;") {
		t.Fatal("no preheader, no hidden div")
	}
}

func TestWebFontsAreLinkedOnlyWhenTheThemeAsks(t *testing.T) {
	d := oneBlock(Block{ID: "h", Type: "heading", Text: "Hi"})
	d.Theme.HeadingFont, d.Theme.BodyFont, d.Theme.WebFonts = "Example Serif", "Example Sans", true
	r, _ := Render(context.Background(), d, nil, RenderOptions{})
	want := `<meta name="x-apple-disable-message-reformatting"><link href="https://fonts.googleapis.com/css?family=Example+Serif:400,400i,700,700i|Example+Sans:400,400i,700,700i&amp;display=swap" rel="stylesheet"><style>`
	if !strings.Contains(r.HTML, want) {
		t.Fatalf("want %s in:\n%s", want, r.HTML)
	}
	d.Theme.BodyFont = "Example Serif"
	r, _ = Render(context.Background(), d, nil, RenderOptions{})
	if !strings.Contains(r.HTML, `<link href="https://fonts.googleapis.com/css?family=Example+Serif:400,400i,700,700i&amp;display=swap" rel="stylesheet">`) {
		t.Fatalf("one family when the fonts are equal:\n%s", r.HTML)
	}
	d.Theme.WebFonts = false
	if r, _ := Render(context.Background(), d, nil, RenderOptions{}); strings.Contains(r.HTML, "fonts.googleapis.com") {
		t.Fatal("no web fonts unless the theme asks")
	}
}

func TestRenderReturnsAnErrorOnlyForACancelledContext(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Render(cancelled, oneBlock(Block{ID: "h", Type: "heading", Text: "Hi"}), nil, RenderOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled render: %v", err)
	}
	// Cancelled while a picture is being read: the error, not an issue.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelling := resolverFunc(func(context.Context, string) (Image, error) {
		cancel()
		return Image{}, errors.New("read interrupted")
	})
	if _, err := Render(ctx, oneBlock(Block{ID: "i", Type: "image", Src: "artifact:a1", Width: 10}), cancelling, RenderOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled during a read: %v", err)
	}
	custom := Document{Subject: "x", TextBody: "x", HTMLBody: `<img src="artifact:a1">`}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	if _, err := Render(ctx, custom, resolverFunc(func(context.Context, string) (Image, error) {
		cancel()
		return Image{}, errors.New("read interrupted")
	}), RenderOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("custom, cancelled during a read: %v", err)
	}
}

func TestEachPictureIsReadOnce(t *testing.T) {
	reads := map[string]int{}
	counting := resolverFunc(func(_ context.Context, id string) (Image, error) {
		reads[id]++
		if id == "gone" {
			return Image{}, errors.New("not readable")
		}
		return pngImage(t, 2, 2), nil
	})
	d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{
		{ID: "a", Type: "image", Src: "artifact:a1", Width: 10},
		{ID: "b", Type: "image", Src: "artifact:a1", Width: 20},
		{ID: "c", Type: "image", Src: "artifact:gone", Width: 10},
		{ID: "d", Type: "image", Src: "artifact:gone", Width: 10},
	}}
	r, err := Render(context.Background(), d, counting, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reads["a1"] != 1 || reads["gone"] != 1 {
		t.Fatalf("reads: %v", reads)
	}
	// Every block that names an unreadable picture says so.
	if countIssues(r.Issues, IssueImageUnavailable) != 2 {
		t.Fatalf("issues: %+v", r.Issues)
	}
}

type resolverFunc func(context.Context, string) (Image, error)

func (f resolverFunc) ResolveImage(ctx context.Context, id string) (Image, error) { return f(ctx, id) }

func TestRenderedHTMLHasNoNewlinesOfItsOwn(t *testing.T) {
	// Text line breaks become <br> (or rows) and attribute line breaks become
	// character references; the markup itself carries no newline.
	d := Document{Subject: "x", Preheader: "p", Theme: neutral(), Blocks: []Block{
		{ID: "h", Type: "heading", Text: "one\ntwo\r\nthree\rfour"},
		{ID: "t", Type: "text", Text: "one\ntwo\n\n- three"},
		{ID: "l", Type: "highlight", Text: "one\r\ntwo"},
		{ID: "b", Type: "button", Label: "one\ntwo", Href: "https://example.com"},
		{ID: "p", Type: "image", Width: 10, Alt: "one\ntwo"},
		{ID: "i", Type: "image", Width: 10, Alt: "one\r\ntwo", Src: "artifact:a1"},
	}}
	r, err := Render(context.Background(), d, fakeImages{"a1": pngImage(t, 2, 2)}, RenderOptions{Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(r.HTML, "\r\n") {
		t.Fatalf("the HTML carries a line break:\n%q", r.HTML)
	}
	for _, want := range []string{
		">one<br>two<br>three<br>four</td>",
		">one<br>two</td>",
		">one</span></td></tr><tr>",
		">one<br>two</span></a>",
		">one<br>two</div>",
		`alt="one&#13;&#10;two"`,
	} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestRenderEscapesAnUncheckedDocument(t *testing.T) {
	// Parse refuses all of this. With a Document built without it, every
	// string Render writes as text or as an attribute value is escaped, so none
	// can close an attribute or open an element.
	const hostile = `"><script>x</script>`
	d := Document{Subject: "x", Preheader: hostile, Theme: neutral(), Blocks: []Block{
		{ID: hostile, Type: "heading", Text: hostile},
		{ID: "t", Type: "text", Text: hostile + "\n\n- " + hostile},
		{ID: "b", Type: "button", Label: hostile, Href: "https://example.com"},
		{ID: "i", Type: "image", Width: 10, Src: "artifact:a1", Alt: hostile},
	}}
	r, err := Render(context.Background(), d, fakeImages{"a1": pngImage(t, 2, 2)}, RenderOptions{Preview: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.HTML, "<script") {
		t.Fatalf("a value reached the HTML unescaped:\n%s", r.HTML)
	}
	// The values are there, escaped: the id, the preheader, the heading, the
	// paragraph and the item, the button's label (twice) and the alt.
	if got := strings.Count(r.HTML, "&#34;&gt;&lt;script&gt;x&lt;/script&gt;"); got < 8 {
		t.Fatalf("want the escaped value throughout the email, found it %d times:\n%s", got, r.HTML)
	}
}

func TestAnUncheckedDocumentCannotInjectStylesLinksOrSources(t *testing.T) {
	// Parse refuses each of these too. Render does not write them: a link that
	// breaks the link rule, a theme colour or font that is not one, an
	// alignment or letter spacing that is not one, and a picture source that is
	// not a Library file or an embedded raster are left out.
	const evil = "https://evil.example/x"
	theme := neutral()
	theme.Page = "#fff;background:url(" + evil + ")"
	theme.Canvas = "#fff\"onmouseover=\"x"
	theme.Primary = "red;background-image:url(" + evil + ")"
	theme.HeadingFont = "Georgia;background:url(" + evil + ")"
	theme.BodyFont = "Arial', url(" + evil + ")"
	theme.WebFonts = true
	png := base64.StdEncoding.EncodeToString(tinyPNG(t, 2, 2))
	d := Document{Subject: "x", Preheader: "p", Theme: theme, Blocks: []Block{
		{ID: "h", Type: "heading", Text: "x", Align: "left;background:url(" + evil + ")", LetterSpacing: "0;background:url(" + evil + ")"},
		{ID: "t", Type: "text", Text: "[x](javascript:alert(1)) and [y](" + evil + "?a=\"onmouseover)", Align: "center;x:url(" + evil + ")"},
		{ID: "b", Type: "button", Label: "Go", Href: "javascript:alert(1)"},
		{ID: "i", Type: "image", Width: 10, Href: "javascript:alert(1)", Src: "image/png;base64," + png},
		{ID: "j", Type: "image", Width: 10, Src: "https://evil.example/p.png"},
	}}
	r, err := Render(context.Background(), d, nil, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{r.HTML, r.Text} {
		for _, banned := range []string{"evil.example", "javascript:", "url(", "onmouseover", "image/png;base64"} {
			if strings.Contains(part, banned) {
				t.Errorf("%q reached the email:\n%s", banned, part)
			}
		}
	}
	for _, want := range []Issue{
		{Code: IssueLinkMissing, Block: "b", Message: `Add a link to "Go".`},
		{Code: IssueImageUnavailable, Block: "i", Message: `The picture for "i" could not be read. Choose it again.`},
		{Code: IssueImageUnavailable, Block: "j", Message: `The picture for "j" could not be read. Choose it again.`},
	} {
		found := false
		for _, issue := range r.Issues {
			found = found || issue == want
		}
		if !found {
			t.Errorf("want %+v in %+v", want, r.Issues)
		}
	}
	// What is left out is drawn without it: the links as words, the colours
	// and fonts as the column's own.
	if !strings.Contains(r.HTML, ">x and y</td>") || !strings.Contains(r.Text, "x and y") {
		t.Errorf("want the link labels as words:\n%s\n%s", r.HTML, r.Text)
	}
}

func TestCustomModeImageWithTwoSourcesUsesTheFirst(t *testing.T) {
	// The tokenizer keeps the first of two equal attributes, as a browser and
	// as sending do (compose.RewriteEmailImageSources reads the same tokens), so
	// the second source is never read and never sent.
	reads := map[string]int{}
	counting := resolverFunc(func(_ context.Context, id string) (Image, error) {
		reads[id]++
		return pngImage(t, 2, 2), nil
	})
	d := Document{Subject: "x", TextBody: "x", HTMLBody: `<p>Hi</p><img src="artifact:a1" src="artifact:a2" alt="Twice">`}
	r, err := Render(context.Background(), d, counting, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Issues) != 0 || reads["a1"] != 1 || reads["a2"] != 0 || !strings.HasPrefix(r.HTML, `<p>Hi</p><img src="data:image/png;base64,`) || strings.Contains(r.HTML, "artifact:") {
		t.Fatalf("issues %+v reads %v\n%s", r.Issues, reads, r.HTML)
	}
}

func TestCustomModeMarkupOverTwoMebibytesStaysAsWritten(t *testing.T) {
	// Parse never admits it, and RewriteEmailImageSources refuses it; a
	// Document built without Parse still renders, as written.
	d := Document{Subject: "x", TextBody: "x", HTMLBody: strings.Repeat("a", 2<<20) + `<img src="artifact:a1">`}
	r, err := Render(context.Background(), d, fakeImages{"a1": pngImage(t, 2, 2)}, RenderOptions{})
	if err != nil || r.HTML != d.HTMLBody {
		t.Fatalf("want the markup as written: %v", err)
	}
}

func TestNoIssuesIsAnEmptyList(t *testing.T) {
	// Callers hand the list to JSON clients, which should see [] and not null.
	r, err := Render(context.Background(), oneBlock(Block{ID: "h", Type: "heading", Text: "Hi"}), nil, RenderOptions{})
	if err != nil || r.Issues == nil || len(r.Issues) != 0 {
		t.Fatalf("want an empty list, got %#v %v", r.Issues, err)
	}
	r, _ = Render(context.Background(), Document{Subject: "x", TextBody: "x"}, nil, RenderOptions{})
	if r.Issues == nil {
		t.Fatal("custom mode: want an empty list, not nil")
	}
}

// drawnBlocks is how many cells a render marks in a preview: one per block of
// a type the renderer draws.
func drawnBlocks(blocks []Block) []string {
	var ids []string
	for _, b := range blocks {
		switch b.Type {
		case typeImage, typeHeading, typeText, typeHighlight, typeButton, typeDivider, typeSpacer, typeHTML:
			ids = append(ids, b.ID)
		}
	}
	return ids
}

func FuzzRender(f *testing.F) {
	for _, blocks := range []string{
		`{"id":"h","type":"heading","text":"Hi {{displayName}} & <b>"}`,
		`{"id":"t","type":"text","text":"a **b** *c* ***d***\n\n- e\n1. f [g](https://example.com/?a=1&b=2)"}`,
		`{"id":"l","type":"highlight","text":"one\ntwo","size":17.3}`,
		`{"id":"b","type":"button","label":"Go","href":"{{confirmUrl}}","size":20.2}`,
		`{"id":"i","type":"image","width":200,"alt":"x","href":"https://example.com"},{"id":"d","type":"divider"},{"id":"s","type":"spacer","height":8}`,
		`{"id":"x","type":"html","html":"<table width=100%><tr><td>A &amp; B&nbsp;&copy;<br></td></tr></table>"}`,
		`{"id":"x","type":"html","html":"<p>one<p>two</p></p><b><i>x</b></i>"}`,
		`{"id":"s","type":"section","blocks":[{"id":"t","type":"text","text":"x"}]}`,
		// Words written to close their cell and forge a mark.
		`{"id":"h","type":"heading","text":"</td></tr></tbody></table><p data-block=\"evil\">x</p>"},` +
			`{"id":"t","type":"text","text":"</td></tr><tr><td data-block=\"evil\">\n\n- </td><td data-block=\"evil\">"},` +
			`{"id":"l","type":"highlight","text":"</span></td></tr><tr><td data-block=\"evil\">"},` +
			`{"id":"b","type":"button","label":"--><td data-block=\"evil\">","href":"https://example.com"},` +
			`{"id":"i","type":"image","width":10,"alt":"\"><td data-block=\"evil\">"},` +
			`{"id":"j","type":"image","width":10,"src":"artifact:a1","alt":"\"><td data-block=\"evil\">"}`,
	} {
		f.Add([]byte(layoutJSON(blocks)), true)
	}
	f.Add([]byte(`{"subject":"x","preheader":"p","textBody":"t","htmlBody":"<body><img src=\"artifact:a1\"></body>"}`), false)
	images := fakeImages{"a1": pngImage(f, 2, 2)}
	f.Fuzz(func(t *testing.T, data []byte, preview bool) {
		d, err := Parse(data)
		if err != nil {
			return
		}
		r, err := Render(context.Background(), d, images, RenderOptions{Preview: preview, Sample: map[string]string{"displayName": `"><b>x`}})
		if err != nil || r.Issues == nil {
			t.Fatalf("render: %v %v", err, r.Issues)
		}
		if d.Mode() != ModeLayout {
			return
		}
		if !strings.HasSuffix(r.HTML, "</body></html>") || strings.Count(r.HTML, "</body>") != 1 || (r.Text != "" && !strings.HasSuffix(r.Text, "\n")) {
			t.Fatalf("the email is not whole:\n%s", r.HTML)
		}
		// Read the way a browser reads it, every drawn block has its own marked
		// cell, in order, and nothing else is marked: no block's content left
		// its cell or reached into another's.
		page, err := html.Parse(strings.NewReader(r.HTML))
		if err != nil {
			t.Fatal(err)
		}
		var marked []string
		var walk func(n *html.Node)
		walk = func(n *html.Node) {
			for _, a := range n.Attr {
				if a.Key == "data-block" {
					marked = append(marked, a.Val)
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(page)
		want := drawnBlocks(d.Blocks)
		if !preview {
			want = nil
		}
		if strings.Join(marked, " ") != strings.Join(want, " ") {
			t.Fatalf("marked cells %v, want %v:\n%s", marked, want, r.HTML)
		}
	})
}

// sendingCap is the most HTML sending takes (compose.RewriteEmailImageSources,
// which compose.ExtractEmailImages applies to every email it sends).
const sendingCap = 2 << 20

var emailTooLarge = Issue{Code: IssueEmailTooLarge, Message: "This email is larger than 2 MiB once its pictures are embedded. Use fewer or smaller pictures."}

// allocatedBy is how many bytes f allocates, all of them, short-lived or not.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// bigPicture is about 0.8 MiB, inside the picture budget on its own, and about
// 1.07 MiB embedded: two of it are more than sending takes.
func bigPicture(t *testing.T) Image {
	t.Helper()
	img := pngImage(t, 520, 540)
	if n := len(img.Data); n > MaxImageBytes || 2*base64.StdEncoding.EncodedLen(n) <= sendingCap || base64.StdEncoding.EncodedLen(n) > sendingCap*3/4 {
		t.Fatalf("fixture is %d bytes, which does not make the case", n)
	}
	return img
}

func TestAPictureUsedTwiceCanMakeTheEmailTooLargeToSend(t *testing.T) {
	ctx := context.Background()
	images := fakeImages{"big": bigPicture(t)}
	once := oneBlock(Block{ID: "one", Type: "image", Src: "artifact:big", Width: 100})
	r, err := Render(ctx, once, images, RenderOptions{})
	if err != nil || len(r.Issues) != 0 || strings.Count(r.HTML, `<img src="data:`) != 1 {
		t.Fatalf("used once: %v %+v", err, r.Issues)
	}
	twice := oneBlock(Block{ID: "two", Type: "image", Src: "artifact:big", Width: 100, Alt: "Again"})
	twice.Blocks = append([]Block{{ID: "one", Type: "image", Src: "artifact:big", Width: 100}}, twice.Blocks...)
	r, err = Render(ctx, twice, images, RenderOptions{})
	if err != nil || len(r.Issues) != 1 || r.Issues[0] != emailTooLarge {
		t.Fatalf("used twice: want only %+v, got %+v %v", emailTooLarge, r.Issues, err)
	}
	// The use that does not fit is drawn as the placeholder, so the HTML
	// stays within what sending takes.
	if strings.Count(r.HTML, `<img src="data:`) != 1 || !strings.Contains(r.HTML, ">Again</div>") || len(r.HTML) > sendingCap {
		t.Fatalf("want one picture and one placeholder in %d bytes", len(r.HTML))
	}
}

func TestRepeatedPicturesCannotInflateTheEmail(t *testing.T) {
	ctx := context.Background()
	images := fakeImages{"big": bigPicture(t)}
	// A layout of 299 uses of one picture is about 18 KB of document.
	var blocks []Block
	for n := 0; n < 299; n++ {
		blocks = append(blocks, Block{ID: fmt.Sprintf("i%d", n), Type: "image", Src: "artifact:big", Width: 100})
	}
	d := oneBlock(blocks[0])
	d.Blocks = append(blocks, d.Blocks[1:]...)
	var r Result
	var err error
	allocated := allocatedBy(func() { r, err = Render(ctx, d, images, RenderOptions{Preview: true}) })
	if err != nil || len(r.Issues) != 1 || r.Issues[0] != emailTooLarge {
		t.Fatalf("layout: want only %+v, got %+v %v", emailTooLarge, r.Issues, err)
	}
	if strings.Count(r.HTML, `<img src="data:`) != 1 || len(r.HTML) > sendingCap || allocated > 64<<20 {
		t.Fatalf("layout: %d pictures embedded, %d bytes of HTML, %d bytes allocated", strings.Count(r.HTML, `<img src="data:`), len(r.HTML), allocated)
	}
	// Custom markup with a thousand uses of it is about 24 KB.
	custom := Document{Subject: "x", TextBody: "x", HTMLBody: strings.Repeat(`<img src="artifact:big">`, 1000)}
	allocated = allocatedBy(func() { r, err = Render(ctx, custom, images, RenderOptions{}) })
	if err != nil || len(r.Issues) != 1 || r.Issues[0] != emailTooLarge {
		t.Fatalf("custom: want only %+v, got %+v %v", emailTooLarge, r.Issues, err)
	}
	if strings.Count(r.HTML, `<img src="data:`) != 1 || strings.Count(r.HTML, `<img src="artifact:big">`) != 999 || len(r.HTML) > sendingCap || allocated > 64<<20 {
		t.Fatalf("custom: %d pictures embedded, %d bytes of HTML, %d bytes allocated", strings.Count(r.HTML, `<img src="data:`), len(r.HTML), allocated)
	}
}

func TestMarkupOverTheSendingCapIsAnIssue(t *testing.T) {
	// No picture at all: about 3,600 list items are more HTML than sending
	// takes, and the email says so.
	d := oneBlock(Block{ID: "t", Type: "text", Text: strings.Repeat("- a\n", 3600)})
	r, err := Render(context.Background(), d, nil, RenderOptions{})
	if err != nil || len(r.HTML) <= sendingCap || len(r.Issues) != 1 || r.Issues[0] != emailTooLarge {
		t.Fatalf("%d bytes: want only %+v, got %+v %v", len(r.HTML), emailTooLarge, r.Issues, err)
	}
	// And a picture after that point is not embedded.
	d.Blocks = append(d.Blocks, Block{ID: "i", Type: "image", Src: "artifact:a1", Width: 10, Alt: "Late"})
	r, _ = Render(context.Background(), d, fakeImages{"a1": pngImage(t, 2, 2)}, RenderOptions{})
	if strings.Contains(r.HTML, `<img src="data:`) || !strings.Contains(r.HTML, ">Late</div>") || countIssues(r.Issues, IssueEmailTooLarge) != 1 {
		t.Fatalf("want the late picture as a placeholder and one issue: %+v", r.Issues)
	}
}

func TestCustomMarkupOverTheSendingCapIsAnIssue(t *testing.T) {
	// The largest HTML Parse admits, and the preheader on top of it, is more
	// than sending takes.
	src := `{"subject":"x","preheader":"` + strings.Repeat("p", 200) + `","textBody":"x","htmlBody":"` + strings.Repeat("a", 2_096_800) + `"}`
	d, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Render(context.Background(), d, nil, RenderOptions{})
	if err != nil || len(r.HTML) <= sendingCap || len(r.Issues) != 1 || r.Issues[0] != emailTooLarge {
		t.Fatalf("%d bytes: want only %+v, got %+v %v", len(r.HTML), emailTooLarge, r.Issues, err)
	}
	// Without the preheader it fits.
	d.Preheader = ""
	if r, _ := Render(context.Background(), d, nil, RenderOptions{}); len(r.Issues) != 0 {
		t.Fatalf("want no issue, got %+v", r.Issues)
	}
}

func TestRenderStopsAtItsCeiling(t *testing.T) {
	ctx := context.Background()
	// Markup grows faster than its source: a list item of four bytes is a row
	// of about six hundred, so a document Parse accepts can ask for hundreds of
	// mebibytes of HTML. Render stops at MaxRenderBytes, as it writes.
	items := strings.Repeat("- a\n", 300_000)
	d, err := Parse([]byte(layoutJSON(`{"id":"t","type":"text","text":` + jsonString(items) + `}`)))
	if err != nil {
		t.Fatal(err)
	}
	// Reading the Markdown costs what the input costs; writing may cost a few
	// times the ceiling (a growing buffer and the rows it is made of), never
	// what the input asks for, which is over a gigabyte here.
	parsing := allocatedBy(func() { _, _ = parseMarkdown(items) })
	var renderErr error
	rendering := allocatedBy(func() { _, renderErr = Render(ctx, d, nil, RenderOptions{}) })
	if !errors.Is(renderErr, ErrTooLarge) || rendering > parsing+8*MaxRenderBytes {
		t.Fatalf("list: want ErrTooLarge within the ceiling, got %v after %d bytes (%d of them reading the Markdown)", renderErr, rendering, parsing)
	}
	// Paragraphs and highlight lines grow the same way.
	paragraphs, lines := strings.Repeat("a\n\n", 300_000), strings.Repeat("a\n", 450_000)
	for _, c := range []struct {
		name, block string
		read        func()
	}{
		{"paragraphs", `{"id":"t","type":"text","text":` + jsonString(paragraphs) + `}`, func() { _, _ = parseMarkdown(paragraphs) }},
		{"highlight lines", `{"id":"l","type":"highlight","text":` + jsonString(lines) + `}`, func() { _ = strings.Split(oneNewline(lines), "\n") }},
	} {
		d, err := Parse([]byte(layoutJSON(c.block)))
		if err != nil {
			t.Fatal(err)
		}
		reading := allocatedBy(c.read)
		rendering := allocatedBy(func() { _, renderErr = Render(ctx, d, nil, RenderOptions{}) })
		if !errors.Is(renderErr, ErrTooLarge) || rendering > reading+8*MaxRenderBytes {
			t.Fatalf("%s: want ErrTooLarge within the ceiling, got %v after %d bytes (%d of them reading)", c.name, renderErr, rendering, reading)
		}
	}
	// It stops as soon as it is past the ceiling: the blocks after are never
	// written, and their pictures never read. (Only a Document built without
	// Parse can hold a heading this long.)
	reads := 0
	counting := resolverFunc(func(context.Context, string) (Image, error) {
		reads++
		return pngImage(t, 2, 2), nil
	})
	long := Document{Subject: "x", Theme: neutral(), Blocks: []Block{
		{ID: "h", Type: "heading", Text: strings.Repeat(`"`, MaxRenderBytes/5+1)},
		{ID: "i", Type: "image", Src: "artifact:a1", Width: 10},
	}}
	if _, err := Render(ctx, long, counting, RenderOptions{}); !errors.Is(err, ErrTooLarge) || reads != 0 {
		t.Fatalf("want ErrTooLarge before the picture is read, got %v and %d reads", err, reads)
	}
	// A sample value used many times.
	d = oneBlock(Block{ID: "t", Type: "text", Text: strings.Repeat("{{displayName}} ", 20)})
	if _, err := Render(ctx, d, nil, RenderOptions{Sample: map[string]string{"displayName": strings.Repeat("x", 1<<20)}}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("sample: want ErrTooLarge, got %v", err)
	}
	// Custom markup past the ceiling, which only a Document built without
	// Parse can hold.
	custom := Document{Subject: "x", TextBody: "x", HTMLBody: strings.Repeat("a", MaxRenderBytes+1)}
	if _, err := Render(ctx, custom, nil, RenderOptions{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("custom: want ErrTooLarge, got %v", err)
	}
}

func TestLinkTagsCountOnlyAsLinks(t *testing.T) {
	// A link tag is a link only where it is an address: a button, a text
	// link, a linked picture or an html block's <a href>. In the preheader, an
	// alt, plain words or the query of another address it links nowhere.
	images := fakeImages{"a1": pngImage(t, 2, 2)}
	for _, c := range []struct {
		name      string
		preheader string
		blocks    func(tag string) []Block
		linked    bool
	}{
		{"a button", "", func(tag string) []Block { return []Block{{ID: "b", Type: "button", Label: "Go", Href: tag}} }, true},
		{"a text link", "", func(tag string) []Block { return []Block{{ID: "t", Type: "text", Text: "[Go](" + tag + ")"}} }, true},
		{"a linked picture", "", func(tag string) []Block {
			return []Block{{ID: "i", Type: "image", Src: "artifact:a1", Width: 10, Href: tag}}
		}, true},
		{"an html block link", "", func(tag string) []Block {
			return []Block{{ID: "x", Type: "html", HTML: `<p><a href="` + tag + `">Go</a></p>`}}
		}, true},
		{"the preheader", "TAG", func(string) []Block { return []Block{{ID: "h", Type: "heading", Text: "Hi"}} }, false},
		{"a picture's alt", "", func(tag string) []Block {
			return []Block{{ID: "i", Type: "image", Src: "artifact:a1", Width: 10, Alt: tag}}
		}, false},
		{"plain words", "", func(tag string) []Block { return []Block{{ID: "t", Type: "text", Text: "Go to " + tag}} }, false},
		{"a heading", "", func(tag string) []Block { return []Block{{ID: "h", Type: "heading", Text: tag}} }, false},
		{"a highlight", "", func(tag string) []Block { return []Block{{ID: "l", Type: "highlight", Text: tag}} }, false},
		{"a button's label", "", func(tag string) []Block {
			return []Block{{ID: "b", Type: "button", Label: tag, Href: "https://example.com"}}
		}, false},
		{"html block words", "", func(tag string) []Block { return []Block{{ID: "x", Type: "html", HTML: "<p>" + tag + "</p>"}} }, false},
		{"the query of another address", "", func(tag string) []Block {
			return []Block{{ID: "t", Type: "text", Text: "[Go](https://example.com/?next=" + tag + ")"}}
		}, false},
	} {
		for _, link := range []struct {
			tag, code string
			opts      RenderOptions
		}{
			{TagUnsubscribeURL, IssueUnsubscribeMissing, RenderOptions{}},
			{TagConfirmURL, IssueConfirmMissing, RenderOptions{RequireConfirm: true}},
		} {
			t.Run(c.name+"/"+link.code, func(t *testing.T) {
				preheader := strings.ReplaceAll(c.preheader, "TAG", link.tag)
				d := Document{Subject: "x", Preheader: preheader, Theme: neutral(), Blocks: c.blocks(link.tag)}
				r, err := Render(context.Background(), d, images, link.opts)
				if err != nil {
					t.Fatal(err)
				}
				if hasIssue(r.Issues, link.code) == c.linked {
					t.Fatalf("linked %v, issues %+v", c.linked, r.Issues)
				}
			})
		}
	}
}

func TestCustomModePreheaderKeepsTheDoctypeFirst(t *testing.T) {
	// Before a doctype the preheader would put the page in quirks mode. It
	// goes after the body tag; without one, after </head>, else after the
	// html tag, else after the doctype, and only else first.
	div := `<div style="display:none;font-size:1px;color:#ffffff;line-height:1px;max-height:0px;max-width:0px;opacity:0;overflow:hidden;mso-hide:all">Soon` +
		strings.Repeat("&#847;&zwnj;&nbsp;", 60) + `</div>`
	for _, c := range []struct{ name, before, after string }{
		{"after the body tag", `<!DOCTYPE html><html><head></head><body class="b">`, `<p>Hi</p></body></html>`},
		{"after the head", `<!DOCTYPE html><html><head><title>x</title></head>`, `<p>Hi</p></html>`},
		{"after the html tag", `<!DOCTYPE html><html lang="en">`, `<p>Hi</p></html>`},
		{"after the doctype", `<!DOCTYPE html>`, "\n<p>Hi</p>"},
		{"first, with none of them", ``, `<p>Hi</p>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := Document{Subject: "x", Preheader: "Soon", TextBody: "x", HTMLBody: c.before + c.after}
			r, err := Render(context.Background(), d, nil, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if want := c.before + div + c.after; r.HTML != want {
				t.Fatalf("\n got %q\nwant %q", r.HTML, want)
			}
		})
	}
}
