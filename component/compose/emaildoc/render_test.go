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
	"regexp"
	"runtime"
	"strconv"
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

// A reader that will read no more pictures says so with ErrTooManyPictures, and
// the email is told it has too many pictures (once), not that a picture could
// not be read: the picture is fine, and the way on is to use fewer.
func TestRenderReportsPicturesAReaderWillNotReadAsTooMany(t *testing.T) {
	reads := map[string]int{}
	reader := resolverFunc(func(_ context.Context, id string) (Image, error) {
		reads[id]++
		if id == "a1" {
			return pngImage(t, 2, 2), nil
		}
		// Wrapped, as a reader that says more than the bare sentence would.
		return Image{}, fmt.Errorf("a preview reads nine pictures: %w", ErrTooManyPictures)
	})
	t.Run("a layout", func(t *testing.T) {
		clear(reads)
		d := oneBlock(Block{ID: "p1", Type: "image", Src: "artifact:a1", Width: 10})
		d.Blocks = append([]Block{
			{ID: "p2", Type: "image", Src: "artifact:b2", Width: 10, Alt: "Two"},
			{ID: "p3", Type: "image", Src: "artifact:b3", Width: 10},
			{ID: "p4", Type: "image", Src: "artifact:b2", Width: 10},
		}, d.Blocks...)
		r := mustRender(t, d, reader, RenderOptions{})
		if n := countIssues(r.Issues, IssueImagesLimit); n != 1 {
			t.Errorf("images_limit said %d times, want once: %+v", n, r.Issues)
		}
		if hasIssue(r.Issues, IssueImageUnavailable) {
			t.Errorf("a picture that was not read is reported as one that could not be: %+v", r.Issues)
		}
		for _, issue := range r.Issues {
			if issue.Code == IssueImagesLimit && issue.Message != picturesOverBudget {
				t.Errorf("message = %q, want %q", issue.Message, picturesOverBudget)
			}
		}
		if n := strings.Count(r.HTML, "data:image/png"); n != 1 {
			t.Errorf("%d pictures embedded, want only the one that was read", n)
		}
		// Each is asked for once, however often it is used.
		if reads["a1"] != 1 || reads["b2"] != 1 || reads["b3"] != 1 {
			t.Errorf("reads: %v", reads)
		}
	})
	t.Run("custom HTML", func(t *testing.T) {
		clear(reads)
		d := Document{Subject: "x", TextBody: "x", HTMLBody: `<img src="artifact:b2" alt="Two"><img src="artifact:a1"><img src="artifact:b3"><img src="artifact:b2">`}
		r := mustRender(t, d, reader, RenderOptions{})
		if n := countIssues(r.Issues, IssueImagesLimit); n != 1 || hasIssue(r.Issues, IssueImageUnavailable) {
			t.Errorf("issues: %+v", r.Issues)
		}
		// The ones not read stay as they were written, and the one read is embedded.
		if n := strings.Count(r.HTML, "data:image/png"); n != 1 || strings.Count(r.HTML, "artifact:b2") != 2 {
			t.Errorf("HTML: %s", r.HTML)
		}
		if reads["b2"] != 1 {
			t.Errorf("reads: %v", reads)
		}
	})
	t.Run("a render that is cancelled is cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cancelling := resolverFunc(func(context.Context, string) (Image, error) {
			cancel()
			return Image{}, ErrTooManyPictures
		})
		if _, err := Render(ctx, oneBlock(Block{ID: "p", Type: "image", Src: "artifact:z", Width: 10}), cancelling, RenderOptions{}); !errors.Is(err, context.Canceled) {
			t.Errorf("got %v", err)
		}
	})
	t.Run("only the sentence of this error", func(t *testing.T) {
		// Any other error is a picture that could not be read.
		other := resolverFunc(func(context.Context, string) (Image, error) { return Image{}, errors.New("too many requests") })
		r := mustRender(t, oneBlock(Block{ID: "p", Type: "image", Src: "artifact:z", Width: 10}), other, RenderOptions{})
		if !hasIssue(r.Issues, IssueImageUnavailable) || hasIssue(r.Issues, IssueImagesLimit) {
			t.Errorf("issues: %+v", r.Issues)
		}
	})
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

// drawnBlocks is the cells a render marks in a preview, in document order: one
// per block of a type the renderer draws, a container's before its blocks'.
func drawnBlocks(blocks []Block) []string {
	var ids []string
	for _, b := range blocks {
		switch b.Type {
		case typeImage, typeHeading, typeText, typeHighlight, typeButton, typeDivider, typeSpacer, typeHTML,
			typeSection, typeColumns, typeSocial, typeFooter:
			ids = append(ids, b.ID)
		}
		switch b.Type {
		case typeSection:
			ids = append(ids, drawnBlocks(b.Blocks)...)
		case typeColumns:
			for _, column := range b.Columns {
				ids = append(ids, drawnBlocks(column.Blocks)...)
			}
		}
	}
	return ids
}

var (
	classPattern      = regexp.MustCompile(`class="([^"]*)"`)
	shrinkPattern     = regexp.MustCompile(`\.efs-([0-9]+)\{font-size:([0-9.]+)px!important\}`)
	stackPattern      = regexp.MustCompile(`@media \(max-width:[0-9.]+px\)\{([^@]*?)\}\}`)
	stackClassPattern = regexp.MustCompile(`\.(c[0-9]+-[cs][0-9]+)[,{]`)
)

// checkPhoneRules checks the phone rules in the head of a layout email
// against the classes its body uses: a rule for every class the renderer
// marks a cell with and none for a class nothing uses. A shrink rule gives the
// size of the plan's formula, the rules are in ascending order of size, and
// there is one shrink style at most.
func checkPhoneRules(t testing.TB, markup string) {
	t.Helper()
	head, body, ok := strings.Cut(markup, "</head>")
	if !ok {
		t.Fatal("no head")
	}
	used := map[string]bool{}
	for _, m := range classPattern.FindAllStringSubmatch(body, -1) {
		for _, class := range strings.Fields(m[1]) {
			if class != "ecw" {
				used[class] = true
			}
		}
	}
	ruled := map[string]bool{}
	previous := 0
	for _, m := range shrinkPattern.FindAllStringSubmatch(head, -1) {
		tenths, _ := strconv.Atoi(m[1])
		if want := number(float64(160+(tenths-159)/2) / 10); m[2] != want {
			t.Errorf("efs-%s shrinks to %spx, want %spx", m[1], m[2], want)
		}
		if tenths <= previous {
			t.Errorf("efs-%s is out of order", m[1])
		}
		previous = tenths
		ruled["efs-"+m[1]] = true
	}
	if n := strings.Count(head, "@media(max-width:550px)"); n > 1 || (n == 1) != (previous > 0) {
		t.Errorf("want one shrink style when there are shrink rules, got %d", n)
	}
	for _, m := range stackPattern.FindAllStringSubmatch(head, -1) {
		for _, c := range stackClassPattern.FindAllStringSubmatch(m[1], -1) {
			ruled[c[1]] = true
		}
	}
	for class := range used {
		if !ruled[class] {
			t.Errorf("the body uses class %q, which the head has no rule for", class)
		}
	}
	for class := range ruled {
		if !used[class] {
			t.Errorf("the head has a rule for class %q, which the body does not use", class)
		}
	}
}

