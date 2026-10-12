package emaildoc

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
)

// builtinCatalog is the catalogue of the plan's Appendix B, in the order
// Layouts lists it: the four built-in layouts with the words that name and
// describe them, and the subject and preheader each one carries.
var builtinCatalog = []struct {
	id, name, description, subject, preheader string
}{
	{"builtin:confirmation", "Confirmation", "A welcome on your brand colour with one button that confirms the subscription.", "Please confirm your subscription", "One click and you're on the list."},
	{"builtin:letter", "Letter", "A personal note with a signature band, a promotion and social links.", "A note from our founder", "Why we started, and what comes next."},
	{"builtin:article", "Article", "A banner, a tinted article body with a highlighted statement, and a signature.", "Three small changes for calmer mornings", "Pick one and try it tomorrow."},
	{"builtin:spotlight", "Spotlight", "One large picture, a short message and one button.", "Meet Ace", "Our guide to getting the small things done."},
}

// builtinTheme is the neutral example brand's palette and fonts, which the
// four layouts share; the Confirmation puts its second colour on the canvas.
var builtinTheme = Theme{
	Page: "#eef0f3", Canvas: "#ffffff", Primary: "#1f5f8b", Secondary: "#5b8fb9", Accent: "#d9534f",
	Tint: "#f6f4ef", Text: "#2b2b2b", Muted: "#8a8f98", HeadingFont: "Georgia", BodyFont: "Arial",
}

func TestLayoutsCatalog(t *testing.T) {
	layouts := Layouts()
	if len(layouts) != len(builtinCatalog) {
		t.Fatalf("want %d built-in layouts, got %d", len(builtinCatalog), len(layouts))
	}
	for i, want := range builtinCatalog {
		got := layouts[i]
		if got.ID != want.id || got.Name != want.name || got.Description != want.description {
			t.Errorf("layout %d: want %q %q %q, got %q %q %q", i, want.id, want.name, want.description, got.ID, got.Name, got.Description)
		}
		d := got.Document
		if d.Mode() != ModeLayout || d.Layout != got.ID {
			t.Errorf("%s: want a layout-mode document that names itself, got mode %d and layout %q", got.ID, d.Mode(), d.Layout)
		}
		if d.Subject != want.subject || d.Preheader != want.preheader {
			t.Errorf("%s: want subject %q and preheader %q, got %q and %q", got.ID, want.subject, want.preheader, d.Subject, d.Preheader)
		}
		again, ok := Layout(got.ID)
		if !ok || !reflect.DeepEqual(again, d) {
			t.Errorf("Layout(%q) must return the document Layouts lists (found %v)", got.ID, ok)
		}
	}
	for _, id := range []string{"builtin:nope", "", "letter", "BUILTIN:LETTER", "org:letter", "builtin:letter "} {
		if _, ok := Layout(id); ok {
			t.Errorf("Layout(%q) must not find a layout", id)
		}
	}
}

