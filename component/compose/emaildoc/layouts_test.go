package emaildoc

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// builtinCatalog is the catalogue of the plan's Appendix B, in the order
// Layouts lists it: the built-in layouts with the words that name and
// describe them, and the subject and preheader each one carries.
var builtinCatalog = []struct {
	id, name, description, subject, preheader string
}{
	{"builtin:confirmation", "Confirmation", "A welcome on your brand colour with one button that confirms the subscription.", "Please confirm your subscription", "One click and you're on the list."},
	{"builtin:letter", "Letter", "A personal note with a signature band, a promotion and social links.", "A note from our founder", "Why we started, and what comes next."},
	{"builtin:article", "Article", "A banner, a tinted article body with a highlighted statement, and a signature.", "Three small changes for calmer mornings", "Pick one and try it tomorrow."},
	{"builtin:lesson", "Lesson", "A banner, a tinted lesson that ends on its takeaway, a promotion beside a picture, and social links.", "Why small habits stick", "A two-minute lesson for busy weeks."},
	{"builtin:three-reasons", "Three reasons", "A banner, a row of icons, a tinted body of titled reasons with a closing line, and a signature.", "Three choices we made on purpose", "What goes into every Acme tool, and why."},
	{"builtin:side-by-side", "Side by side", "A banner, a tinted introduction, two cards side by side and closing lines, with a signature.", "Two sizes, made for different jobs", "Find the one that fits your home."},
	{"builtin:spotlight", "Spotlight", "One large picture, a short message and one button.", "Meet Ace", "Our guide to getting the small things done."},
	{"builtin:welcome-tour", "Welcome tour", "A banner, a short introduction, one to three benefits with icons, a button and social links.", "Welcome to Acme", "Three things to try first."},
	{"builtin:plain-note", "Plain note", "A greeting and a few paragraphs of plain text, with a signature.", "A quick note from us", "Just a few words."},
	{"builtin:greeting", "Greeting", "A full-width picture, a short message and a signature.", "Warm wishes from all of us", "A small hello."},
	{"builtin:announcement", "Announcement", "A banner, a headline, a short text, one button and social links.", "Something new at Acme", "Here is what is changing."},
	{"builtin:video", "Video", "A headline, a video picture that links to the video, a short text, one button and social links.", "Watch: a short introduction", "Two minutes, start to finish."},
	{"builtin:product-launch", "Product launch", "A product picture, a headline with a short line above it, a list of features, one button and social links.", "Meet the new Acme Mini", "Small, sturdy and ready today."},
	{"builtin:product-grid", "Product grid", "A headline, a short introduction and rows of two products, each with a picture, name, line and button, with social links.", "Our favourites this season", "Four tools worth a look."},
	{"builtin:product-focus", "Product focus", "One product with its picture beside its name, text and button, and a quote below.", "Meet the Acme Max", "Built for the bigger jobs."},
	{"builtin:offer", "Offer", "A headline, a short text, a highlighted code, one button, small print and social links.", "A little something for you", "Use your code at checkout."},
	{"builtin:newsletter", "Newsletter", "A short introduction and one to three stories, each with a picture, title, excerpt and link, with social links.", "This month at Acme", "Three stories worth a read."},
	{"builtin:steps", "Steps", "A headline, a short introduction and two to six numbered steps, each with a title and text, with a button and a reply.", "How to get started", "A few simple steps to begin."},
	{"builtin:picture-steps", "Picture steps", "A headline and two to four rows, each with a picture beside its text.", "Follow along, step by step", "See how it works, one picture at a time."},
	{"builtin:questions", "Questions", "A headline, a short introduction and two to six questions, each with its answer, with a reply.", "Your questions, answered", "The answers you asked for."},
	{"builtin:event", "Event", "A picture, a headline, a tinted box with when and where, a short text and one button.", "You are invited", "Save the date."},
	{"builtin:testimonial", "Testimonial", "A highlighted quote, the person who said it with a photo, one button and social links.", "What our customers say", "A few kind words."},
	{"builtin:review-request", "Review request", "A headline, a product picture, a short text and one button.", "How did we do?", "Tell us what you think."},
	{"builtin:thank-you", "Thank you", "A headline, a short text, a tinted box of tips, a reply and a signature.", "Thank you", "We are glad you are here."},
}

