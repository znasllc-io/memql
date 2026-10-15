package emaildoc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The gallery of the VS Code email pages (editors/productivity/gallery, built by
// npm run gallery there) shows real renders: this test writes them. It is a
// developer tool and skips unless MEMQL_EMAIL_GALLERY_DIR names the directory
// to write into; the gallery's build sets it to a git-ignored directory, so the
// fixtures are never committed.
//
// Each file is one email as the pages receive it: the id, name and description
// of the layout it comes from, the document, and its render in preview mode
// (subject, text, issues and HTML with the block marks the Design page reads).
// The emails are the built-in layouts and nothing else, so they carry no one's
// copy, pictures, colours or fonts:
//   - <name>.json: each built-in layout as it ships, as New email lists it;
//   - <name>-finished.json: the layout as a finished email, every picture
//     chosen and every link given an address, so nothing keeps it from being
//     published;
//   - confirmation-hero-missing.json: the finished Confirmation with its hero
//     picture still to choose, one issue;
//   - custom.json: an email written in HTML.
//
// The pictures of a finished email are drawn here from its own theme colours
// (a landscape for a photo, a badge for a logo, a stroke for a signature), and
// its addresses are example.com ones.

const galleryDirVar = "MEMQL_EMAIL_GALLERY_DIR"

// galleryFixture is one file of the gallery, the fields the pages read.
type galleryFixture struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Document    json.RawMessage `json:"document"`
	Subject     string          `json:"subject"`
	Text        string          `json:"text"`
	Issues      []Issue         `json:"issues"`
	HTML        string          `json:"html"`
}

// galleryCustomHTML is the email written in HTML: a short note in the built-in
// brand, with merge tags for Preview with and the unsubscribe link sending needs.
const galleryCustomHTML = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Acme</title></head>
<body style="margin:0;padding:0;background:#eef0f3">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#eef0f3"><tr><td align="center" style="padding:32px 16px">
<table role="presentation" width="560" cellpadding="0" cellspacing="0" style="width:560px;max-width:100%;background:#ffffff">
<tr><td style="padding:28px 40px 4px;font:bold 20px Georgia,serif;letter-spacing:.12em;color:#1f5f8b">ACME</td></tr>
<tr><td style="padding:16px 40px 8px;font:28px/1.25 Georgia,serif;color:#1c1c1c">Your Saturday workshop seat is saved, {{displayName}}</td></tr>
<tr><td style="padding:8px 40px 20px;font:15px/1.6 Arial,sans-serif;color:#1c1c1c">Thanks for signing up with {{accountName}}. We start at ten, with coffee from half past nine. Bring one routine you would like to make easier.</td></tr>
<tr><td style="padding:0 40px 32px"><a href="https://example.com/workshop" style="display:inline-block;padding:12px 24px;border-radius:6px;background:#1f5f8b;color:#ffffff;font:bold 15px Arial,sans-serif;text-decoration:none">See the workshop details</a></td></tr>
<tr><td style="padding:18px 40px 28px;border-top:1px solid #e3e5e8;font:12px/1.6 Arial,sans-serif;color:#696e77">Acme, 123 Example Street, Springfield. <a href="{{unsubscribeUrl}}" style="color:#1f5f8b">Unsubscribe</a></td></tr>
</table></td></tr></table>
</body></html>
`

const galleryCustomText = `Your Saturday workshop seat is saved, {{displayName}}

Thanks for signing up with {{accountName}}. We start at ten, with coffee from half past nine. Bring one routine you would like to make easier.

See the workshop details: https://example.com/workshop