// classFree reports whether no html block of the blocks carries a class
// attribute of its own: an author's class is the author's, with no rule.
func classFree(blocks []Block) bool {
	for _, b := range blocks {
		if b.Type == typeHTML && strings.Contains(strings.ToLower(b.HTML), "class") {
			return false
		}
		if !classFree(b.Blocks) {
			return false
		}
		for _, column := range b.Columns {
			if !classFree(column.Blocks) {
				return false
			}
		}
	}
	return true
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
		`{"id":"s","type":"section","background":"tint","maxWidth":502,"blocks":[{"id":"h","type":"heading","text":"Hi","size":37.3},{"id":"c","type":"columns","columns":[{"width":50,"blocks":[{"id":"f","type":"social","title":"Follow","links":[{"network":"A","href":"https://example.com","icon":"artifact:a1"},{"network":"B","href":""}]}]},{"width":41.33,"blocks":[{"id":"t","type":"text","text":"x","size":18.7}]}]}]}`,
		`{"id":"c","type":"columns","background":"secondary","gap":8,"stackBelow":500,"columns":[{"width":56,"cellPadding":4,"blocks":[{"id":"i","type":"image","width":204,"src":"artifact:a1"}]},{"width":20,"blocks":[]},{"width":20}]}`,
		`{"id":"f","type":"social","links":[{"network":"A","href":"{{confirmUrl}}"},{"network":"B","href":"https://example.com","icon":"artifact:a1","width":40}],"maxWidth":300,"gap":0}`,
		`{"id":"f","type":"footer","links":[{"label":"u","href":"{{unsubscribeUrl}}"},{"label":"c"}],"lines":["Acme","1 Example Street"],"separator":" | ","gap":38.4,"after":22.4,"size":14.7,"font":"body"}`,
		// Text over 16 pixels with no words to write has no row to mark.
		`{"id":"t","type":"text","text":" \n ","size":19.9},{"id":"u","type":"text","text":"x","size":18.7},{"id":"h","type":"highlight","text":"","size":17.3}`,
		// Words in containers written to close their cell and forge a mark.
		`{"id":"f","type":"social","title":"</td></tr><tr><td data-block=\"evil\">","links":[{"network":"\"><td data-block=\"evil\">","href":""},{"network":"</a></td><td data-block=\"evil\">","href":"https://example.com","icon":"artifact:a1"}]},` +
			`{"id":"g","type":"footer","links":[{"label":"</span></a></td><td data-block=\"evil\">","href":""}],"lines":["</td></tr><tr><td data-block=\"evil\">"],"separator":"</span><td data-block=\"evil\">"}`,
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
		// The head has the phone rules the body's classes need, and no more.
		if classFree(d.Blocks) {
			checkPhoneRules(t, r.HTML)
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
	// of about six hundred. Parse refuses a text block of more than 2,000
	// lines, but a Document built without it can ask for hundreds of
	// mebibytes of HTML, and a highlight of many lines can through Parse.
	// Render stops at MaxRenderBytes, as it writes.
	//
	// Reading the Markdown costs what the input costs, once: text over 16
	// pixels, which the head marks to shrink, is not read for that. Writing
	// may cost a few times the ceiling (a growing buffer and the rows it is
	// made of), never what the input asks for, which is over a gigabyte here.
	// One paragraph of many lines stops at the ceiling too, line by line.
	items, paragraphs := strings.Repeat("- a\n", 300_000), strings.Repeat("a\n\n", 300_000)
	paragraph := strings.Repeat(strings.Repeat("&", 50)+"\n", 200_000)
	var renderErr error
	for _, size := range []float64{0, 18} {
		for _, c := range []struct{ name, text string }{{"list", items}, {"paragraphs", paragraphs}, {"one paragraph of many lines", paragraph}} {
			d := oneBlock(Block{ID: "t", Type: "text", Text: c.text, Size: size})
			parsing := allocatedBy(func() { _, _ = parseMarkdown(c.text) })
			rendering := allocatedBy(func() { _, renderErr = Render(ctx, d, nil, RenderOptions{}) })
			if !errors.Is(renderErr, ErrTooLarge) || rendering > parsing+8*MaxRenderBytes {
				t.Errorf("%s, size %v: want ErrTooLarge within the ceiling, got %v after %d bytes (%d of them reading the Markdown once)", c.name, size, renderErr, rendering, parsing)
			}
		}
	}
	// Highlight lines grow the same way.
	lines := strings.Repeat("a\n", 450_000)
	highlight, err := Parse([]byte(layoutJSON(`{"id":"l","type":"highlight","text":` + jsonString(lines) + `}`)))
	if err != nil {
		t.Fatal(err)
	}
	reading := allocatedBy(func() { _ = strings.Split(oneNewline(lines), "\n") })
	rendering := allocatedBy(func() { _, renderErr = Render(ctx, highlight, nil, RenderOptions{}) })
	if !errors.Is(renderErr, ErrTooLarge) || rendering > reading+8*MaxRenderBytes {
		t.Fatalf("highlight lines: want ErrTooLarge within the ceiling, got %v after %d bytes (%d of them reading)", renderErr, rendering, reading)
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
	d := oneBlock(Block{ID: "t", Type: "text", Text: strings.Repeat("{{displayName}} ", 20)})
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

// blockCell is the markup of the outer cell of the block with the given id in
// a preview: from its <td data-block="id" to the </td> that closes it.
func blockCell(t *testing.T, markup, id string) string {
	t.Helper()
	start := strings.Index(markup, `<td data-block="`+id+`"`)
	if start < 0 {
		t.Fatalf("no cell for %q in:\n%s", id, markup)
	}
	depth := 0
	for i := start; i < len(markup); {
		switch {
		case strings.HasPrefix(markup[i:], "</td>"):
			depth--
			i += len("</td>")
			if depth == 0 {
				return markup[start:i]
			}
		case strings.HasPrefix(markup[i:], "<td ") || strings.HasPrefix(markup[i:], "<td>"):
			depth++
			i += len("<td")
		default:
			i++
		}
	}
	t.Fatalf("the cell for %q does not close", id)
	return ""
}

func mustRender(t *testing.T, d Document, images ImageResolver, opts RenderOptions) Result {
	t.Helper()
	r, err := Render(context.Background(), d, images, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The frame of a container, as the plan's markup appendix writes it, and the
// cell of a column in it.
func frameOpen(pad, background, inset, max string) string {
	if background != "" {
		background = ";background-color:" + background
	}
	return `style="padding:` + pad + `"><table border="0" cellpadding="0" cellspacing="0" align="center" style="display:table;border-spacing:0px;border-collapse:separate;width:100%;max-width:100%;table-layout:fixed;margin:0 auto` + background +
		`"><tbody><tr><td style="text-align:center;padding:` + inset + `"><table border="0" cellpadding="0" cellspacing="0" style="border-spacing:0px;border-collapse:separate;width:100%;max-width:` + max + `px;table-layout:fixed;margin:0 auto"><tbody><tr>`
}

func columnOpen(width, class, cellPadding string) string {
	if class != "" {
		class = ` class="` + class + `"`
	}
	return `<td width="` + width + `%"` + class + ` style="width:` + width + `%;box-sizing:border-box;vertical-align:top"><table border="0" cellpadding="0" cellspacing="0" style="border-spacing:0px;border-collapse:separate;width:100%;table-layout:fixed"><tbody><tr><td style="padding:` + cellPadding + `px">`
}

func TestContainerDefaults(t *testing.T) {
	// A section holding two text blocks: the first has no padding at the
	// sides and 16 pixels below, the last none at all.
	d := oneBlock(Block{ID: "s", Type: "section", Blocks: []Block{
		{ID: "first", Type: "text", Text: "One"},
		{ID: "last", Type: "text", Text: "Two"},
	}})
	r := mustRender(t, d, nil, RenderOptions{Preview: true})
	for _, want := range []string{
		`<td data-block="first" style="padding:0px 0px 16px 0px">`,
		`<td data-block="last" style="padding:0px 0px 0px 0px">`,
		`<td data-block="s" ` + frameOpen("0px 0px 16px 0px", "", "0px 24px 0px 24px", "552") + columnOpen("100.00", "", "24"),
	} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("missing %s", want)
		}
	}
	// Each container type's own defaults at the top level.
	icons := fakeImages{"a1": pngImage(t, 2, 2)}
	for _, c := range []struct {
		name  string
		block Block
		want  []string
	}{
		{"columns", Block{ID: "x", Type: "columns", Columns: []Column{{Width: 50, Blocks: []Block{}}, {Width: 50, Blocks: []Block{}}}}, []string{
			`<td data-block="x" ` + frameOpen("0px 24px 16px 24px", "", "0px 0px 0px 0px", "552") + columnOpen("50.00", "c1-c0", "0"),
			`<td width="16" class="c1-s0" style="width:16px;box-sizing:border-box;font-size:0">&nbsp;</td>`,
			`<style>@media (max-width:450px){`,
		}},
		{"social", Block{ID: "x", Type: "social", Links: []Link{{Network: "N", Href: "https://example.com/n", Icon: "artifact:a1"}, {Network: "M", Href: "https://example.com/m"}}}, []string{
			`<td data-block="x" style="padding:0px 24px 16px 24px"><table align="center" width="100%" border="0" cellpadding="0" cellspacing="0" role="presentation" style="border-collapse:collapse"><tbody><tr><td ` +
				frameOpen("0px 0px 0px 0px", "", "0px 0px 0px 0px", "451") + columnOpen("48.23", "c1-c0", "0"),
			`<td width="16" class="c1-s0" style="width:16px;box-sizing:border-box;font-size:0">&nbsp;</td>`,
			`width="47" height="47" alt="N"`,
		}},
		{"footer", Block{ID: "x", Type: "footer", Links: []Link{{Label: "A", Href: "https://example.com/a"}, {Label: "B"}}, Lines: []string{"L"}}, []string{
			`<td data-block="x" ` + frameOpen("0px 0px 0px 0px", "#ffffff", "20px 24px 20px 24px", "552") + columnOpen("100.00", "", "13"),
			`<tr><td dir="ltr" style="font-size:14px;text-align:center;padding:0px 0px 0px;line-height:22px;mso-line-height-alt:22px">` +
				`<a href="https://example.com/a" target="_blank" rel="noopener nofollow" style="color:#5b8fb9;text-decoration:inherit"><span style="text-decoration:underline;color:#5b8fb9;white-space:pre-wrap">A</span></a>` +
				`<span style="color:#5b8fb9;white-space:pre-wrap">    |    </span><span style="text-decoration:underline;color:#5b8fb9;white-space:pre-wrap">B</span></td></tr>`,
			`<tr><td dir="ltr" style="color:#8a8f98;font-size:14px;white-space:pre-wrap;text-align:center;padding:0px 0px 0px;line-height:22px;mso-line-height-alt:22px">L</td></tr>`,
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := mustRender(t, oneBlock(c.block), icons, RenderOptions{Preview: true})
			for _, want := range c.want {
				if !strings.Contains(r.HTML, want) {
					t.Errorf("missing %s in:\n%s", want, r.HTML)
				}
			}
		})
	}
	// A container inside a section or a column takes the rule of the blocks
	// there, not its type's own default; spacers have no padding anywhere,
	// and a block's own padding wins everywhere.
	nested := Document{Subject: "x", Theme: neutral(), Blocks: []Block{
		{ID: "band", Type: "section", Blocks: []Block{
			{ID: "cols", Type: "columns", Columns: []Column{{Width: 100, Blocks: []Block{
				{ID: "follow", Type: "social", Links: []Link{{Network: "N", Href: "https://example.com"}}},
				{ID: "own", Type: "heading", Text: "x", Padding: &[4]float64{1, 2, 3, 4}},
				{ID: "gap", Type: "spacer", Height: 5},
			}}}},
			{ID: "follow-last", Type: "social", Links: []Link{{Network: "N", Href: "https://example.com"}}},
		}},
	}}
	r = mustRender(t, nested, nil, RenderOptions{Preview: true})
	for _, want := range []string{
		`<td data-block="cols" style="padding:0px 0px 16px 0px">`,
		`<td data-block="follow" style="padding:0px 0px 16px 0px">`,
		`<td data-block="own" dir="ltr" class="efs-280" style="color:#1f5f8b;font-size:28px;font-weight:700;font-family:Georgia, Arial, Helvetica, sans-serif;white-space:pre-wrap;text-align:center;padding:1px 2px 3px 4px;`,
		`<td data-block="gap" style="height:5px;line-height:5px;font-size:0;mso-line-height-rule:exactly">&nbsp;</td>`,
		`<td data-block="follow-last" style="padding:0px 0px 0px 0px">`,
	} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestPicturesInsideContainersMeasureTheirColumn(t *testing.T) {
	// Outlook gets a fixed-width table for a picture narrower than the room
	// it has. In a section that is the smaller of its maximum width and the
	// column less its padding and inset, less its cell padding on both
	// sides; in a column, its share of the container's width rounded down,
	// less its cell padding; and every block less its own padding.
	mso := func(width int) string {
		return fmt.Sprintf(`<!--[if mso]><table cellpadding="0" cellspacing="0" border="0" width="%d" style="width:%dpx">`, width, width)
	}
	images := fakeImages{"a1": pngImage(t, 2, 2)}
	picture := func(width int, padding *[4]float64) Block {
		return Block{ID: "i", Type: "image", Src: "artifact:a1", Width: width, Padding: padding}
	}
	for _, c := range []struct {
		name  string
		room  int // the widest picture that needs no Outlook table
		place func(Block) Block
	}{
		{"section defaults", 504, func(b Block) Block {
			return Block{ID: "s", Type: "section", Blocks: []Block{b}}
		}},
		{"section maximum width", 454, func(b Block) Block {
			return Block{ID: "s", Type: "section", MaxWidth: 502, Blocks: []Block{b}}
		}},
		{"section maximum width over the room", 504, func(b Block) Block {
			return Block{ID: "s", Type: "section", MaxWidth: 580, Blocks: []Block{b}}
		}},
		{"section padding, inset and cell padding", 566, func(b Block) Block {
			return Block{ID: "s", Type: "section", Padding: &[4]float64{0, 10, 0, 10}, Inset: &[4]float64{0, 5, 0, 5}, CellPadding: ptr(2), Blocks: []Block{b}}
		}},
		{"a block's own padding in a section", 484, func(b Block) Block {
			b.Padding = &[4]float64{0, 10, 0, 10}
			return Block{ID: "s", Type: "section", Blocks: []Block{b}}
		}},
		{"half of the columns", 276, func(b Block) Block {
			return Block{ID: "c", Type: "columns", Columns: []Column{{Width: 50, Blocks: []Block{b}}, {Width: 50, Blocks: []Block{}}}}
		}},
		{"a column's share is rounded down", 247, func(b Block) Block {
			return Block{ID: "c", Type: "columns", Padding: &[4]float64{0, 0, 0, 0}, MaxWidth: 600, Columns: []Column{{Width: 41.33, Blocks: []Block{b}}}}
		}},
		{"a column's cell padding", 260, func(b Block) Block {
			return Block{ID: "c", Type: "columns", Columns: []Column{{Width: 50, CellPadding: ptr(8), Blocks: []Block{b}}}}
		}},
		{"columns inset and maximum width", 200, func(b Block) Block {
			return Block{ID: "c", Type: "columns", Inset: &[4]float64{0, 50, 0, 50}, MaxWidth: 400, Columns: []Column{{Width: 50, Blocks: []Block{b}}}}
		}},
		{"columns in a section", 252, func(b Block) Block {
			return Block{ID: "s", Type: "section", Blocks: []Block{{ID: "c", Type: "columns", Columns: []Column{{Width: 50, Blocks: []Block{b}}}}}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, width := range []int{c.room, c.room - 1} {
				r := mustRender(t, oneBlock(c.place(picture(width, nil))), images, RenderOptions{})
				if got, want := strings.Contains(r.HTML, mso(width)), width < c.room; got != want {
					t.Errorf("a picture %d wide: Outlook table %v, want %v", width, got, want)
				}
			}
		})
	}
	// A social icon measures its share of the social block: two links in
	// 451 pixels with a gap of 16 are 48.23 percent each, 217 pixels.
	for _, width := range []int{217, 216} {
		social := oneBlock(Block{ID: "f", Type: "social", Links: []Link{{Network: "N", Href: "https://example.com", Icon: "artifact:a1", Width: width}, {Network: "M", Href: "https://example.com"}}})
		r := mustRender(t, social, images, RenderOptions{})
		if got, want := strings.Contains(r.HTML, mso(width)), width < 217; got != want {
			t.Errorf("an icon %d wide: Outlook table %v, want %v", width, got, want)
		}
	}
}

func TestColumnsStackOnPhones(t *testing.T) {
	two := func(id string) Block {
		return Block{ID: id, Type: "columns", Columns: []Column{{Width: 56, Blocks: []Block{}}, {Width: 41.33, Blocks: []Block{}}}}
	}
	d := oneBlock(two("first"))
	d.Blocks = append([]Block{d.Blocks[0], two("second")}, d.Blocks[1:]...)
	r := mustRender(t, d, nil, RenderOptions{})
	head, body, _ := strings.Cut(r.HTML, "</head>")
	for _, want := range []string{
		`<!--[if !mso]><!--><style>@media (max-width:450px){.c1-c0,.c1-c1{display:block!important;width:100%!important}.c1-s0{display:block!important;width:auto!important;height:16px;font-size:0}}</style><!--<![endif]-->`,
		`<!--[if !mso]><!--><style>@media (max-width:450px){.c2-c0,.c2-c1{display:block!important;width:100%!important}.c2-s0{display:block!important;width:auto!important;height:16px;font-size:0}}</style><!--<![endif]-->`,
	} {
		if !strings.Contains(head, want) {
			t.Errorf("the head is missing %s", want)
		}
	}
	for _, want := range []string{`width="56.00%" class="c1-c0"`, `width="41.33%" class="c1-c1"`, `class="c1-s0"`, `width="56.00%" class="c2-c0"`, `class="c2-s0"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the body is missing %s", want)
		}
	}
	if strings.Index(head, ".c1-c0") > strings.Index(head, ".c2-c0") {
		t.Error("the rules follow the document order")
	}
	checkPhoneRules(t, r.HTML)
}

func TestContainersAreNumberedInDocumentOrder(t *testing.T) {
	// Columns and social blocks are numbered from 1 in document order, a
	// container before the blocks in it. A container of one column has no gap
	// and so no gap rule; one of four has three; stackBelow moves its rule.
	link := func(n string) Link { return Link{Network: n, Href: "https://example.com/" + n} }
	empty := func(width float64) Column { return Column{Width: width, Blocks: []Block{}} }
	d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{
		{ID: "band", Type: "section", Blocks: []Block{{ID: "inner", Type: "columns", Columns: []Column{empty(50), empty(50)}}}},
		{ID: "follow", Type: "social", Links: []Link{link("a")}},
		{ID: "outer", Type: "columns", StackBelow: 520, Columns: []Column{
			{Width: 50, Blocks: []Block{{ID: "nested", Type: "social", Links: []Link{link("b"), link("c")}}}},
			empty(50),
		}},
		{ID: "four", Type: "columns", Columns: []Column{empty(25), empty(25), empty(25), empty(25)}},
	}}
	r := mustRender(t, d, nil, RenderOptions{})
	head, _, _ := strings.Cut(r.HTML, "</head>")
	rules := []string{
		`@media (max-width:450px){.c1-c0,.c1-c1{display:block!important;width:100%!important}.c1-s0{display:block!important;width:auto!important;height:16px;font-size:0}}`,
		`@media (max-width:450px){.c2-c0{display:block!important;width:100%!important}}`,
		`@media (max-width:520px){.c3-c0,.c3-c1{display:block!important;width:100%!important}.c3-s0{display:block!important;width:auto!important;height:16px;font-size:0}}`,
		`@media (max-width:450px){.c4-c0,.c4-c1{display:block!important;width:100%!important}.c4-s0{display:block!important;width:auto!important;height:16px;font-size:0}}`,
		`@media (max-width:450px){.c5-c0,.c5-c1,.c5-c2,.c5-c3{display:block!important;width:100%!important}.c5-s0,.c5-s1,.c5-s2{display:block!important;width:auto!important;height:16px;font-size:0}}`,
	}
	at := 0
	for _, rule := range rules {
		n := strings.Index(head[at:], rule)
		if n < 0 {
			t.Fatalf("missing, or out of order: %s\nin %s", rule, head)
		}
		at += n + len(rule)
	}
	if strings.Contains(head, ".c6-") || strings.Count(head, "@media (max-width:") != 6 { // the five and .ecw's
		t.Fatalf("want exactly five stacking rules:\n%s", head)
	}
	checkPhoneRules(t, r.HTML)
}

func TestLargeTextShrinksOnPhones(t *testing.T) {
	d := oneBlock(Block{ID: "a", Type: "heading", Text: "A", Size: 48})
	d.Blocks = append([]Block{d.Blocks[0], {ID: "b", Type: "heading", Text: "B", Size: 26.7}, {ID: "c", Type: "heading", Text: "C", Size: 14.7}}, d.Blocks[1:]...)
	r := mustRender(t, d, nil, RenderOptions{Preview: true})
	if !strings.Contains(r.HTML, `<td data-block="a" dir="ltr" class="efs-480" style=`) || !strings.Contains(r.HTML, `<td data-block="b" dir="ltr" class="efs-267" style=`) || !strings.Contains(r.HTML, `<td data-block="c" dir="ltr" style=`) {
		t.Fatalf("want efs-480, efs-267 and no class:\n%s", r.HTML)
	}
	head, _, _ := strings.Cut(r.HTML, "</head>")
	const rule = `<style>@media(max-width:550px){.efs-267{font-size:21.4px!important}.efs-480{font-size:32px!important}}</style>`
	if !strings.HasSuffix(head, rule) || strings.Count(r.HTML, "max-width:550px") != 1 {
		t.Fatalf("want the one shrink rule at the end of the head:\n%s", head)
	}
	checkPhoneRules(t, r.HTML)
}

func TestEverySizeOverSixteenPixelsShrinks(t *testing.T) {
	// One rule per size, in ascending order, for every block that marks text
	// to shrink: headings, text blocks with words, highlights and a social
	// block's title. Phone tenths are 160 + (tenths - 159) / 2, rounded down.
	d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{
		{ID: "a", Type: "heading", Text: "x", Size: 96},
		{ID: "b", Type: "heading", Text: "x", Size: 16.1},
		{ID: "c", Type: "text", Text: "x", Size: 17},
		{ID: "d", Type: "highlight", Text: "x", Size: 40},
		{ID: "e", Type: "heading", Text: "x"}, // 28 by default
		{ID: "f", Type: "heading", Text: "x", Size: 28},
		{ID: "g", Type: "social", Title: "Follow", Links: []Link{{Network: "N", Href: "https://example.com"}}},
		{ID: "h", Type: "text", Text: " \n ", Size: 50}, // no words, no row to mark
		{ID: "i", Type: "button", Label: "x", Href: "https://example.com", Size: 30},
		{ID: "j", Type: "footer", Size: 30, Lines: []string{"x"}, Links: []Link{{Label: "x"}}},
		{ID: "k", Type: "text", Text: "x", Size: 16},
	}}
	r := mustRender(t, d, nil, RenderOptions{})
	want := `<style>@media(max-width:550px){.efs-161{font-size:16.1px!important}.efs-170{font-size:16.5px!important}.efs-200{font-size:18px!important}.efs-280{font-size:22px!important}.efs-400{font-size:28px!important}.efs-960{font-size:56px!important}}</style></head>`
	if !strings.Contains(r.HTML, want) {
		t.Fatalf("want %s in:\n%s", want, r.HTML)
	}
	checkPhoneRules(t, r.HTML)
	// Without text over 16 pixels, the head has no shrink rule.
	plain := mustRender(t, oneBlock(Block{ID: "t", Type: "text", Text: "x"}), nil, RenderOptions{})
	if strings.Contains(plain.HTML, "efs-") || strings.Contains(plain.HTML, "max-width:550px") {
		t.Fatalf("want no shrink rule:\n%s", plain.HTML)
	}
}

func TestSocialWithoutIconsRendersTextLinks(t *testing.T) {
	d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{
		{ID: "follow", Type: "social", Links: []Link{{Network: "Instagram", Href: ""}}},
	}}
	r := mustRender(t, d, nil, RenderOptions{})
	if strings.Contains(r.HTML, "<a") || !strings.Contains(r.HTML, `<span style="color:#1f5f8b;text-decoration:underline">Instagram</span>`) {
		t.Fatalf("want the word in a span and no link:\n%s", r.HTML)
	}
	if want := (Issue{Code: IssueLinkMissing, Block: "follow", Message: `Add a link to "Instagram".`}); !hasIssue(r.Issues, IssueLinkMissing) || r.Issues[0] != want {
		t.Fatalf("want %+v first, got %+v", want, r.Issues)
	}
	// With an address it is a link; with an icon it is the picture.
	d.Blocks[0].Links = []Link{{Network: "Instagram", Href: "https://example.com/i"}, {Network: "Video", Href: "https://example.com/v", Icon: "artifact:a1"}}
	r = mustRender(t, d, fakeImages{"a1": pngImage(t, 2, 2)}, RenderOptions{})
	if !strings.Contains(r.HTML, `<a href="https://example.com/i" target="_blank" rel="noopener nofollow" style="color:#1f5f8b;text-decoration:underline">Instagram</a>`) ||
		!strings.Contains(r.HTML, `<a href="https://example.com/v" target="_blank" rel="noopener nofollow" style="display:block;text-decoration:none;border:none;outline:none" aria-label="Video">`) ||
		!strings.Contains(r.HTML, `alt="Video" style="display:block;width:47px;height:auto;max-width:100%">`) || hasIssue(r.Issues, IssueLinkMissing) {
		t.Fatalf("want a text link and an icon link, %+v:\n%s", r.Issues, r.HTML)
	}
}

func TestSocialIconsArePicturesLikeAnyOther(t *testing.T) {
	// An icon is read once, counts toward the eight pictures, and is drawn
	// as the network's text link when it cannot be sent, with the issue
	// naming the network.
	reads := map[string]int{}
	images := resolverFunc(func(_ context.Context, id string) (Image, error) {
		reads[id]++
		if id == "gone" {
			return Image{}, errors.New("not readable")
		}
		return pngImage(t, 2+len(id), 2), nil
	})
	d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{{ID: "follow", Type: "social", Links: []Link{
		{Network: "One", Href: "https://example.com/1", Icon: "artifact:i1"},
		{Network: "Again", Href: "https://example.com/2", Icon: "artifact:i1"},
		{Network: "Gone", Href: "https://example.com/3", Icon: "artifact:gone"},
	}}}}
	r := mustRender(t, d, images, RenderOptions{})
	if reads["i1"] != 1 || reads["gone"] != 1 {
		t.Fatalf("reads: %v", reads)
	}
	want := Issue{Code: IssueImageUnavailable, Block: "follow", Message: `The picture for "Gone" could not be read. Choose it again.`}
	if countIssues(r.Issues, IssueImageUnavailable) != 1 || !hasIssue(r.Issues, IssueImageUnavailable) {
		t.Fatalf("want %+v, got %+v", want, r.Issues)
	}
	for _, issue := range r.Issues {
		if issue.Code == IssueImageUnavailable && issue != want {
			t.Fatalf("want %+v, got %+v", want, issue)
		}
	}
	if strings.Count(r.HTML, `<img src="data:`) != 2 || !strings.Contains(r.HTML, `<a href="https://example.com/3" target="_blank" rel="noopener nofollow" style="color:#1f5f8b;text-decoration:underline">Gone</a>`) {
		t.Fatalf("want two icons and a text link:\n%s", r.HTML)
	}
	// Eight picture blocks and a ninth picture as an icon are nine.
	library := fakeImages{}
	var blocks []Block
	for n := 0; n < 8; n++ {
		id := fmt.Sprintf("p%d", n)
		library[id] = pngImage(t, 3+n, 2)
		blocks = append(blocks, Block{ID: "i" + id, Type: "image", Src: "artifact:" + id, Width: 10})
	}
	library["icon"] = pngImage(t, 30, 2)
	blocks = append(blocks, Block{ID: "follow", Type: "social", Links: []Link{{Network: "N", Href: "https://example.com", Icon: "artifact:icon"}}})
	d = oneBlock(blocks[0])
	d.Blocks = append(blocks, d.Blocks[1:]...)
	r = mustRender(t, d, library, RenderOptions{})
	if want := (Issue{Code: IssueImagesLimit, Block: "follow", Message: "Use up to eight pictures totaling 1 MiB."}); len(r.Issues) != 1 || r.Issues[0] != want {
		t.Fatalf("want only %+v, got %+v", want, r.Issues)
	}
}

func TestEveryPictureOccurrenceIsGuarded(t *testing.T) {
	// A social block of eight links with one large icon is one picture, but
	// eight occurrences of it: only those that fit within what sending takes
	// are embedded, the rest are text links, and the email says so once.
	big := bigPicture(t)
	var links []Link
	for n := 0; n < 8; n++ {
		links = append(links, Link{Network: fmt.Sprintf("N%d", n), Href: "https://example.com", Icon: "artifact:big"})
	}
	d := oneBlock(Block{ID: "follow", Type: "social", Links: links})
	var r Result
	var err error
	allocated := allocatedBy(func() { r, err = Render(context.Background(), d, fakeImages{"big": big}, RenderOptions{}) })
	if err != nil || len(r.Issues) != 1 || r.Issues[0] != emailTooLarge {
		t.Fatalf("want only %+v, got %+v %v", emailTooLarge, r.Issues, err)
	}
	if strings.Count(r.HTML, `<img src="data:`) != 1 || strings.Count(r.HTML, `text-decoration:underline">N`) != 7 || len(r.HTML) > sendingCap || allocated > 64<<20 {
		t.Fatalf("%d icons embedded, %d bytes of HTML, %d bytes allocated", strings.Count(r.HTML, `<img src="data:`), len(r.HTML), allocated)
	}
	// A picture block and an icon of the same picture share the budget too.
	both := oneBlock(Block{ID: "i", Type: "image", Src: "artifact:big", Width: 100})
	both.Blocks = append([]Block{both.Blocks[0], {ID: "follow", Type: "social", Links: links[:1]}}, both.Blocks[1:]...)
	r = mustRender(t, both, fakeImages{"big": big}, RenderOptions{})
	if strings.Count(r.HTML, `<img src="data:`) != 1 || !strings.Contains(r.HTML, `text-decoration:underline">N0</a>`) || countIssues(r.Issues, IssueEmailTooLarge) != 1 {
		t.Fatalf("want the picture, the icon as a text link and one issue: %+v", r.Issues)
	}
}

func TestFooterText(t *testing.T) {
	d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{{ID: "f", Type: "footer",
		Links: []Link{{Label: "Contact us", Href: "mailto:hello@example.com"}, {Label: "Unsubscribe", Href: "{{unsubscribeUrl}}"}},
		Lines: []string{"Acme", "123 Example Street"},
	}}}
	r := mustRender(t, d, nil, RenderOptions{})
	if want := "Contact us: mailto:hello@example.com\nUnsubscribe: {{unsubscribeUrl}}\nAcme\n123 Example Street\n"; r.Text != want {
		t.Fatalf("got %q, want %q", r.Text, want)
	}
}

// TestFooterLinkWithoutAnAddressIsReported pins that a footer link, which is
// drawn as a link with its words underlined, is reported when it has no
// address, as a button or a social link without one is: the same code and
// wording, naming the footer and the link's label, and nothing else about the
// email changes.
func TestFooterLinkWithoutAnAddressIsReported(t *testing.T) {
	long := strings.Repeat("L", 70)
	d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{
		{ID: "cta", Type: "button", Label: "Go"},
		{ID: "legal", Type: "footer", Links: []Link{
			{Label: "Contact us"},
			{Label: "Unsubscribe", Href: TagUnsubscribeURL},
			{Label: "Privacy", Href: ""},
			{Label: long},
		}},
	}}
	r := mustRender(t, d, nil, RenderOptions{})
	want := []Issue{
		{Code: IssueLinkMissing, Block: "cta", Message: `Add a link to "Go".`},
		{Code: IssueLinkMissing, Block: "legal", Message: `Add a link to "Contact us".`},
		{Code: IssueLinkMissing, Block: "legal", Message: `Add a link to "Privacy".`},
		{Code: IssueLinkMissing, Block: "legal", Message: `Add a link to "` + strings.Repeat("L", 60) + `...".`},
	}
	if len(r.Issues) != len(want) {
		t.Fatalf("want %+v, got %+v", want, r.Issues)
	}
	for i := range want {
		if r.Issues[i] != want[i] {
			t.Errorf("issue %d: want %+v, got %+v", i, want[i], r.Issues[i])
		}
	}
	// The words stay as they were: underlined in a span of their own, not in a
	// link, and the link beside them still a link.
	if !strings.Contains(r.HTML, `<span style="text-decoration:underline;color:#5b8fb9;white-space:pre-wrap">Contact us</span><span style="color:#5b8fb9;white-space:pre-wrap">    |    </span><a href="{{unsubscribeUrl}}"`) {
		t.Fatalf("want the words of a link without an address left as they are:\n%s", r.HTML)
	}
	if want := "Go\n\nContact us\nUnsubscribe: {{unsubscribeUrl}}\nPrivacy\n" + long + "\n"; r.Text != want {
		t.Fatalf("text part: got %q, want %q", r.Text, want)
	}
	// Given an address each, or with no links at all, a footer reports nothing.
	d.Blocks[0].Href = "https://example.com/go"
	d.Blocks[1].Links = []Link{{Label: "Contact us", Href: "mailto:hello@example.com"}, {Label: "Unsubscribe", Href: TagUnsubscribeURL}}
	d.Blocks = append(d.Blocks, Block{ID: "bare", Type: "footer", Lines: []string{"Acme"}})
	if r = mustRender(t, d, nil, RenderOptions{}); len(r.Issues) != 0 {
		t.Fatalf("want a footer with every address given, and one without links, to be quiet: %+v", r.Issues)
	}
}

func TestTextPartOfContainers(t *testing.T) {
	d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{
		{ID: "intro", Type: "heading", Text: "Intro"},
		{ID: "band", Type: "section", Blocks: []Block{
			{ID: "band-title", Type: "heading", Text: "In a section"},
			{ID: "cols", Type: "columns", Columns: []Column{
				{Width: 50, Blocks: []Block{{ID: "left", Type: "text", Text: "Left **one**\n\n- item"}, {ID: "pic", Type: "image", Width: 10}}},
				{Width: 50, Blocks: []Block{{ID: "right", Type: "button", Label: "Right", Href: "https://example.com/r"}}},
			}},
		}},
		{ID: "follow", Type: "social", Title: "Follow us", Links: []Link{
			{Network: "One", Href: "https://example.com/1"},
			{Network: "Two"},
			{Network: "Bad", Href: "javascript:alert(1)"}, // only a Document built without Parse
		}},
		{ID: "quiet", Type: "social", Links: []Link{{Network: "Three", Href: "{{viewUrl}}"}}},
		{ID: "legal", Type: "footer", Links: []Link{{Label: "Plain"}, {Label: "Bad", Href: "javascript:alert(1)"}}, Lines: []string{"Line\r\none", "Two"}},
		{ID: "bare", Type: "footer"},
	}}
	r := mustRender(t, d, nil, RenderOptions{})
	want := "Intro\n\nIn a section\n\nLeft one\n\n- item\n\nRight: https://example.com/r\n\nFollow us\n\nOne: https://example.com/1\nTwo\nBad\n\nThree: {{viewUrl}}\n\nPlain\nBad\nLine\none\nTwo\n"
	if r.Text != want {
		t.Fatalf("text part:\n got %q\nwant %q", r.Text, want)
	}
}

func TestSocialAndFooterLinksCountAsLinks(t *testing.T) {
	// A footer or social link is a link like any other: a link tag in its
	// address counts, in its words it does not, and an address that breaks
	// the link rule is never written.
	for _, c := range []struct {
		name   string
		block  Block
		code   string
		opts   RenderOptions
		linked bool
	}{
		{"a footer link", Block{ID: "f", Type: "footer", Links: []Link{{Label: "Leave", Href: TagUnsubscribeURL}}}, IssueUnsubscribeMissing, RenderOptions{}, true},
		{"a social link", Block{ID: "s", Type: "social", Links: []Link{{Network: "Yes", Href: TagConfirmURL}}}, IssueConfirmMissing, RenderOptions{RequireConfirm: true}, true},
		{"a social icon link", Block{ID: "s", Type: "social", Links: []Link{{Network: "Yes", Href: TagConfirmURL, Icon: "artifact:a1"}}}, IssueConfirmMissing, RenderOptions{RequireConfirm: true}, true},
		{"a button in a column", Block{ID: "c", Type: "columns", Columns: []Column{{Width: 100, Blocks: []Block{{ID: "b", Type: "button", Label: "Go", Href: TagConfirmURL}}}}}, IssueConfirmMissing, RenderOptions{RequireConfirm: true}, true},
		{"a text link in a section", Block{ID: "s", Type: "section", Blocks: []Block{{ID: "t", Type: "text", Text: "[Leave](" + TagUnsubscribeURL + ")"}}}, IssueUnsubscribeMissing, RenderOptions{}, true},
		{"a footer line", Block{ID: "f", Type: "footer", Lines: []string{TagUnsubscribeURL}}, IssueUnsubscribeMissing, RenderOptions{}, false},
		{"a footer label", Block{ID: "f", Type: "footer", Links: []Link{{Label: TagUnsubscribeURL, Href: "https://example.com"}}}, IssueUnsubscribeMissing, RenderOptions{}, false},
		{"a footer separator", Block{ID: "f", Type: "footer", Separator: textPtr(TagUnsubscribeURL), Links: []Link{{Label: "a"}, {Label: "b"}}}, IssueUnsubscribeMissing, RenderOptions{}, false},
		{"a social network", Block{ID: "s", Type: "social", Links: []Link{{Network: TagConfirmURL, Href: "https://example.com"}}}, IssueConfirmMissing, RenderOptions{RequireConfirm: true}, false},
		{"a social title", Block{ID: "s", Type: "social", Title: TagConfirmURL, Links: []Link{{Network: "N", Href: "https://example.com"}}}, IssueConfirmMissing, RenderOptions{RequireConfirm: true}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{c.block}}
			r := mustRender(t, d, fakeImages{"a1": pngImage(t, 2, 2)}, c.opts)
			if hasIssue(r.Issues, c.code) == c.linked {
				t.Fatalf("linked %v, issues %+v", c.linked, r.Issues)
			}
		})
	}
	// Built without Parse, a link that breaks the link rule is its words.
	d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{
		{ID: "f", Type: "footer", Links: []Link{{Label: "Bad", Href: "javascript:alert(1)"}, {Label: "Leave", Href: TagUnsubscribeURL}}},
		{ID: "s", Type: "social", Links: []Link{{Network: "Worse", Href: "javascript:alert(2)"}, {Network: "Icon", Href: "javascript:alert(3)", Icon: "artifact:a1"}}},
	}}
	r := mustRender(t, d, fakeImages{"a1": pngImage(t, 2, 2)}, RenderOptions{})
	// Three links have no usable address: the footer's and both of the social block's.
	if strings.Contains(r.HTML, "javascript") || strings.Contains(r.Text, "javascript") || countIssues(r.Issues, IssueLinkMissing) != 3 || hasIssue(r.Issues, IssueUnsubscribeMissing) {
		t.Fatalf("a bad address reached the email, or was not missing: %+v\n%s", r.Issues, r.HTML)
	}
}