// fileDocument parses the embedded file of a built-in layout directly: a
// document that nothing in layouts.go has had a hand in, to compare with.
func fileDocument(t *testing.T, id string) Document {
	t.Helper()
	data, err := layoutFiles.ReadFile("layouts/" + strings.TrimPrefix(id, "builtin:") + ".email.json")
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestLayoutsAreCopies pins that a caller can change what it is given. A
// Document holds pointers and slices, and the Materializer merges the model's
// words into the layout it is handed; if that were the one copy every request
// shares, one organization's email would show up in the next blank one.
func TestLayoutsAreCopies(t *testing.T) {
	scribble := func(blocks []Block) {
		for i := range blocks {
			b := &blocks[i]
			b.Text += " changed"
			b.Label += " changed"
			b.Alt += " changed"
			if b.Padding != nil {
				b.Padding[0]++
			}
			if b.Inset != nil {
				b.Inset[0]++
			}
			if b.Gap != nil {
				*b.Gap++
			}
			for j := range b.Blocks {
				b.Blocks[j].Text += " changed"
				b.Blocks[j].Alt += " changed"
			}
			for j := range b.Columns {
				for k := range b.Columns[j].Blocks {
					b.Columns[j].Blocks[k].Text += " changed"
					b.Columns[j].Blocks[k].Alt += " changed"
				}
			}
			for j := range b.Links {
				b.Links[j].Href = "https://changed.example"
				b.Links[j].Label += " changed"
			}
			for j := range b.Lines {
				b.Lines[j] += " changed"
			}
		}
	}
	for _, want := range builtinCatalog {
		pristine := fileDocument(t, want.id)
		mine, _ := Layout(want.id)
		scribble(mine.Blocks)
		mine.Subject, mine.Theme.Primary = "changed", "#000000"
		if again, _ := Layout(want.id); !reflect.DeepEqual(again, pristine) {
			t.Errorf("%s: changing the document Layout returned changed the layout for everyone", want.id)
		}
		listed := Layouts()
		for i := range listed {
			scribble(listed[i].Document.Blocks)
			listed[i].Name = "changed"
		}
		if again, _ := Layout(want.id); !reflect.DeepEqual(again, pristine) {
			t.Errorf("%s: changing the documents Layouts returned changed the layout for everyone", want.id)
		}
	}
	if Layouts()[0].Name != builtinCatalog[0].name {
		t.Errorf("changing the list Layouts returned changed the next list")
	}
}

// linkAllowed reports whether a built-in layout may carry this address: none
// yet, or one of the four link merge tags. A made-up web or email address would
// be sent as it is, and nothing would tell the person to replace it; an empty
// one is reported as link_missing until they do.
func linkAllowed(href string) bool {
	if href == "" {
		return true
	}
	for _, tag := range LinkTags() {
		if href == tag {
			return true
		}
	}
	return false
}

func TestLayoutsAreNeutral(t *testing.T) {
	for _, info := range Layouts() {
		d := info.Document
		t.Run(info.ID, func(t *testing.T) {
			wantTheme := builtinTheme
			if info.ID == "builtin:confirmation" {
				wantTheme.Canvas = builtinTheme.Secondary
			}
			if d.Theme != wantTheme {
				t.Errorf("theme: want %+v, got %+v", wantTheme, d.Theme)
			}
			if d.Theme.HeadingFont != "Georgia" || d.Theme.BodyFont != "Arial" || d.Theme.WebFonts {
				t.Errorf("fonts must be Georgia and Arial with no web font link, got %q and %q (webFonts %v)", d.Theme.HeadingFont, d.Theme.BodyFont, d.Theme.WebFonts)
			}
			footers := 0
			walkBlocks(d.Blocks, func(b Block) bool {
				if b.Type == typeImage && b.Src != "" {
					t.Errorf("block %q: a built-in picture is a placeholder, got src %q", b.ID, b.Src)
				}
				if b.Type == typeImage && b.Height == 0 {
					t.Errorf("block %q: a placeholder picture needs a height", b.ID)
				}
				if !linkAllowed(b.Href) {
					t.Errorf("block %q: link %q must be empty or a link merge tag", b.ID, b.Href)
				}
				if b.Type == typeText && strings.Contains(b.Text, "](") {
					t.Errorf("block %q: a Markdown link has an address, and a built-in has none to give", b.ID)
				}
				for _, link := range b.Links {
					if link.Icon != "" {
						t.Errorf("block %q: link %q has an icon %q; no third-party logo ships", b.ID, link.Network, link.Icon)
					}
					if !linkAllowed(link.Href) {
						t.Errorf("block %q: link %q has the address %q; it must be empty or a link merge tag", b.ID, link.Network+link.Label, link.Href)
					}
				}
				if b.Type == typeFooter {
					footers++
					hrefs := map[string]bool{}
					for _, link := range b.Links {
						hrefs[link.Href] = true
					}
					for _, tag := range []string{TagUnsubscribeURL, TagPreferencesURL, TagViewURL} {
						if !hrefs[tag] {
							t.Errorf("footer %q: no link to %s", b.ID, tag)
						}
					}
				}
				return true
			})
			if footers != 1 {
				t.Errorf("want one footer, got %d", footers)
			}
		})
	}
}

// roleList lists "id=role" for every block of blocks, depth first in document
// order, with each block's role as the Materializer reads it: its own, else the
// role of the container it sits in, else content at the top level.
func roleList(blocks []Block, container string) []string {
	var out []string
	for _, b := range blocks {
		role := b.Role
		if role == "" {
			role = container
		}
		out = append(out, b.ID+"="+role)
		out = append(out, roleList(b.Blocks, role)...)
		for _, column := range b.Columns {
			out = append(out, roleList(column.Blocks, role)...)
		}
	}
	return out
}

// builtinRoles is the order of the blocks and who owns each one, as the plan's
// Appendix B lists them: brand blocks keep their content when a person or the
// model fills a layout, content blocks are what gets written.
var builtinRoles = map[string]string{
	"builtin:confirmation": "top=brand logo=brand rule-top=brand headline=content subhead=content hero=content confirm=content note=content rule-bottom=brand footer=brand",
	"builtin:letter": "logo=brand letter=content headline=content body=content cta=content " +
		"signature=brand signature-photo=brand signature-space=brand signature-name=brand signature-title=brand " +
		"rule-1=brand promo=brand promo-space=brand promo-title=brand promo-text=brand promo-picture=brand " +
		"follow=brand social=brand rule-2=brand footer=brand",
	"builtin:article": "logo=brand banner=content body=content intro=content highlight=content article=content " +
		"question=brand question-more=brand ask=brand " +
		"signature=brand signature-photo=brand signature-space=brand signature-name=brand signature-title=brand " +
		"rule=brand footer=brand",
	"builtin:spotlight": "logo=brand spotlight=content message-box=content message=content cta=content follow=brand social=brand footer=brand",
}

func TestLayoutsSplitBrandAndContent(t *testing.T) {
	for _, info := range Layouts() {
		d := info.Document
		t.Run(info.ID, func(t *testing.T) {
			roles := roleList(d.Blocks, "content")
			if got := strings.Join(roles, " "); got != builtinRoles[info.ID] {
				t.Errorf("blocks and roles:\n got %s\nwant %s", got, builtinRoles[info.ID])
			}
			byID := map[string]string{}
			for _, entry := range roles {
				id, role, _ := strings.Cut(entry, "=")
				byID[id] = role
			}
			content := 0
			walkBlocks(d.Blocks, func(b Block) bool {
				switch {
				case b.Type == typeSection || b.Type == typeColumns:
					return true // a container holds blocks; it is the blocks that are written
				case byID[b.ID] == "content":
					content++
				}
				if (b.Type == typeFooter || b.Type == typeDivider || b.ID == "logo") && byID[b.ID] != "brand" {
					t.Errorf("block %q (%s) belongs to the brand, got %q", b.ID, b.Type, byID[b.ID])
				}
				return true
			})
			if content < 2 {
				t.Errorf("a layout needs at least two content blocks to be written, got %d", content)
			}
		})
	}
	confirmation, _ := Layout("builtin:confirmation")
	var confirm *Block
	for i := range confirmation.Blocks {
		if confirmation.Blocks[i].ID == "confirm" {
			confirm = &confirmation.Blocks[i]
		}
	}
	if confirm == nil || confirm.Type != typeButton || confirm.Href != TagConfirmURL {
		t.Fatalf("the confirmation button must link to %s, got %+v", TagConfirmURL, confirm)
	}
}

// TestBuiltinRenderIssuesAreOnlyPlaceholdersAndLinks lists what a person has
// to supply before a built-in layout can be sent: its pictures, and the
// destinations of its buttons and social links. Nothing else is wrong with a
// layout that comes out of the box (it carries its unsubscribe link, its
// pictures are all placeholders and no address is made up).
func TestBuiltinRenderIssuesAreOnlyPlaceholdersAndLinks(t *testing.T) {
	wantIssues := map[string]string{
		"builtin:confirmation": "image_missing:logo image_missing:hero",
		"builtin:letter": "image_missing:logo link_missing:cta image_missing:signature-photo image_missing:signature-name " +
			"image_missing:promo-picture link_missing:social link_missing:social link_missing:social",
		"builtin:article": "image_missing:logo image_missing:banner link_missing:ask image_missing:signature-photo image_missing:signature-name",
		"builtin:spotlight": "image_missing:logo image_missing:spotlight link_missing:cta " +
			"link_missing:social link_missing:social link_missing:social",
	}
	for _, info := range Layouts() {
		t.Run(info.ID, func(t *testing.T) {
			r, err := Render(context.Background(), info.Document, nil, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, issue := range r.Issues {
				if issue.Code != IssueImageMissing && issue.Code != IssueLinkMissing {
					t.Errorf("unexpected issue %+v", issue)
				}
				got = append(got, issue.Code+":"+issue.Block)
			}
			if joined := strings.Join(got, " "); joined != wantIssues[info.ID] {
				t.Errorf("issues:\n got %s\nwant %s", joined, wantIssues[info.ID])
			}
		})
	}
	// The Confirmation is the one that confirms: it links to the confirm tag,
	// so a journey that requires one accepts it as it is.
	confirmation, _ := Layout("builtin:confirmation")
	r, err := Render(context.Background(), confirmation, nil, RenderOptions{RequireConfirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if hasIssue(r.Issues, IssueConfirmMissing) {
		t.Errorf("the confirmation layout must carry the confirm link: %+v", r.Issues)
	}
}

// TestLayoutsGolden renders each built-in layout as it is sent. The goldens are
// written by -update from the renderer, as the per-block ones are, and the diff
// is the review.
func TestLayoutsGolden(t *testing.T) {
	for _, info := range Layouts() {
		name := strings.TrimPrefix(info.ID, "builtin:")
		t.Run(name, func(t *testing.T) {
			r, err := Render(context.Background(), info.Document, nil, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			again, err := Render(context.Background(), info.Document, nil, RenderOptions{})
			if err != nil || again.HTML != r.HTML || again.Text != r.Text {
				t.Fatalf("rendering a layout twice must give the same bytes (err %v)", err)
			}
			checkGolden(t, "layout-"+name+".html", r.HTML)
			checkGolden(t, "layout-"+name+".txt", r.Text)
		})
	}
}

// TestLayoutFilesAreCanonical pins that each embedded file is the bytes
// Document.JSON writes for it: a blank email is filed from a layout, and a
// person who opens it and saves it without a change must not see a diff.
func TestLayoutFilesAreCanonical(t *testing.T) {
	entries, err := layoutFiles.ReadDir("layouts")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(builtinCatalog) {
		t.Fatalf("want %d layout files, got %d", len(builtinCatalog), len(entries))
	}
	for _, want := range builtinCatalog {
		name := "layouts/" + strings.TrimPrefix(want.id, "builtin:") + ".email.json"
		data, err := layoutFiles.ReadFile(name)
		if err != nil {
			t.Errorf("%s: %v", want.id, err)
			continue
		}
		d, err := Parse(data)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if canonical, err := d.JSON(); err != nil || !bytes.Equal(canonical, data) {
			t.Errorf("%s is not in canonical form (write it with Document.JSON; err %v)", name, err)
		}
	}
}