Acme, 123 Example Street, Springfield.
Unsubscribe: {{unsubscribeUrl}}
`

func TestWriteEditorGalleryFixtures(t *testing.T) {
	dir := os.Getenv(galleryDirVar)
	if dir == "" {
		t.Skip("set " + galleryDirVar + " to write the email gallery's fixtures (npm run gallery in editors/productivity does)")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pictures := galleryPictures{}
	write := func(file string, info LayoutInfo, d Document) Result {
		t.Helper()
		data, err := d.JSON()
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		// What the gallery shows is a document the editor and the engine accept.
		if _, err := Parse(data); err != nil {
			t.Fatalf("%s: not a valid email document: %v", file, err)
		}
		r, err := Render(context.Background(), d, pictures, RenderOptions{Preview: true})
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if d.Mode() == ModeLayout && !strings.Contains(r.HTML, `data-block="`) {
			t.Fatalf("%s: a preview render carries the block marks", file)
		}
		out, err := json.Marshal(galleryFixture{ID: info.ID, Name: info.Name, Description: info.Description, Document: data, Subject: r.Subject, Text: r.Text, Issues: r.Issues, HTML: r.HTML})
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if err := os.WriteFile(filepath.Join(dir, file+".json"), out, 0o644); err != nil {
			t.Fatal(err)
		}
		return r
	}
	issueList := func(r Result) string {
		var list []string
		for _, issue := range r.Issues {
			list = append(list, issue.Code+":"+issue.Block)
		}
		return strings.Join(list, " ")
	}

	layouts := Layouts()
	if len(layouts) != 4 {
		t.Fatalf("want the four built-in layouts, got %d", len(layouts))
	}
	for _, info := range layouts {
		name := strings.TrimPrefix(info.ID, "builtin:")
		// finish fills in the document it is given, so each email starts from a copy of its own.
		fresh := func() Document {
			d, _ := Layout(info.ID)
			return d
		}
		write(name, info, info.Document)
		if r := write(name+"-finished", info, pictures.finish(name, fresh())); len(r.Issues) != 0 {
			t.Errorf("%s: a finished email has no issues, got %s", name, issueList(r))
		}
		if name == "confirmation" {
			r := write("confirmation-hero-missing", info, pictures.finish(name, fresh(), "hero"))
			if got := issueList(r); got != IssueImageMissing+":hero" {
				t.Errorf("the Confirmation with its hero picture to choose has that one issue, got %s", got)
			}
		}
	}

	custom := Document{Subject: "Your workshop seat is saved", Preheader: "Saturday at ten, coffee from half past nine.", TextBody: galleryCustomText, HTMLBody: galleryCustomHTML}
	if r := write("custom", LayoutInfo{}, custom); len(r.Issues) != 0 {
		t.Errorf("the email written in HTML has no issues, got %s", issueList(r))
	}
}

// galleryPictures holds the pictures drawn for finished emails, by artifact id,
// and hands them to Render as the Library would.
type galleryPictures map[string]Image

func (p galleryPictures) ResolveImage(_ context.Context, id string) (Image, error) {
	if img, ok := p[id]; ok {
		return img, nil
	}
	return Image{}, errors.New("no such picture")
}

// finish makes d a finished email, in place, and returns it: each picture (but
// those of the blocks in missing) drawn and chosen, each empty link given an
// address.
func (p galleryPictures) finish(layout string, d Document, missing ...string) Document {
	var visit func(blocks []Block)
	visit = func(blocks []Block) {
		for i := range blocks {
			b := &blocks[i]
			switch b.Type {
			case typeImage:
				if b.Src == "" && !slices.Contains(missing, b.ID) {
					id := "gallery-" + layout + "-" + b.ID
					p[id] = Image{MIMEType: "image/png", Data: galleryPicture(d.Theme, b)}
					b.Src = "artifact:" + id
				}
			case typeButton:
				if b.Href == "" {
					b.Href = "https://example.com/" + b.ID
				}
			case typeSocial, typeFooter:
				for j := range b.Links {
					link := &b.Links[j]
					switch {
					case link.Href != "":
					case b.Type == typeSocial:
						link.Href = "https://example.com/" + strings.ToLower(link.Network)
					default:
						link.Href = "mailto:hello@example.com"
					}
				}
			case typeSection:
				visit(b.Blocks)
			case typeColumns:
				for j := range b.Columns {
					visit(b.Columns[j].Blocks)
				}
			}
		}
	}
	visit(d.Blocks)
	return d
}