func TestContainersEscapeTheirWords(t *testing.T) {
	const hostile = `<b>"x" & 'y'</b>`
	const escaped = `&lt;b&gt;&#34;x&#34; &amp; &#39;y&#39;&lt;/b&gt;`
	for _, b := range []Block{
		{ID: "a", Type: "social", Title: hostile, Links: []Link{{Network: "N", Href: "https://example.com"}}},
		{ID: "a", Type: "social", Links: []Link{{Network: hostile, Href: "https://example.com"}}},
		{ID: "a", Type: "social", Links: []Link{{Network: hostile}}},
		{ID: "a", Type: "social", Links: []Link{{Network: hostile, Href: "https://example.com", Icon: "artifact:a1"}}},
		{ID: "a", Type: "footer", Links: []Link{{Label: hostile, Href: "https://example.com"}}},
		{ID: "a", Type: "footer", Links: []Link{{Label: hostile}}},
		{ID: "a", Type: "footer", Separator: textPtr(hostile), Links: []Link{{Label: "a"}, {Label: "b"}}},
		{ID: "a", Type: "footer", Lines: []string{hostile}},
	} {
		r := mustRender(t, oneBlock(b), fakeImages{"a1": pngImage(t, 2, 2)}, RenderOptions{})
		if strings.Contains(r.HTML, "<b>") || !strings.Contains(r.HTML, escaped) {
			t.Errorf("%+v reached the HTML unescaped:\n%s", b, r.HTML)
		}
	}
}

