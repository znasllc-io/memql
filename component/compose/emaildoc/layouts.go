package emaildoc

import (
	"embed"
	"maps"
	"slices"
	"strings"
	"sync"
)

// builtinLayoutFS holds the documents of the built-in layouts, one
// layouts/<name>.email.json each.
//
//go:embed layouts/*.email.json
var builtinLayoutFS embed.FS

// LayoutInfo is a built-in layout as it is listed: the id it is chosen by, the
// name and description a picker shows, and the document itself. Layouts says
// what the documents hold.
type LayoutInfo struct {
	ID          string // "builtin:confirmation"
	Name        string // "Confirmation"
	Description string
	Document    Document // Layout field equals ID

	// Category is the group a picker files the layout under: one of LayoutCategories.
	Category string
	// Optional lists the ids of content blocks a person may leave out.
	Optional []string
	// Repeat maps the id of a content block to how many times it may appear.
	Repeat map[string]RepeatRange
}

// RepeatRange is how many copies of a repeatable content block a layout allows.
type RepeatRange struct{ Min, Max int }

// LayoutCategories are the groups layouts are filed under, in the order a picker shows them.
var LayoutCategories = []string{"Welcome", "Letters", "Articles", "Announcements", "Products", "Promotions", "News", "Guides", "Events", "Feedback"}

// The ids of the brand blocks every layout keeps for the organization's brand kit.
const (
	SlotLogo      = "logo"
	SlotSignature = "signature"
	SlotFollow    = "follow"
	SlotSocial    = "social"
	SlotFooter    = "footer"
	SlotReply     = "reply"
)

// BrandSlots lists the brand slot ids: logo, signature, follow, social, footer and reply.
func BrandSlots() []string {
	return []string{SlotLogo, SlotSignature, SlotFollow, SlotSocial, SlotFooter, SlotReply}
}

// builtinLayouts is the catalogue, in the order Layouts lists it. An id is
// "builtin:" and the name of the file in layouts/ that holds the document.
var builtinLayouts = [...]struct {
	id, name, description, category string
	optional                        []string
	repeat                          map[string]RepeatRange
}{
	{"builtin:confirmation", "Confirmation", "A welcome on your brand colour with one button that confirms the subscription.", "Welcome", nil, nil},
	{"builtin:letter", "Letter", "A personal note with a signature band, a promotion and social links.", "Letters", nil, nil},
	{"builtin:article", "Article", "A banner, a tinted article body with a highlighted statement, and a signature.", "Articles",
		[]string{"lead", "highlight", "closing", "footer-rule"}, nil},
	{"builtin:lesson", "Lesson", "A banner, a tinted lesson that ends on its takeaway, a promotion beside a picture, and social links.", "Articles", []string{"highlight", "promo-rule"}, nil},
	{"builtin:three-reasons", "Three reasons", "A banner, a row of icons, a tinted body of titled reasons with a closing line, and a signature.", "Articles",
		[]string{"icons", "closing"}, map[string]RepeatRange{"reason": {Min: 2, Max: 4}}},
	{"builtin:side-by-side", "Side by side", "A banner, a tinted introduction, two cards side by side and closing lines, with a signature.", "Products", []string{"closing"}, nil},
	{"builtin:spotlight", "Spotlight", "One large picture, a short message and one button.", "Announcements", nil, nil},
}

// Layouts lists the built-in layouts in the order of the catalogue:
// Confirmation, Letter, Article, Lesson, Three reasons, Side by side and
// Spotlight.
//
// Each is a ready-made layout-mode document in a neutral example brand
// ("Acme", Georgia and Arial, a small blue and grey palette with a red
// accent), and carries no one's copy, pictures or logos. A person starts from
// one, or the Materializer fills one; an organization that wants its own look
// saves a finished email as a layout of its own.
//
// What a layout leaves to whoever uses it is what Render reports as issues.
// Every picture is a placeholder (image_missing), and every address is empty
// except the link merge tags: a made-up one would be sent as it is, with
// nothing to say it was never replaced, so a button, a social link or a
// footer link without one is reported (link_missing) until someone gives it a
// destination. The words are an example to write over.
//
// Each block has a role. Brand blocks are the frame an organization keeps the
// same from one email to the next (the logo, dividers, signature, follow links
// and footer, for example) and keep their content when a layout is filled;
// content blocks are what gets written. A block without a role of its own
// inside a section or columns takes the container's.
//
// The colours are readable as they come: every pair of text and background
// colour a layout draws has a contrast ratio of at least 4.5 to 1, or 3 to 1
// for large text (24 pixels, or 18.66 pixels and bold), the WCAG 2 level AA. A
// test of this package checks every pair, so a layout that is changed has to
// keep it.
//
// Each call returns documents of its own, which the caller may change.
func Layouts() []LayoutInfo {
	verifyLayouts()
	list := make([]LayoutInfo, len(builtinLayouts))
	for i := range builtinLayouts {
		list[i] = layoutInfo(i, true)
	}
	return list
}

// LayoutMeta returns the catalogue entry of the built-in layout with the id,
// without its document, as a copy the caller may change. ok is false for any
// other id.
func LayoutMeta(id string) (LayoutInfo, bool) {
	for i, entry := range builtinLayouts {
		if entry.id == id {
			return layoutInfo(i, false), true
		}
	}
	return LayoutInfo{}, false
}

func layoutInfo(i int, withDocument bool) LayoutInfo {
	entry := builtinLayouts[i]
	info := LayoutInfo{ID: entry.id, Name: entry.name, Description: entry.description, Category: entry.category,
		Optional: slices.Clone(entry.optional), Repeat: maps.Clone(entry.repeat)}
	if withDocument {
		info.Document = layoutDocument(entry.id)
	}
	return info
}

// Layout returns the built-in layout with the id "builtin:<name>", as a
// document of its own: a caller may fill it in (the Materializer writes its
// content into the blocks) without changing the layout for the next caller.
// ok is false for any other id. Layouts says what the layouts hold.
func Layout(id string) (Document, bool) {
	verifyLayouts()
	for _, entry := range builtinLayouts {
		if entry.id == id {
			return layoutDocument(id), true
		}
	}
	return Document{}, false
}

var layoutsVerified sync.Once

// verifyLayouts reads every embedded layout once, on first use, and panics if
// one is not a document Parse admits or does not name itself: a broken layout
// is a defect of the build that the tests of this package catch, not a
// condition for a caller to handle.
func verifyLayouts() {
	layoutsVerified.Do(func() {
		for _, entry := range builtinLayouts {
			layoutDocument(entry.id)
		}
	})
}

// layoutDocument reads one embedded layout. It parses the file again for each
// caller instead of keeping a Document to hand out, because a Document holds
// pointers and slices that a caller who changes the one it was given would
// change for everyone after it.
func layoutDocument(id string) Document {
	file := "layouts/" + strings.TrimPrefix(id, "builtin:") + ".email.json"
	data, err := builtinLayoutFS.ReadFile(file)
	if err != nil {
		panic("emaildoc: the built-in layout " + id + " is not embedded: " + err.Error())
	}
	d, err := Parse(data)
	if err != nil {
		panic("emaildoc: the built-in layout " + id + " is not a valid email document: " + err.Error())
	}
	if d.Mode() != ModeLayout || d.Layout != id {
		panic("emaildoc: " + file + " must be a layout that names itself " + id)
	}
	return d
}