// galleryPicture draws a picture for an image block at its own size, in the
// theme's colours: a stroke for a signature (a picture four times as wide as
// tall or more), a badge for a logo, and otherwise a landscape whose sun and
// hills move with the block's id, so no two photos are the same.
func galleryPicture(theme Theme, b *Block) []byte {
	w := max(b.Width, 1)
	h := int(math.Round(b.Height))
	if h < 1 {
		h = max(w/2, 1)
	}
	primary, secondary, accent, tint, text := galleryColour(theme.Primary), galleryColour(theme.Secondary), galleryColour(theme.Accent), galleryColour(theme.Tint), galleryColour(theme.Text)
	white := color.NRGBA{255, 255, 255, 255}
	fw, fh := float64(w), float64(h)
	seed := fnv.New32a()
	seed.Write([]byte(b.ID))
	shift := float64(seed.Sum32()%1000) / 1000

	var background func(y float64) color.NRGBA
	var layers []galleryLayer
	switch {
	case w >= 4*h:
		// A signature: a pen line with a few loops, on nothing.
		layers = []galleryLayer{{text, func(x, y float64) bool {
			if x < fh*0.3 || x > fw-fh*0.3 {
				return false
			}
			t := x / fw
			centre := fh*0.55 + fh*0.22*math.Sin(t*math.Pi*7+shift*6) - fh*0.08*math.Sin(t*math.Pi*2)
			return math.Abs(y-centre) < fh*0.06+0.6
		}}}
	case strings.Contains(b.ID, "logo"):
		// A logo: a mark and two lines of a wordmark on a primary badge.
		background = func(float64) color.NRGBA { return primary }
		r, cx, cy := fh*0.26, fh*0.62, fh/2
		left := cx + r + fh*0.28
		bar := func(top, length float64) func(x, y float64) bool {
			return func(x, y float64) bool { return x >= left && x <= left+length && y >= top && y <= top+fh*0.11 }
		}
		layers = []galleryLayer{
			{white, func(x, y float64) bool { return math.Hypot(x-cx, y-cy) <= r }},
			{primary, func(x, y float64) bool { return math.Hypot(x-cx-r*0.35, y-cy+r*0.35) <= r*0.45 }},
			{white, bar(cy-fh*0.17, math.Min(fw*0.42, fw-left-fh*0.3))},
			{white, bar(cy+fh*0.06, math.Min(fw*0.28, fw-left-fh*0.3))},
		}
	default:
		// A photo: a sky lighter at the top, a sun and two hills.
		background = func(y float64) color.NRGBA {
			return galleryMix(galleryMix(tint, white, 0.5), galleryMix(tint, secondary, 0.45), y/fh)
		}
		sunX, sunY, sunR := fw*(0.25+0.5*shift), fh*0.34, math.Min(fw, fh)*0.12
		layers = []galleryLayer{
			{galleryMix(accent, tint, 0.45), func(x, y float64) bool { return math.Hypot(x-sunX, y-sunY) <= sunR }},
			{galleryMix(secondary, primary, 0.3), func(x, y float64) bool { return y > fh*(0.6+0.07*math.Sin(x/fw*math.Pi*2.2+shift*5)) }},
			{primary, func(x, y float64) bool { return y > fh*(0.76+0.06*math.Sin(x/fw*math.Pi*1.6+2+shift*3)) }},
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, galleryPaint(w, h, background, layers)); err != nil {
		panic(err)
	}
	return out.Bytes()
}

// galleryLayer is a shape in one colour: the points inside is true at.
type galleryLayer struct {
	colour color.NRGBA
	inside func(x, y float64) bool
}

// galleryPaint draws the layers in order over the background (by row; nil is
// transparent), each pixel sampled four by four so edges are smooth.
func galleryPaint(w, h int, background func(y float64) color.NRGBA, layers []galleryLayer) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	const samples = 4
	for py := 0; py < h; py++ {
		var ground color.NRGBA
		if background != nil {
			ground = background(float64(py) + 0.5)
		}
		for px := 0; px < w; px++ {
			r, g, b, a := float64(ground.R), float64(ground.G), float64(ground.B), float64(ground.A)/255
			for _, layer := range layers {
				hits := 0
				for sy := 0; sy < samples; sy++ {
					for sx := 0; sx < samples; sx++ {
						if layer.inside(float64(px)+(float64(sx)+0.5)/samples, float64(py)+(float64(sy)+0.5)/samples) {
							hits++
						}
					}
				}
				cover := float64(hits) / (samples * samples)
				if cover == 0 {
					continue
				}
				// Source over, in straight alpha.
				outA := cover + a*(1-cover)
				blend := func(src uint8, dst float64) float64 { return (float64(src)*cover + dst*a*(1-cover)) / outA }
				r, g, b, a = blend(layer.colour.R, r), blend(layer.colour.G, g), blend(layer.colour.B, b), outA
			}
			img.SetNRGBA(px, py, color.NRGBA{uint8(math.Round(r)), uint8(math.Round(g)), uint8(math.Round(b)), uint8(math.Round(a * 255))})
		}
	}
	return img
}

// galleryColour reads a theme colour, "#rrggbb" (Parse has checked it is one).
func galleryColour(value string) color.NRGBA {
	n, err := strconv.ParseUint(strings.TrimPrefix(value, "#"), 16, 32)
	if err != nil {
		panic("emaildoc: a theme colour is #rrggbb, got " + value)
	}
	return color.NRGBA{uint8(n >> 16), uint8(n >> 8), uint8(n), 255}
}

// galleryMix is a and b mixed, share parts b (0 to 1).
func galleryMix(a, b color.NRGBA, share float64) color.NRGBA {
	share = math.Max(0, math.Min(1, share))
	at := func(x, y uint8) uint8 { return uint8(math.Round(float64(x)*(1-share) + float64(y)*share)) }
	return color.NRGBA{at(a.R, b.R), at(a.G, b.G), at(a.B, b.B), 255}
}