func TestPreviewMarksContainersAndTheirBlocks(t *testing.T) {
	d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{
		{ID: "band", Type: "section", Blocks: []Block{
			{ID: "cols", Type: "columns", Columns: []Column{{Width: 50, Blocks: []Block{{ID: "left", Type: "text", Text: "L"}}}, {Width: 50, Blocks: []Block{{ID: "right", Type: "heading", Text: "R"}}}}},
		}},
		{ID: "follow", Type: "social", Title: "Follow", Links: []Link{{Network: "N", Href: "https://example.com"}}},
		{ID: "legal", Type: "footer", Lines: []string{"x"}},
	}}
	r := mustRender(t, d, nil, RenderOptions{Preview: true})
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
	if got := strings.Join(marked, " "); got != "band cols left right follow legal" {
		t.Fatalf("marked %s", got)
	}
	// The title of a social block is not a block of its own.
	if cell := blockCell(t, r.HTML, "follow"); !strings.Contains(cell, `<tr><td dir="ltr" class="efs-200" style=`) {
		t.Fatalf("want the title row unmarked in:\n%s", cell)
	}
}

func TestTheSendingCapIsExact(t *testing.T) {
	// The HTML may be exactly what sending takes; one byte more is
	// email_too_large.
	heading := func(n int) Document { return oneBlock(Block{ID: "h", Type: "heading", Text: strings.Repeat("a", n)}) }
	base := mustRender(t, heading(1), nil, RenderOptions{})
	fit := sendingCap - len(base.HTML) + 1
	r := mustRender(t, heading(fit), nil, RenderOptions{})
	if len(r.HTML) != sendingCap || len(r.Issues) != 0 {
		t.Fatalf("%d bytes: want %d and no issue, got %+v", len(r.HTML), sendingCap, r.Issues)
	}
	r = mustRender(t, heading(fit+1), nil, RenderOptions{})
	if len(r.HTML) != sendingCap+1 || len(r.Issues) != 1 || r.Issues[0] != emailTooLarge {
		t.Fatalf("%d bytes: want only %+v, got %+v", len(r.HTML), emailTooLarge, r.Issues)
	}
}