// builtinTheme is the neutral example brand's palette and fonts, which the
// built-in layouts share: a grey scale, so the layouts are structure and a brand
// kit supplies the colour.
var builtinTheme = Theme{
	Page: "#f1f1f1", Canvas: "#ffffff", Primary: "#2f2f2f", Secondary: "#595959", Accent: "#3d3d3d",
	Tint: "#f5f5f5", Text: "#1c1c1c", Muted: "#666666", HeadingFont: "Georgia", BodyFont: "Arial",
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
	data, err := builtinLayoutFS.ReadFile("layouts/" + strings.TrimPrefix(id, "builtin:") + ".email.json")
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
			if d.Theme != builtinTheme {
				t.Errorf("theme: want %+v, got %+v", builtinTheme, d.Theme)
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
	"builtin:confirmation": "panel=content top=brand logo=brand rule-top=brand headline=content subhead=content hero=content confirm=content note=content rule-bottom=brand footer=brand " +
		"panel-end=brand panel-end-space=brand",
	"builtin:letter": "logo=brand letter=content headline=content body=content cta=content " +
		"signature=brand signature-photo=brand signature-space=brand signature-name=brand signature-title=brand " +
		"rule-1=brand promo=content promo-space=content promo-title=content promo-text=content promo-picture=content " +
		"follow=brand social=brand rule-2=brand footer=brand",
	"builtin:article": "logo=brand banner=content body=content lead=content intro=content highlight=content article=content " +
		"closing=content closing-title=content closing-text=content " +
		"reply=brand reply-heading=brand reply-button=brand reply-note=brand " +
		"signature=brand signature-photo=brand signature-space=brand signature-name=brand signature-title=brand " +
		"footer-rule=content rule=brand footer=brand",
	"builtin:lesson": "logo=brand banner=content body=content intro=content article=content highlight=content promo-rule=content rule=brand " +
		"promo=content promo-space=content promo-title=content promo-text=content promo-picture=content " +
		"follow=brand social=brand rule-2=brand footer=brand",
	"builtin:three-reasons": "logo=brand banner=content icons=content icon-1=content icon-2=content icon-3=content " +
		"body=content lead=content reason=content reason-title=content reason-text=content closing=content " +
		"reply=brand reply-heading=brand reply-button=brand reply-note=brand " +
		"signature=brand signature-photo=brand signature-space=brand signature-name=brand signature-title=brand " +
		"rule=brand footer=brand",
	"builtin:side-by-side": "logo=brand banner=content body=content intro=content statement=content " +
		"cards=content card-a-icon=content card-a-title=content card-a-subtitle=content card-a-text=content " +
		"card-b-icon=content card-b-title=content card-b-subtitle=content card-b-text=content " +
		"closing=content closing-title=content closing-text=content " +
		"reply=brand reply-heading=brand reply-button=brand reply-note=brand " +
		"signature=brand signature-photo=brand signature-space=brand signature-name=brand signature-title=brand " +
		"rule=brand footer=brand",
	"builtin:spotlight":    "logo=brand spotlight=content message-box=content message=content cta=content social=brand footer=brand",
	"builtin:welcome-tour": "logo=brand hero=content headline=content intro=content benefit=content benefit-icon=content benefit-text=content cta=content social=brand footer=brand",
	"builtin:plain-note": "logo=brand letter=content greeting=content body=content " +
		"signature=brand signature-photo=brand signature-space=brand signature-name=brand signature-title=brand footer=brand",
	"builtin:greeting": "logo=brand picture=content message=content " +
		"signature=brand signature-photo=brand signature-space=brand signature-name=brand signature-title=brand footer=brand",
	"builtin:announcement":   "logo=brand hero=content headline=content text=content cta=content social=brand footer=brand",
	"builtin:video":          "logo=brand headline=content video=content text=content cta=content social=brand footer=brand",
	"builtin:product-launch": "logo=brand product=content eyebrow=content headline=content features=content cta=content social=brand footer=brand",
	"builtin:product-grid": "logo=brand headline=content intro=content row=content product-a-picture=content product-a-name=content product-a-line=content product-a-button=content " +
		"product-b-picture=content product-b-name=content product-b-line=content product-b-button=content social=brand footer=brand",
	"builtin:product-focus":  "logo=brand focus=content focus-picture=content focus-name=content focus-text=content focus-button=content quote=content footer=brand",
	"builtin:offer":          "logo=brand offer-headline=content offer-text=content code=content cta=content fine-print=content social=brand footer=brand",
	"builtin:newsletter":     "logo=brand intro=content story=content story-picture=content story-title=content story-excerpt=content story-link=content social=brand footer=brand",
	"builtin:steps":          "logo=brand headline=content intro=content step=content step-number=content step-title=content step-text=content cta=content reply=brand reply-heading=brand reply-button=brand reply-note=brand footer=brand",
	"builtin:picture-steps":  "logo=brand headline=content row=content row-picture=content row-text=content footer=brand",
	"builtin:questions":      "logo=brand headline=content intro=content qa=content qa-question=content qa-answer=content reply=brand reply-heading=brand reply-button=brand reply-note=brand footer=brand",
	"builtin:event":          "logo=brand picture=content headline=content details=content details-when=content details-where=content text=content cta=content footer=brand",
	"builtin:testimonial":    "logo=brand quote=content person=content person-photo=content person-name=content cta=content social=brand footer=brand",
	"builtin:review-request": "logo=brand headline=content product=content text=content cta=content footer=brand",
	"builtin:thank-you":      "logo=brand headline=content text=content tips=content tips-title=content tips-text=content reply=brand reply-heading=brand reply-button=brand reply-note=brand signature=brand signature-photo=brand signature-space=brand signature-name=brand signature-title=brand footer=brand",
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
	confirm := blockByID(t, confirmation, "confirm")
	if confirm.Type != typeButton || confirm.Href != TagConfirmURL {
		t.Fatalf("the confirmation button must link to %s, got %+v", TagConfirmURL, confirm)
	}
}

// TestBuiltinRenderIssuesAreOnlyPlaceholdersAndLinks lists what a person has
// to supply before a built-in layout can be sent: its pictures, and the
// destinations of its buttons, social links and the footer's "Contact us".
// Nothing else is wrong with a layout that comes out of the box (it carries its
// unsubscribe link, its pictures are all placeholders and no address is made
// up).
func TestBuiltinRenderIssuesAreOnlyPlaceholdersAndLinks(t *testing.T) {
	wantIssues := map[string]string{
		"builtin:confirmation": "image_missing:logo image_missing:hero link_missing:footer",
		"builtin:letter": "image_missing:logo link_missing:cta image_missing:signature-photo image_missing:signature-name " +
			"image_missing:promo-picture link_missing:social link_missing:social link_missing:social link_missing:footer",
		"builtin:article": "image_missing:logo image_missing:banner link_missing:reply-button image_missing:signature-photo image_missing:signature-name " +
			"link_missing:footer",
		"builtin:lesson": "image_missing:logo image_missing:banner image_missing:promo-picture " +
			"link_missing:social link_missing:social link_missing:social link_missing:footer",
		"builtin:three-reasons": "image_missing:logo image_missing:banner image_missing:icon-1 image_missing:icon-2 image_missing:icon-3 " +
			"link_missing:reply-button image_missing:signature-photo image_missing:signature-name link_missing:footer",
		"builtin:side-by-side": "image_missing:logo image_missing:banner image_missing:card-a-icon image_missing:card-b-icon " +
			"link_missing:reply-button image_missing:signature-photo image_missing:signature-name link_missing:footer",
		"builtin:spotlight": "image_missing:logo image_missing:spotlight link_missing:cta " +
			"link_missing:social link_missing:social link_missing:social link_missing:footer",
		"builtin:welcome-tour": "image_missing:logo image_missing:hero image_missing:benefit-icon link_missing:cta " +
			"link_missing:social link_missing:social link_missing:social link_missing:footer",
		"builtin:plain-note": "image_missing:logo image_missing:signature-photo image_missing:signature-name link_missing:footer",
		"builtin:greeting":   "image_missing:logo image_missing:picture image_missing:signature-photo image_missing:signature-name link_missing:footer",
		"builtin:announcement": "image_missing:logo image_missing:hero link_missing:cta " +
			"link_missing:social link_missing:social link_missing:social link_missing:footer",
		"builtin:video": "image_missing:logo image_missing:video link_missing:cta " +
			"link_missing:social link_missing:social link_missing:social link_missing:footer",
		"builtin:product-launch": "image_missing:logo image_missing:product link_missing:cta " +
			"link_missing:social link_missing:social link_missing:social link_missing:footer",
		"builtin:product-grid": "image_missing:logo image_missing:product-a-picture link_missing:product-a-button image_missing:product-b-picture link_missing:product-b-button " +
			"link_missing:social link_missing:social link_missing:social link_missing:footer",
		"builtin:product-focus": "image_missing:logo image_missing:focus-picture link_missing:focus-button link_missing:footer",
		"builtin:offer": "image_missing:logo link_missing:cta " +
			"link_missing:social link_missing:social link_missing:social link_missing:footer",
		"builtin:newsletter": "image_missing:logo image_missing:story-picture link_missing:story-link " +
			"link_missing:social link_missing:social link_missing:social link_missing:footer",
		"builtin:steps":          "image_missing:logo link_missing:cta link_missing:reply-button link_missing:footer",
		"builtin:picture-steps":  "image_missing:logo image_missing:row-picture link_missing:footer",
		"builtin:questions":      "image_missing:logo link_missing:reply-button link_missing:footer",
		"builtin:event":          "image_missing:logo image_missing:picture link_missing:cta link_missing:footer",
		"builtin:testimonial":    "image_missing:logo image_missing:person-photo link_missing:cta link_missing:social link_missing:social link_missing:social link_missing:footer",
		"builtin:review-request": "image_missing:logo image_missing:product link_missing:cta link_missing:footer",
		"builtin:thank-you":      "image_missing:logo link_missing:reply-button image_missing:signature-photo image_missing:signature-name link_missing:footer",
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
				// The footer's issue names its link: "Contact us" is the one
				// without an address.
				if issue.Block == "footer" && issue.Message != `Add a link to "Contact us".` {
					t.Errorf("the footer's issue should name its link: %+v", issue)
				}
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

// A highlight is written one row to a line of the statement, and each line is a
// span on the highlight colour. A span's background fills the font's whole
// content area (about 1.15 times its size in the fonts of these layouts, and up
// to 1.2 in others a reader may have), so a line height under that makes lines
// that sit close draw over each other. A statement that wraps on a phone does:
// the Article's highlight had a line height of half its size, and at 375 pixels
// "before." was written over "decision". A built-in layout's highlight keeps
// the room its lines need, whether they are rows of their own or the lines of
// one that wraps. (A highlight that names no line height gets 1.4 times its
// size from the renderer; TestHighlightDefaultLineHeightLeavesRoomForItsLines.)
func TestBuiltinHighlightsLeaveRoomForTheirLines(t *testing.T) {
	highlights := 0
	for _, info := range Layouts() {
		walkBlocks(info.Document.Blocks, func(b Block) bool {
			if b.Type != typeHighlight {
				return true
			}
			highlights++
			size := highlightSize(b)
			if lineHeight := b.LineHeight; lineHeight < 1.2*size {
				t.Errorf("%s: highlight %q has a line height of %gpx for text of %gpx, under 1.2 times its size (%.2fpx): its lines draw over each other when they wrap",
					info.ID, b.ID, lineHeight, size, 1.2*size)
			}
			return true
		})
	}
	if highlights == 0 {
		t.Fatal("no built-in layout has a highlight: the test checks nothing")
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
	entries, err := builtinLayoutFS.ReadDir("layouts")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(builtinCatalog) {
		t.Fatalf("want %d layout files, got %d", len(builtinCatalog), len(entries))
	}
	for _, want := range builtinCatalog {
		name := "layouts/" + strings.TrimPrefix(want.id, "builtin:") + ".email.json"
		data, err := builtinLayoutFS.ReadFile(name)
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

// requireContentBlock fails unless the layout holds a block with the id whose
// role is content.
func requireContentBlock(t *testing.T, l LayoutInfo, id string) {
	t.Helper()
	found := false
	eachBlock(l.Document.Blocks, roleContent, func(b *Block, role string) {
		if b.ID == id && role == roleContent {
			found = true
		}
	})
	if !found {
		t.Errorf("%s: %s is not a content block", l.ID, id)
	}
}

func TestLayoutCatalogueMetadata(t *testing.T) {
	for _, l := range Layouts() {
		if !slices.Contains(LayoutCategories, l.Category) {
			t.Errorf("%s: category %q", l.ID, l.Category)
		}
		for _, id := range l.Optional {
			requireContentBlock(t, l, id)
		}
		for id, r := range l.Repeat {
			requireContentBlock(t, l, id)
			if r.Min < 1 || r.Max < r.Min || r.Max > 6 {
				t.Errorf("%s: %s range %+v", l.ID, id, r)
			}
		}
		eachBlock(l.Document.Blocks, "", func(b *Block, role string) {
			if slices.Contains(BrandSlots(), b.ID) && role != "brand" {
				t.Errorf("%s: slot %s is %s", l.ID, b.ID, role)
			}
		})
	}
}

func TestLayoutMetaReturnsACopy(t *testing.T) {
	l, ok := LayoutMeta("builtin:article")
	if !ok || l.Category != "Articles" || !slices.Equal(l.Optional, []string{"lead", "highlight", "closing", "footer-rule"}) {
		t.Fatalf("article meta = %+v, %v", l, ok)
	}
	l.Optional[0] = "changed"
	if again, _ := LayoutMeta("builtin:article"); again.Optional[0] != "lead" {
		t.Error("LayoutMeta shares its slices with the catalogue")
	}
	if _, ok := LayoutMeta("builtin:nope"); ok {
		t.Error("unknown id found")
	}
}

func TestBrandSlotsOrder(t *testing.T) {
	want := []string{"logo", "signature", "follow", "social", "footer", "reply"}
	if got := BrandSlots(); !slices.Equal(got, want) {
		t.Errorf("BrandSlots = %v", got)
	}
}

// TestReplySlotIsTheSameInEveryLayout pins that the reply section, a brand
// slot, is drawn alike wherever a built-in layout has one, at the measurements
// of the series the layouts were taken from: a kit's reply should look the same
// in every email of an account.
func TestReplySlotIsTheSameInEveryLayout(t *testing.T) {
	var first []byte
	var firstID string
	count := 0
	for _, info := range Layouts() {
		b := findBlock(info.Document.Blocks, SlotReply)
		if b == nil {
			continue
		}
		count++
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first, firstID = data, info.ID
			continue
		}
		if !bytes.Equal(data, first) {
			t.Errorf("%s draws its reply unlike %s:\n%s\n%s", info.ID, firstID, data, first)
		}
	}
	if count < 3 {
		t.Fatalf("want a reply in Article, Three reasons and Side by side, got %d layouts with one", count)
	}
}