func TestAPictureIsEmbeddedOnlyWithinTheSendingCap(t *testing.T) {
	// A picture is embedded when the HTML up to its last byte is within what
	// sending takes, and not when it would be one byte past it. The phone
	// rules in the head come before it, and count: forty columns blocks put
	// a few kilobytes of them there.
	img := pngImage(t, 2, 2)
	uri := dataURI(img.Data)
	images := fakeImages{"a1": img}
	doc := func(n int) Document {
		var blocks []Block
		for i := 0; i < 40; i++ {
			blocks = append(blocks, Block{ID: fmt.Sprintf("c%d", i), Type: "columns", Columns: []Column{{Width: 50, Blocks: []Block{}}, {Width: 50, Blocks: []Block{}}}})
		}
		blocks = append(blocks, Block{ID: "h", Type: "heading", Text: strings.Repeat("a", n)}, Block{ID: "i", Type: "image", Src: "artifact:a1", Width: 10, Alt: "Late"})
		d := oneBlock(blocks[0])
		d.Blocks = append(blocks, d.Blocks[1:]...)
		return d
	}
	base := mustRender(t, doc(1), images, RenderOptions{})
	at := strings.Index(base.HTML, uri)
	if at < 0 || strings.Index(base.HTML, "</head>") < 40*200 {
		t.Fatalf("want the picture after a head of phone rules")
	}
	fit := sendingCap - (at + len(uri)) + 1
	r := mustRender(t, doc(fit), images, RenderOptions{})
	if end := strings.Index(r.HTML, uri) + len(uri); end != sendingCap {
		t.Fatalf("want the picture to end at %d, it ends at %d", sendingCap, end)
	}
	r = mustRender(t, doc(fit+1), images, RenderOptions{})
	if strings.Contains(r.HTML, uri) || !strings.Contains(r.HTML, ">Late</div>") || countIssues(r.Issues, IssueEmailTooLarge) != 1 {
		t.Fatalf("one byte later: want the placeholder and one issue, %+v", r.Issues)
	}
	// And the same for an icon: the HTML up to its last byte.
	social := func(n int) Document {
		d := doc(n)
		d.Blocks[41] = Block{ID: "f", Type: "social", Links: []Link{{Network: "Late", Href: "https://example.com", Icon: "artifact:a1"}}}
		return d
	}
	base = mustRender(t, social(1), images, RenderOptions{})
	fit = sendingCap - (strings.Index(base.HTML, uri) + len(uri)) + 1
	r = mustRender(t, social(fit), images, RenderOptions{})
	if end := strings.Index(r.HTML, uri) + len(uri); end != sendingCap {
		t.Fatalf("icon: want the picture to end at %d, it ends at %d", sendingCap, end)
	}
	r = mustRender(t, social(fit+1), images, RenderOptions{})
	if strings.Contains(r.HTML, uri) || !strings.Contains(r.HTML, `text-decoration:underline">Late</a>`) || countIssues(r.Issues, IssueEmailTooLarge) != 1 {
		t.Fatalf("icon one byte later: want the text link and one issue, %+v", r.Issues)
	}
}

func TestCustomSendingCapIsExact(t *testing.T) {
	custom := func(body, preheader string) Result {
		return mustRender(t, Document{Subject: "x", Preheader: preheader, TextBody: "x", HTMLBody: body}, fakeImages{"a1": pngImage(t, 2, 2)}, RenderOptions{})
	}
	check := func(name string, r Result, size int, over bool) {
		t.Helper()
		if len(r.HTML) != size || (len(r.Issues) == 1 && r.Issues[0] == emailTooLarge) != over || (!over && len(r.Issues) != 0) {
			t.Errorf("%s: %d bytes, want %d, over %v, issues %+v", name, len(r.HTML), size, over, r.Issues)
		}
	}
	check("the body alone", custom(strings.Repeat("a", sendingCap), ""), sendingCap, false)
	check("the body alone, one more", custom(strings.Repeat("a", sendingCap+1), ""), sendingCap+1, true)
	div := len(custom("a", "Soon").HTML) - 1
	check("with a preheader", custom(strings.Repeat("a", sendingCap-div), "Soon"), sendingCap, false)
	check("with a preheader, one more", custom(strings.Repeat("a", sendingCap-div+1), "Soon"), sendingCap+1, true)
	// A Library picture is embedded when the HTML it makes is within what
	// sending takes; one byte more and its source stays.
	img := `<img src="` + dataURI(tinyPNG(t, 2, 2)) + `">`
	body := func(n int) string { return strings.Repeat("a", n) + `<img src="artifact:a1">` }
	r := custom(body(sendingCap-len(img)), "")
	if len(r.HTML) != sendingCap || len(r.Issues) != 0 || !strings.HasSuffix(r.HTML, img) {
		t.Errorf("picture: %d bytes, issues %+v", len(r.HTML), r.Issues)
	}
	r = custom(body(sendingCap-len(img)+1), "")
	if !strings.HasSuffix(r.HTML, `<img src="artifact:a1">`) || len(r.Issues) != 1 || r.Issues[0] != emailTooLarge {
		t.Errorf("picture, one more: issues %+v", r.Issues)
	}
}

func TestTheCeilingIsExact(t *testing.T) {
	ctx := context.Background()
	// A layout of exactly MaxRenderBytes of HTML is written (and more than
	// sending takes); one byte more is ErrTooLarge.
	heading := func(n int) Document { return oneBlock(Block{ID: "h", Type: "heading", Text: strings.Repeat("a", n)}) }
	base := mustRender(t, heading(1), nil, RenderOptions{})
	fit := MaxRenderBytes - len(base.HTML) + 1
	r, err := Render(ctx, heading(fit), nil, RenderOptions{})
	if err != nil || len(r.HTML) != MaxRenderBytes || len(r.Issues) != 1 || r.Issues[0] != emailTooLarge {
		t.Fatalf("layout: %d bytes, %+v, %v", len(r.HTML), r.Issues, err)
	}
	if _, err := Render(ctx, heading(fit+1), nil, RenderOptions{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("layout, one more: want ErrTooLarge, got %v", err)
	}
	// Filling a merge tag makes a part longer: exactly MaxRenderBytes once
	// filled is written, one byte more is ErrTooLarge.
	tagged := oneBlock(Block{ID: "h", Type: "heading", Text: "{{displayName}}"})
	sample := func(n int) RenderOptions {
		return RenderOptions{Sample: map[string]string{"displayName": strings.Repeat("a", n)}}
	}
	base = mustRender(t, tagged, nil, sample(1))
	fit = MaxRenderBytes - len(base.HTML) + 1
	if r, err := Render(ctx, tagged, nil, sample(fit)); err != nil || len(r.HTML) != MaxRenderBytes {
		t.Fatalf("filled: %d bytes, %v", len(r.HTML), err)
	}
	if _, err := Render(ctx, tagged, nil, sample(fit+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("filled, one more: want ErrTooLarge, got %v", err)
	}
	// Custom markup and text, which only a Document built without Parse can
	// make this large.
	for _, c := range []struct {
		name string
		doc  func(n int) Document
	}{
		{"custom html", func(n int) Document { return Document{Subject: "x", TextBody: "x", HTMLBody: strings.Repeat("a", n)} }},
		{"custom text", func(n int) Document { return Document{Subject: "x", TextBody: strings.Repeat("a", n), HTMLBody: "x"} }},
	} {
		if _, err := Render(ctx, c.doc(MaxRenderBytes), nil, RenderOptions{}); err != nil {
			t.Errorf("%s of exactly the ceiling: %v", c.name, err)
		}
		if _, err := Render(ctx, c.doc(MaxRenderBytes+1), nil, RenderOptions{}); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s, one more: want ErrTooLarge, got %v", c.name, err)
		}
	}
}

func TestContainerChildrenAreCounted(t *testing.T) {
	ctx := context.Background()
	// The blocks inside a container count toward the ceiling as they are
	// written: past it, nothing after them is written or read.
	items := strings.Repeat("- a\n", 300_000) // only a Document built without Parse
	reads := 0
	counting := resolverFunc(func(context.Context, string) (Image, error) {
		reads++
		return pngImage(t, 2, 2), nil
	})
	late := Block{ID: "late", Type: "image", Src: "artifact:a1", Width: 10}
	for _, c := range []struct {
		name  string
		block Block
	}{
		{"in a section", Block{ID: "s", Type: "section", Blocks: []Block{{ID: "t", Type: "text", Text: items}, late}}},
		{"in a column", Block{ID: "c", Type: "columns", Columns: []Column{{Width: 50, Blocks: []Block{{ID: "t", Type: "text", Text: items}}}, {Width: 50, Blocks: []Block{late}}}}},
	} {
		reads = 0
		_, err := Render(ctx, Document{Subject: "x", Theme: neutral(), Blocks: []Block{c.block, late}}, counting, RenderOptions{})
		if !errors.Is(err, ErrTooLarge) || reads != 0 {
			t.Errorf("%s: want ErrTooLarge before the picture is read, got %v and %d reads", c.name, err, reads)
		}
	}
	// So do the links and lines of one social or footer block, the columns of
	// one columns block, and the depth of containers in containers: none of
	// them can make a render write much past the ceiling.
	var many []Link
	for n := 0; n < 200_000; n++ {
		many = append(many, Link{Label: "a", Network: "a", Href: "https://example.com"})
	}
	columns := make([]Column, 200_000)
	for n := range columns {
		columns[n] = Column{Width: 1, Blocks: []Block{}}
	}
	deep := Block{ID: "bottom", Type: "text", Text: "x"}
	for n := 0; n < 20_000; n++ {
		deep = Block{ID: "s", Type: "section", Blocks: []Block{deep}}
	}
	for _, c := range []struct {
		name  string
		block Block
	}{
		{"footer links", Block{ID: "f", Type: "footer", Links: many}},
		{"footer lines", Block{ID: "f", Type: "footer", Lines: strings.Split(strings.Repeat("a\n", 4_000_000), "\n")}},
		{"social links", Block{ID: "s", Type: "social", Links: many}},
		{"columns", Block{ID: "c", Type: "columns", Columns: columns}},
		{"nested sections", deep},
	} {
		var err error
		allocated := allocatedBy(func() {
			_, err = Render(ctx, Document{Subject: "x", Theme: neutral(), Blocks: []Block{c.block}}, nil, RenderOptions{})
		})
		if !errors.Is(err, ErrTooLarge) || allocated > 16*MaxRenderBytes {
			t.Errorf("%s: want ErrTooLarge within the ceiling, got %v after %d bytes", c.name, err, allocated)
		}
	}
	// The phone rules in the head count as they are written too: past the
	// ceiling there, no block is written and no picture read.
	thousand := make([]Link, 1000)
	for n := range thousand {
		thousand[n] = Link{Network: "n", Href: "https://example.com"}
	}
	blocks := []Block{late}
	for n := 0; n < 400; n++ {
		blocks = append(blocks, Block{ID: fmt.Sprintf("s%d", n), Type: "social", Links: thousand})
	}
	reads = 0
	if _, err := Render(ctx, Document{Subject: "x", Theme: neutral(), Blocks: blocks}, counting, RenderOptions{}); !errors.Is(err, ErrTooLarge) || reads != 0 {
		t.Errorf("phone rules: want ErrTooLarge before the picture is read, got %v and %d reads", err, reads)
	}
}

func TestCustomLinksCountOnlyAsWritten(t *testing.T) {
	// Sending fills a link tag by replacing its text in the markup, as
	// written, so only an address written as the tag itself is the link. One
	// written with character references is never filled.
	for _, c := range []struct {
		body   string
		linked bool
	}{
		{`<a href="{{confirmUrl}}">Confirm</a>`, true},
		{`<a href=" {{confirmUrl}} ">Confirm</a>`, true},
		{`<a HREF='{{confirmUrl}}'>Confirm</a>`, true},
		{`<a href={{confirmUrl}}>Confirm</a>`, true},
		{`<a href="&#123;&#123;confirmUrl&#125;&#125;">Confirm</a>`, false},
		{`<a href="&lbrace;&lbrace;confirmUrl&rbrace;&rbrace;">Confirm</a>`, false},
		{`<a href="{{confirm&#85;rl}}">Confirm</a>`, false},
		{`<a href="&#32;{{confirmUrl}}">Confirm</a>`, false},
		{`<a title="{{confirmUrl}}" href="&#123;&#123;confirmUrl&#125;&#125;">Confirm</a>`, false},
		{`<a href="https://example.com/" href="{{confirmUrl}}">Confirm</a>`, false},
		{`<p title="{{confirmUrl}}">x</p>`, false},
	} {
		r := mustRender(t, Document{Subject: "x", TextBody: "x", HTMLBody: c.body}, nil, RenderOptions{RequireConfirm: true})
		if hasIssue(r.Issues, IssueConfirmMissing) == c.linked {
			t.Errorf("%s: linked %v, issues %+v", c.body, c.linked, r.Issues)
		}
	}
}

func TestThePictureBudgetIsExact(t *testing.T) {
	// Pictures of exactly 1 MiB together are within the budget, and so is
	// one picture of exactly 1 MiB, Library or embedded; one byte more is
	// images_limit. A PNG is read by its header, so bytes after its end make
	// a picture of any size.
	sized := func(n, w int) Image {
		data := tinyPNG(t, w, 2)
		if n < len(data) {
			t.Fatalf("a picture of %d bytes is too small", n)
		}
		return Image{MIMEType: "image/png", Data: append(data, make([]byte, n-len(data))...)}
	}
	half := MaxImageBytes / 2
	for _, c := range []struct {
		name   string
		images fakeImages
		srcs   []string
		over   bool
	}{
		{"two pictures of 1 MiB together", fakeImages{"a": sized(half, 2), "b": sized(MaxImageBytes-half, 3)}, []string{"artifact:a", "artifact:b"}, false},
		{"two pictures one byte over", fakeImages{"a": sized(half, 2), "b": sized(MaxImageBytes-half+1, 3)}, []string{"artifact:a", "artifact:b"}, true},
		{"one picture of 1 MiB", fakeImages{"a": sized(MaxImageBytes, 2)}, []string{"artifact:a"}, false},
		{"one picture one byte over", fakeImages{"a": sized(MaxImageBytes+1, 2)}, []string{"artifact:a"}, true},
		{"an embedded picture of 1 MiB", nil, []string{dataURI(sized(MaxImageBytes, 2).Data)}, false},
		{"an embedded picture one byte over", nil, []string{dataURI(sized(MaxImageBytes+1, 2).Data)}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var blocks []Block
			for i, src := range c.srcs {
				blocks = append(blocks, Block{ID: fmt.Sprintf("i%d", i), Type: "image", Src: src, Width: 10})
			}
			d := oneBlock(blocks[0])
			d.Blocks = append(blocks, d.Blocks[1:]...)
			r := mustRender(t, d, c.images, RenderOptions{})
			if hasIssue(r.Issues, IssueImagesLimit) != c.over || (!c.over && len(r.Issues) != 0) {
				t.Fatalf("over the budget %v, got %+v", c.over, r.Issues)
			}
		})
	}
}

func TestSocialGapsWiderThanTheBlockLeaveNoRoom(t *testing.T) {
	// Parse admits gaps of up to 200 pixels and any maximum width, so a
	// social block's gaps can be wider than the block: its links then have
	// no room at all, never less than none.
	d := oneBlock(Block{ID: "f", Type: "social", MaxWidth: 100, Gap: ptr(200), Links: []Link{{Network: "A", Href: "https://example.com/a"}, {Network: "B", Href: "https://example.com/b"}}})
	r := mustRender(t, d, nil, RenderOptions{})
	if strings.Contains(r.HTML, `width="-`) || strings.Count(r.HTML, `width="0.00%"`) != 2 {
		t.Fatalf("want two columns of no width:\n%s", r.HTML)
	}
}

func TestSocialIconsMeasureANarrowColumn(t *testing.T) {
	// In a column narrower than its maximum width, a social block's links
	// share the column: in half of the columns there are 276 pixels, 133
	// for each of two links.
	mso := func(width int) string {
		return fmt.Sprintf(`<!--[if mso]><table cellpadding="0" cellspacing="0" border="0" width="%d" style="width:%dpx">`, width, width)
	}
	for _, width := range []int{133, 132} {
		social := Block{ID: "f", Type: "social", Links: []Link{{Network: "N", Href: "https://example.com", Icon: "artifact:a1", Width: width}, {Network: "M", Href: "https://example.com"}}}
		d := oneBlock(Block{ID: "c", Type: "columns", Columns: []Column{{Width: 50, Blocks: []Block{social}}, {Width: 50, Blocks: []Block{}}}})
		r := mustRender(t, d, fakeImages{"a1": pngImage(t, 2, 2)}, RenderOptions{})
		if got, want := strings.Contains(r.HTML, mso(width)), width < 133; got != want {
			t.Errorf("an icon %d wide: Outlook table %v, want %v", width, got, want)
		}
	}
}

func TestContainersWithoutColumnsHaveNoPhoneRule(t *testing.T) {
	// Only a Document built without Parse has columns without a column or a
	// social block without a link. They have nothing to stack and no rule,
	// but they still take their numbers.
	d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{
		{ID: "c", Type: "columns"},
		{ID: "s", Type: "social"},
		{ID: "n", Type: "columns", Columns: []Column{{Width: 100, Blocks: []Block{}}}},
	}}
	r := mustRender(t, d, nil, RenderOptions{})
	head, _, _ := strings.Cut(r.HTML, "</head>")
	if strings.Contains(head, "){{") || strings.Contains(head, ".c1-") || strings.Contains(head, ".c2-") ||
		!strings.Contains(head, `@media (max-width:450px){.c3-c0{display:block!important;width:100%!important}}`) {
		t.Fatalf("want one rule, for the third container:\n%s", head)
	}
	checkPhoneRules(t, r.HTML)
}

func TestUnknownBlockTypesWriteNothing(t *testing.T) {
	// Only a Document built without Parse has a block of a type Parse does
	// not know: it has no row, no mark and no words.
	r := mustRender(t, oneBlock(Block{ID: "x", Type: "marquee", Text: "Hello"}), nil, RenderOptions{Preview: true})
	if strings.Contains(r.HTML, "<tr></tr>") || strings.Contains(r.HTML, `data-block="x"`) || strings.Contains(r.HTML+r.Text, "Hello") {
		t.Fatalf("an unknown block was written:\n%s\n%s", r.HTML, r.Text)
	}
}

func TestContainersNeverHaveANegativeWidth(t *testing.T) {
	// Parse admits padding and insets of up to 200 pixels a side, which can
	// leave a section or columns less than no room: their maximum width is
	// then none, never less.
	for _, b := range []Block{
		{ID: "s", Type: "section", Padding: &[4]float64{0, 200, 0, 200}, Inset: &[4]float64{0, 200, 0, 200}},
		{ID: "c", Type: "columns", Padding: &[4]float64{0, 200, 0, 200}, Inset: &[4]float64{0, 200, 0, 200}, Columns: []Column{{Width: 100, Blocks: []Block{}}}},
	} {
		r := mustRender(t, oneBlock(b), nil, RenderOptions{})
		if strings.Contains(r.HTML, "max-width:-") || !strings.Contains(r.HTML, "max-width:0px;table-layout:fixed;margin:0 auto") {
			t.Errorf("%s: want a maximum width of none:\n%s", b.Type, r.HTML)
		}
	}
}

func TestLinkLabelsInTheTextPartHaveOneKindOfLineBreak(t *testing.T) {
	d := Document{Subject: "x", Theme: neutral(), Blocks: []Block{
		{ID: "b", Type: "button", Label: "Two\r\nlines", Href: "https://example.com/b"},
		{ID: "s", Type: "social", Links: []Link{{Network: "Net\rwork", Href: "https://example.com/s"}}},
		{ID: "f", Type: "footer", Links: []Link{{Label: "Foot\r\nnote"}}},
	}}
	r := mustRender(t, d, nil, RenderOptions{})
	if want := "Two\nlines: https://example.com/b\n\nNet\nwork: https://example.com/s\n\nFoot\nnote\n"; r.Text != want {
		t.Fatalf("got %q, want %q", r.Text, want)
	}
}

func TestLinkTagsAroundOtherSpacesAreNotLinks(t *testing.T) {
	// A browser strips only C0 controls and the space from the ends of an
	// address. Any other space stays in it, so the address is broken once
	// sending fills the tag, and it is not the link.
	for _, c := range []struct {
		href   string
		linked bool
	}{
		{" {{confirmUrl}}", false},
		{"　{{confirmUrl}}　", false},
		{"{{confirmUrl}} ", false},
		{"\u0085{{confirmUrl}}", false},
		{" \t\n{{confirmUrl}}\r\f ", true},
		{"\x01{{confirmUrl}}\x1f", true},
	} {
		body := `<a href="` + c.href + `">Confirm</a>`
		r := mustRender(t, Document{Subject: "x", TextBody: "x", HTMLBody: body}, nil, RenderOptions{RequireConfirm: true})
		if hasIssue(r.Issues, IssueConfirmMissing) == c.linked {
			t.Errorf("%q: linked %v, issues %+v", c.href, c.linked, r.Issues)
		}
	}
}

// formattedTextCost is the most Parse and Render together may allocate for
// one of the worst documents Parse accepts under its budget for formatted
// text (see TestFormattedTextCostIsBounded). The worst of the table measured
// 228 MiB: a paragraph filling the document with a merge tag every 20 bytes,
// filled with a 50-byte sample value, its one "(" (from "](") making the
// reader build its table of parentheses, its HTML just under the ceiling, and
// the rest of the mark budget spent in another block (2026-10-11, go1.27.2,
// darwin/arm64). The bound is 1.5x that, rounded up to the next 16 MiB.
const formattedTextCost = 352 << 20

func TestFormattedTextCostIsBounded(t *testing.T) {
	// The worst documents Parse accepts under the budget for formatted text,
	// read and written: whatever a member writes in an email, reading it and
	// writing it out stays within a bound.
	items := func(n int, item string) string { return strings.Repeat("- "+item+"\n", n) }
	runs := func(n int) string { return strings.Repeat("*a* ", n) }
	var fifty []string
	for n := 0; n < 50; n++ {
		fifty = append(fifty, runs(100))
	}
	footer := `{"id":"f","type":"footer","links":[{"label":"u","href":"{{unsubscribeUrl}}"}]}`
	// raw writes a JSON string as it is: & and < need no escape in JSON.
	raw := func(s string) string { return `"` + s + `"` }
	document := func(size string, texts ...string) []byte {
		var blocks []string
		for n, text := range texts {
			blocks = append(blocks, fmt.Sprintf(`{"id":"t%d","type":"text"%s,"text":%s}`, n, size, text))
		}
		return []byte(layoutJSON(strings.Join(append(blocks, footer), ",")))
	}
	quoted := func(texts ...string) []string {
		out := make([]string, len(texts))
		for n, text := range texts {
			out[n] = jsonString(text)
		}
		return out
	}
	// A document whose first text block is a paragraph as long as the
	// document can hold (prefix, then unit as many times as fit), with the
	// other texts in blocks after it.
	filled := func(size, prefix, unit string, others ...string) []byte {
		room := MaxBytes - len(document(size, append([]string{raw(prefix)}, others...)...))
		return document(size, append([]string{raw(prefix + strings.Repeat(unit, room/len(unit)))}, others...)...)
	}
	// The mark budget spent in a block of its own, all but the one "(" of a
	// paragraph's "](".
	spent := jsonString(runs(4999))
	preview := RenderOptions{Preview: true}
	sample := RenderOptions{Preview: true, Sample: map[string]string{"displayName": strings.Repeat("a", 50)}}
	worst := 0
	for _, size := range []string{"", `,"size":18`} {
		for _, c := range []struct {
			name string
			doc  []byte
			opts RenderOptions
		}{
			{"one paragraph of 5,000 emphasis runs", document(size, quoted(runs(5000))...), preview},
			{"5,000 list items", document(size, quoted(items(2000, "a"), items(2000, "a"), items(1000, "a"))...), preview},
			{"5,000 list items and 10,000 marks", document(size, quoted(items(2000, "*a*"), items(2000, "*a*"), items(1000, "*a*"))...), preview},
			{"50 blocks of 100 emphasis runs on one line", document(size, quoted(fifty...)...), preview},
			{"a plain paragraph of 2 MiB", filled(size, "", "a"), preview},
			{"a paragraph of 2 MiB that escapes to five times its size", filled(size, "", "&"), preview},
			// The one "(" of "](" makes the reader build its table of the
			// parentheses it pairs, as long as the text; with "&&)" the HTML
			// comes just under the ceiling, so all of it is written, and the
			// text part too.
			{"one open parenthesis, its table, and HTML near the ceiling", filled(size, "](", "&&)"), preview},
			{"the same, and the rest of the mark budget in another block", filled(size, "](", "&&)", spent), preview},
			// A merge tag every 20 bytes filled with a 50-byte sample value:
			// the HTML and the text part are written again, filled.
			{"merge tags filled near the ceiling, and the mark budget spent", filled(size, "](", "&&&&&{{displayName}}", spent), sample},
		} {
			var parseErr, renderErr error
			allocated := allocatedBy(func() {
				var d Document
				if d, parseErr = Parse(c.doc); parseErr == nil {
					_, renderErr = Render(context.Background(), d, nil, c.opts)
				}
			})
			if parseErr != nil || (renderErr != nil && !errors.Is(renderErr, ErrTooLarge)) {
				t.Fatalf("%s%s: want it accepted and rendered, got %v and %v", c.name, size, parseErr, renderErr)
			}
			t.Logf("%s%s: %d bytes, %d MiB allocated, render error %v", c.name, size, len(c.doc), allocated>>20, renderErr)
			worst = max(worst, int(allocated))
			if allocated > formattedTextCost {
				t.Errorf("%s%s: %d bytes allocated, the bound is %d", c.name, size, allocated, formattedTextCost)
			}
		}
	}
	t.Logf("worst: %d MiB", worst>>20)
	// Each open parenthesis is a mark: 2 MiB of them after "](" (which cost
	// 409 MiB to read and write when they were not) is refused at once.
	var err error
	doc := filled("", "](", "(")
	if allocated := allocatedBy(func() { _, err = Parse(doc) }); err == nil || err.Error() != "This email has too much formatted text. Shorten it or split it into more emails." || allocated > 64<<20 {
		t.Fatalf("2 MiB of open parentheses: want the budget's refusal, cheaply, got %v after %d bytes", err, allocated)
	}
}
